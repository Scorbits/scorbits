package explorer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"scorbits/auth"
	"scorbits/blockchain"
	"scorbits/db"
	"scorbits/email"
	"scorbits/i18n"
	"scorbits/mempool"
	"scorbits/network"
	"scorbits/transaction"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crypto/sha256"

	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/v2/bson"
	xdraw "golang.org/x/image/draw"
)

// ─── RATE LIMITER ─────────────────────────────────────────────────────────────

type rateBucket struct {
	count   int
	resetAt time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
	max     int
	window  time.Duration
}

func newRateLimiter(max int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{buckets: make(map[string]*rateBucket), max: max, window: window}
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.mu.Lock()
			now := time.Now()
			for k, b := range rl.buckets {
				if now.After(b.resetAt) {
					delete(rl.buckets, k)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok || now.After(b.resetAt) {
		rl.buckets[key] = &rateBucket{count: 1, resetAt: now.Add(rl.window)}
		return true
	}
	b.count++
	return b.count <= rl.max
}

// ─── NEWS CACHE ───────────────────────────────────────────────────────────────

type newsCacheEntry struct {
	data     []byte
	cachedAt time.Time
}

var adminUsernames = map[string]bool{
	"Yousse": true,
}

var (
	newsCacheMu sync.RWMutex
	newsCache   newsCacheEntry
)

// ─── CHAT SPAM TRACKER ────────────────────────────────────────────────────────

// ─── EXPLORER ─────────────────────────────────────────────────────────────────

type Explorer struct {
	bc      *blockchain.Blockchain
	mp      *mempool.Mempool
	port    string
	node    *network.Node
	chatHub *ChatHub

	mineMu              sync.Mutex // protège bc.Blocks contre les soumissions concurrentes
	lastBlockAcceptedAt int64      // timestamp réel serveur du dernier bloc accepté

	minerMu         sync.Mutex       // protège minerLastSubmit
	minerLastSubmit map[string]int64 // adresse -> timestamp dernière soumission acceptée

	activeMinersMu sync.Mutex
	activeMiners   map[string]int64 // adresse mineur ou IP -> timestamp dernier /mining/work

	lastAdjustTime  int64 // timestamp serveur du dernier ajustement de difficulté
	lastAdjustBlock int   // index (len) de la chaîne au dernier ajustement

	rlLogin    *RateLimiter
	rlRegister *RateLimiter
	rlSend     *RateLimiter
	rlMine     *RateLimiter
}

func NewExplorer(bc *blockchain.Blockchain, mp *mempool.Mempool, port string) *Explorer {
	nodePort := os.Getenv("NODE_PORT")
	if nodePort == "" {
		nodePort = "3000"
	}
	return &Explorer{
		bc:                  bc,
		mp:                  mp,
		port:                port,
		node:                network.NewNode(nodePort, bc),
		chatHub:             NewChatHub(),
		rlLogin:             newRateLimiter(5, time.Minute),
		rlRegister:          newRateLimiter(3, 10*time.Minute),
		rlSend:              newRateLimiter(10, time.Minute),
		rlMine:              newRateLimiter(60, time.Minute),
		lastBlockAcceptedAt: time.Now().Unix() - 120,
		minerLastSubmit:     make(map[string]int64),
		activeMiners:        make(map[string]int64),
		lastAdjustTime:      time.Now().Unix(),
		lastAdjustBlock:     len(bc.Blocks),
	}
}

func (e *Explorer) Start() error {
	if err := i18n.Load("./static/i18n"); err != nil {
		fmt.Println("[i18n] Load error:", err)
	}

	// Nœud P2P : reçoit les NEW_BLOCK du CLI et met à jour bc en mémoire
	go func() {
		if err := e.node.Start(); err != nil {
			fmt.Printf("[Node] Erreur démarrage nœud P2P: %v\n", err)
		}
	}()

	// Chat hub
	go e.chatHub.Run()
	go e.startChatCleanup()

	// Nettoyage des mineurs actifs toutes les 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			cutoff := time.Now().Unix() - 300
			e.activeMinersMu.Lock()
			for k, ts := range e.activeMiners {
				if ts < cutoff {
					delete(e.activeMiners, k)
				}
			}
			e.activeMinersMu.Unlock()
		}
	}()

	// SCO order payment checker
	go e.startScoOrderChecker()
	db.DeleteOldChatMessages()

	// Create uploads directories
	os.MkdirAll("./static/uploads/profiles", 0755)
	os.MkdirAll("./static/uploads/posts", 0755)
	os.MkdirAll("./static/uploads/announcements", 0755)
	os.MkdirAll("./static/uploads/chat", 0755)

	mux := http.NewServeMux()

	// Explorer public
	mux.HandleFunc("/", e.handleHome)
	mux.HandleFunc("/block/", e.handleBlock)
	mux.HandleFunc("/address/", e.handleAddress)
	mux.HandleFunc("/search", e.handleSearch)

	// Auth pages
	mux.HandleFunc("/wallet", e.handleWalletLogin)
	mux.HandleFunc("/wallet/register", e.handleRegister)
	mux.HandleFunc("/wallet/logout", e.handleLogout)
	mux.HandleFunc("/verify", e.handleVerify)

	// Wallet pages
	mux.HandleFunc("/wallet/dashboard", e.handleDashboard)
	mux.HandleFunc("/wallet/send", e.handleSend)
	mux.HandleFunc("/wallet/receive", e.handleReceive)
	mux.HandleFunc("/wallet/history", e.handleHistory)
	mux.HandleFunc("/wallet/profile", e.handleProfile)

	// Profile page (top-level)
	mux.HandleFunc("/profile", e.handleProfile)

	// Mine
	mux.HandleFunc("/mine", e.handleMine)
	mux.HandleFunc("/download/cli-windows", e.handleDownloadCLIWindows)

	// API publique
	mux.HandleFunc("/api/stats", e.apiStats)
	mux.HandleFunc("/api/blocks", e.apiBlocks)
	mux.HandleFunc("/api/block/", e.apiBlock)
	mux.HandleFunc("/api/address/", e.apiAddress)
	mux.HandleFunc("/api/history/", e.apiHistory)
	mux.HandleFunc("/api/news", e.apiNews)
	mux.HandleFunc("/api/news/react", e.apiNewsReact)
	mux.HandleFunc("/api/posts", e.apiPosts)
	mux.HandleFunc("/api/posts/react", e.apiPostsReact)
	mux.HandleFunc("/api/posts/comments", e.apiPostsComments)
	mux.HandleFunc("/api/notifications", e.apiNotifications)
	mux.HandleFunc("/api/notifications/poll", e.apiNotificationsPoll)
	mux.HandleFunc("/api/notifications/read", e.apiNotificationsReadAll)
	mux.HandleFunc("/api/notifications/read/", e.apiNotificationsReadOne)
	mux.HandleFunc("/api/leaderboard", e.apiLeaderboard)
	mux.HandleFunc("/api/profile/", e.apiProfile)
	mux.HandleFunc("/api/user/profile/", e.apiProfile)
	mux.HandleFunc("/api/activity", e.apiActivity)
	mux.HandleFunc("/api/active-miners", e.apiActiveMiners)

	// Profile API (authenticated)
	mux.HandleFunc("/api/profile/upload-pic", e.apiProfileUploadPic)
	mux.HandleFunc("/api/profile/delete-pic", e.apiProfileDeletePic)
	mux.HandleFunc("/api/profile/change-password", e.apiProfileChangePassword)
	mux.HandleFunc("/api/profile/blocked-users", e.apiProfileBlockedUsers)

	// API auth
	mux.HandleFunc("/api/auth/register", e.apiRegister)
	mux.HandleFunc("/api/auth/login", e.apiLogin)
	mux.HandleFunc("/api/auth/check-email", e.apiCheckEmail)

	// API wallet
	mux.HandleFunc("/api/wallet/send", e.apiWalletSend)
	mux.HandleFunc("/api/wallet/balance", e.apiWalletBalance)
	mux.HandleFunc("/api/wallet/avatar", e.apiUpdateAvatar)

	// Chat
	mux.HandleFunc("/ws/chat", e.wsChat)
	mux.HandleFunc("/api/chat", e.apiChat)
	mux.HandleFunc("/api/chat/reaction", e.apiChatReaction)
	mux.HandleFunc("/api/chat/block", e.apiChatBlock)
	mux.HandleFunc("/api/chat/unblock", e.apiChatUnblock)
	mux.HandleFunc("/api/chat/mute", e.apiChatMute)
	mux.HandleFunc("/api/chat/gif", e.apiGifSearch)

	// DM (private messaging)
	mux.HandleFunc("/api/dm/conversations", e.apiDMConversations)
	mux.HandleFunc("/api/dm/messages", e.apiDMMessages)
	mux.HandleFunc("/api/dm/send", e.apiDMSend)
	mux.HandleFunc("/api/dm/read", e.apiDMRead)
	mux.HandleFunc("/api/dm/message", e.apiDMDelete)
	mux.HandleFunc("/api/dm/unread", e.apiDMUnread)
	mux.HandleFunc("/api/dm/settings", e.apiDMSettings)
	mux.HandleFunc("/api/users/search", e.apiUsersSearch)

	// DM Groups
	mux.HandleFunc("/api/dm/group/create", e.apiDMGroupCreate)
	mux.HandleFunc("/api/dm/group/list", e.apiDMGroupList)
	mux.HandleFunc("/api/dm/group/messages", e.apiDMGroupMessages)
	mux.HandleFunc("/api/dm/group/send", e.apiDMGroupSend)
	mux.HandleFunc("/api/dm/group/read", e.apiDMGroupRead)
	mux.HandleFunc("/api/dm/group/add-member", e.apiDMGroupAddMember)
	mux.HandleFunc("/api/dm/group/leave", e.apiDMGroupLeave)
	mux.HandleFunc("/api/dm/group/rename", e.apiDMGroupRename)

	// Mine (WASM browser miner)
	mux.HandleFunc("/api/mine/job", e.apiMineJob)
	mux.HandleFunc("/api/mine/submit", e.apiMineSubmit)

	// Mine (external binary miner)
	mux.HandleFunc("/mining/work", e.apiMiningWork)
	mux.HandleFunc("/mining/submit", e.apiMiningSubmit)

	// SCO Purchase
	mux.HandleFunc("/api/sco/price", e.apiScoPrice)
	mux.HandleFunc("/api/sco/order", e.apiScoOrder)
	mux.HandleFunc("/api/sco/order/", e.apiScoOrderStatus)
	mux.HandleFunc("/api/sco/cancel", e.apiScoOrderCancel)
	mux.HandleFunc("/api/sco/test-payment/", e.apiScoTestPayment)

	// P2P peers API
	mux.HandleFunc("/api/admin/post/delete", e.apiAdminPostDelete)
	mux.HandleFunc("/api/admin/comment/delete", e.apiAdminCommentDelete)
	mux.HandleFunc("/api/comment/like", e.apiCommentLike)
	mux.HandleFunc("/api/comment/reply", e.apiCommentReply)
	mux.HandleFunc("/api/admin/chat/delete", e.apiAdminChatDelete)
	mux.HandleFunc("/api/admin/set-difficulty", e.apiAdminSetDifficulty)
	mux.HandleFunc("/api/admin/unforce-difficulty", e.apiAdminUnforceDifficulty)
	mux.HandleFunc("/api/announcements", e.apiAnnouncements)
	mux.HandleFunc("/api/announcements/upload", e.apiAnnouncementUpload)
	mux.HandleFunc("/api/announcements/", e.apiAnnouncementByID)
	mux.HandleFunc("/api/chat/upload-image", e.apiChatUploadImage)
	mux.HandleFunc("/api/block-template", e.apiBlockTemplate)
	mux.HandleFunc("/api/submit-block", e.apiSubmitBlock)
	mux.HandleFunc("/api/pool-payout", e.apiPoolPayout)

	mux.HandleFunc("/api/peers", e.apiPeers)
	mux.HandleFunc("/api/peers/add", e.apiPeersAdd)
	mux.HandleFunc("/api/peers/status", e.apiPeersStatus)

	// Pool page
	mux.HandleFunc("/pool", e.handlePool)
	// Pool proxy (CORS-safe relay to localhost:3334)
	mux.HandleFunc("/api/proxy/pool/stats", e.apiProxyPoolStats)
	mux.HandleFunc("/api/proxy/pool/workers", e.apiProxyPoolWorkers)
	mux.HandleFunc("/api/proxy/pool/blocks", e.apiProxyPoolBlocks)
	mux.HandleFunc("/api/proxy/pool/balance", e.apiProxyPoolBalance)

	// Node page
	mux.HandleFunc("/node", e.handleNode)

	// Wallets page
	mux.HandleFunc("/wallets", e.handleWallets)
	mux.HandleFunc("/transactions", e.handleTransactions)
	mux.HandleFunc("/api/transactions", e.apiTransactions)

	// i18n
	mux.HandleFunc("/set-lang", i18n.HandleSetLang)

	// Fichiers statiques
	os.MkdirAll("./dist", 0755)
	os.MkdirAll("./static/nodes", 0755)
	mux.Handle("/static/nodes/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment")
		http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))).ServeHTTP(w, r)
	}))
	mux.Handle("/releases/", http.StripPrefix("/releases/", http.FileServer(http.Dir("./releases"))))
	mux.Handle("/static/miners/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", "attachment")
		}
		http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))).ServeHTTP(w, r)
	}))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))).ServeHTTP(w, r)
	}))
	mux.Handle("/dist/", http.StripPrefix("/dist/", http.FileServer(http.Dir("./dist"))))

	server := &http.Server{
		Addr:         ":" + e.port,
		Handler:      e.securityHeaders(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	fmt.Printf("[Explorer] Démarré sur http://localhost:%s\n", e.port)
	return server.ListenAndServe()
}

// securityHeaders ajoute les headers HTTP de sécurité sur toutes les réponses
func (e *Explorer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	return r.RemoteAddr
}

// ─── EXPLORER PUBLIC ──────────────────────────────────────────────────────────

func (e *Explorer) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	last := e.bc.GetLastBlock()
	blocks := e.bc.Blocks
	recent := blocks
	if len(recent) > 10 {
		recent = blocks[len(blocks)-10:]
	}
	rows := ""
	for i := len(recent) - 1; i >= 0; i-- {
		b := recent[i]
		minerLabel := b.MinerAddress
		if len(minerLabel) > 18 {
			minerLabel = minerLabel[:8] + "..." + minerLabel[len(minerLabel)-6:]
		}
		minerLink := `<span class="block-miner">` + minerLabel + `</span>`
		u, err := db.GetUserByAddress(b.MinerAddress)
		if err == nil {
			minerLink = fmt.Sprintf(`<span class="block-miner"><a href="/address/%s">@%s</a></span>`, b.MinerAddress, u.Pseudo)
		}
		elapsed := time.Since(time.Unix(b.Timestamp, 0))
		var tsLabel string
		if elapsed < time.Minute {
			tsLabel = fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
		} else if elapsed < time.Hour {
			tsLabel = fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
		} else {
			tsLabel = time.Unix(b.Timestamp, 0).Format("02/01 15:04")
		}
		hashShort := b.Hash
		if len(hashShort) > 20 {
			hashShort = hashShort[:12] + "..." + hashShort[len(hashShort)-6:]
		}
		rows += fmt.Sprintf(`<div class="block-row" onclick="window.location='/block/%d'">
			<span class="block-num">#%d</span>
			<span class="block-hash">%s</span>
			%s
			<span class="block-ts">%s</span>
			<span class="block-reward">+%d SCO</span>
		</div>`, b.Index, b.Index, hashShort, minerLink, tsLabel, b.Reward)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "nav.explorer"), homeContent(last, len(e.bc.Blocks), e.bc.TotalSupply, rows, lang, user), "explorer", lang, user))
}

func homeContent(last *blockchain.Block, totalBlocks, totalSupply int, rows string, lang string, user *db.User) string {
	return fmt.Sprintf(`
<div class="home-layout">

  <!-- COLONNE GAUCHE : stats + activité + blocs + leaderboard -->
  <div class="col-left">
    <div class="panel-title">
      <svg width="14" height="14" viewBox="0 0 14 14"><circle cx="7" cy="7" r="6" fill="none" stroke="currentColor" stroke-width="1.5"/><circle cx="7" cy="7" r="2.5" fill="currentColor"/></svg>
      `+i18n.T(lang, "home.network")+` <span class="blink-dot"></span>
    </div>
    <div class="ns-grid">
      <div class="ns-card"><div class="ns-card-label">⬡ `+i18n.T(lang, "home.blocks")+`</div><div class="ns-card-val" id="live-blocks">%d</div></div>
      <div class="ns-card"><div class="ns-card-label">◎ `+i18n.T(lang, "home.supply")+`</div><div class="ns-card-val">%d <span style="font-size:0.62rem;color:#4a6a4a">SCO</span></div></div>
      <div class="ns-card"><div class="ns-card-label">◈ `+i18n.T(lang, "home.max_supply")+`</div><div class="ns-card-val" style="font-size:0.82rem;color:#7ab98a">99 000 000</div></div>
      <div class="ns-card"><div class="ns-card-label">⚡ `+i18n.T(lang, "home.difficulty")+`</div><div class="ns-card-val">%d</div></div>
      <div class="ns-card"><div class="ns-card-label">⟁ `+i18n.T(lang, "home.tx_fee")+`</div><div class="ns-card-val">0.1%%</div><div style="font-size:11px;color:#4a6a4a;margin-top:2px">`+i18n.T(lang, "home.tx_fee_sub")+`</div></div>
      <div class="ns-card"><div class="ns-card-label">⏱ `+i18n.T(lang, "home.last_block")+`</div><div class="ns-card-val" style="font-size:0.7rem;color:#7ab98a;">%s</div></div>
      <div class="ns-card" style="grid-column:span 2"><div class="ns-card-label">⛏ `+i18n.T(lang, "home.active_miners")+`</div><div class="ns-card-val" id="home-active-miners">—</div></div>
    </div>

    <div style="text-align:center;margin:1rem 0 0.5rem;">
      <div style="display:flex;gap:0.6rem;justify-content:center;flex-wrap:wrap;">
        <a href="/wallet#buy" style="display:inline-block;background:#f5a623;color:#000;font-weight:700;padding:12px 32px;border-radius:8px;text-decoration:none;font-size:1rem;letter-spacing:0.02em;">Buy SCO</a>
        <a href="/transactions" style="display:inline-block;background:#f5a623;color:#000;font-weight:700;padding:12px 32px;border-radius:8px;text-decoration:none;font-size:1rem;letter-spacing:0.02em;">Transactions</a>
      </div>
    </div>

    <div class="panel-title" style="margin-top:1.2rem">
      <svg width="14" height="14" viewBox="0 0 14 14"><rect x="1" y="1" width="12" height="4" rx="1" fill="none" stroke="currentColor" stroke-width="1.5"/><rect x="1" y="9" width="12" height="4" rx="1" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>
      `+i18n.T(lang, "home.current_block")+`
    </div>
    <div class="block-progress-wrap">
      <div style="display:flex;justify-content:space-between;margin-bottom:0.5rem;">
        <span style="font-size:0.78rem;color:var(--text2)">`+i18n.T(lang, "home.next")+`&#160;: <span id="bp-next" style="color:var(--green);font-weight:700">#%d</span></span>
        <span style="font-size:0.78rem;color:var(--text2)">`+i18n.T(lang, "home.diff")+`&#160;: <span id="bp-diff" style="color:var(--green);font-weight:700">%d</span></span>
      </div>
      <span id="bp-elapsed" class="bp-elapsed"></span>
    </div>

    <div class="panel-title" style="margin-top:1.2rem">
      <svg width="14" height="14" viewBox="0 0 14 14"><rect x="1" y="1" width="12" height="12" rx="2" fill="none" stroke="currentColor" stroke-width="1.5"/><rect x="3" y="5" width="2" height="6" fill="currentColor" rx="1"/><rect x="6" y="3" width="2" height="8" fill="currentColor" rx="1"/><rect x="9" y="6" width="2" height="5" fill="currentColor" rx="1"/></svg>
      `+i18n.T(lang, "home.latest_blocks")+`
    </div>
    <div class="block-list" id="block-list">%s</div>

    <div style="margin-top:.9rem;padding:.65rem 1rem;background:#0d1f35;border:1px solid #1a3a5c;border-radius:8px;display:flex;align-items:center;justify-content:space-between;gap:.5rem;">
      <span style="font-size:.8rem;color:#aac;">`+func() string { if lang == "fr" { return "Minez en commun avec le" }; return "Mine together with" }()+` <strong style="color:#00ff88;">Scorbits Pool</strong></span>
      <a href="/pool" style="font-size:.75rem;padding:.3rem .7rem;background:#00ff88;color:#000;border-radius:5px;font-weight:700;text-decoration:none;white-space:nowrap;">`+func() string { if lang == "fr" { return "Rejoindre" }; return "Join" }()+`</a>
    </div>

    <div class="panel-title" style="margin-top:1.2rem">
      <svg width="14" height="14" viewBox="0 0 14 14"><polygon points="7,1 9,5 13,5.5 10,8.5 10.5,13 7,11 3.5,13 4,8.5 1,5.5 5,5" fill="currentColor"/></svg>
      `+i18n.T(lang, "home.leaderboard")+`
    </div>
    <div id="leaderboard" class="leaderboard-widget">
      <div class="af-loading">`+i18n.T(lang, "common.loading")+`</div>
    </div>

    <div class="panel-title" style="margin-top:1.2rem">
      <svg width="14" height="14" viewBox="0 0 14 14"><rect x="1" y="6" width="2" height="7" fill="currentColor" rx="1"/><rect x="4.5" y="3" width="2" height="10" fill="currentColor" rx="1"/><rect x="8" y="1" width="2" height="12" fill="currentColor" rx="1"/><rect x="11.5" y="4" width="2" height="9" fill="currentColor" rx="1"/></svg>
      `+i18n.T(lang, "home.activity")+`
    </div>
    <div id="activity-feed" class="activity-feed">
      <div class="af-loading">`+i18n.T(lang, "common.loading")+`</div>
    </div>
  </div>

  <!-- COLONNE MILIEU : chat global + feed communauté -->
  <div class="col-mid">
    <div class="chat-panel">
      <div class="chat-header">
        <span class="chat-title">💬 Global Chat</span>
        <span class="chat-online" id="chat-online">● 0 online</span>
      </div>
      <div class="chat-messages" id="chat-messages"></div>
      <div class="chat-input-area" id="chat-input-area">
        <div class="chat-mentions-list hidden" id="chat-mentions"></div>
        <div class="chat-share-preview hidden" id="chat-share-preview"></div>
        <div class="chat-media-preview hidden" id="chat-media-preview">
          <button class="chat-media-cancel" onclick="cancelChatMedia()" title="Annuler">&#10005;</button>
          <img id="chat-media-preview-img" src="" alt="" class="chat-media-preview-img">
        </div>
        <div class="chat-toolbar">
          <button class="chat-emoji-btn" onclick="toggleEmojiPicker(event)" title="Emoji">😊</button>
          <button class="chat-gif-btn" onclick="toggleGifPicker(event)" title="GIF">GIF</button>
          <label class="chat-img-btn" title="Photo">
            📷
            <input type="file" id="chat-img-input" accept="image/jpeg,image/png,image/gif" style="display:none" onchange="chatUploadImage(this)">
          </label>
          <input type="text" id="chat-input" class="chat-input" placeholder="`+i18n.T(lang, "chat.placeholder")+`" maxlength="5000" autocomplete="off">
          <button class="chat-send-btn" onclick="sendChatMsg()">➤</button>
        </div>
        <!-- Emoji picker -->
        <div class="emoji-picker hidden" id="emoji-picker"></div>
        <!-- GIF picker -->
        <div class="gif-picker hidden" id="gif-picker" onclick="event.stopPropagation()">
          <div class="gif-search-wrap">
            <input type="text" id="gif-search" class="gif-search-input" placeholder="`+i18n.T(lang, "chat.search_gif")+`" oninput="searchGifs(this.value)">
          </div>
          <div class="gif-grid" id="gif-grid"></div>
        </div>
      </div>
    </div>
    <div class="panel-title" style="margin-top:1rem">
      <svg width="14" height="14" viewBox="0 0 14 14"><path d="M1 2h12v1H1zm0 3h12v1H1zm0 3h8v1H1z" fill="currentColor"/></svg>
      `+i18n.T(lang, "home.community_feed")+`
    </div>
    <div id="posts-feed" class="posts-feed"><div class="posts-loading">`+i18n.T(lang, "common.loading")+`</div></div>

    <!-- Official Announcements -->
    <div style="margin-top:1.5rem">
      <div class="panel-title" style="margin-bottom:0.75rem">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg>
        `+i18n.T(lang, "home.announcements")+`
        <span class="ann-badge">OFFICIAL</span>
      </div>
      `+func() string {
		if user != nil && user.Pseudo == "Yousse" {
			return `<div style="margin-bottom:0.75rem">
        <button class="btn-green" onclick="openAnnModal()">+ New Announcement</button>
      </div>`
		}
		return ""
	}()+`
      <div id="announcements-list"><div style="color:#555;font-size:.85rem">`+i18n.T(lang, "common.loading")+`</div></div>
    </div>

    <!-- Modal rich editor annonces -->
    <div id="ann-modal" class="modal-overlay hidden" onclick="if(event.target===this)closeAnnModal()">
      <div class="modal-box" style="max-width:640px;width:95%%;max-height:90vh;overflow-y:auto;">
        <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:1rem;">
          <span style="font-weight:700;color:#fff">Announcement</span>
          <button onclick="closeAnnModal()" style="background:none;border:none;color:var(--text2);cursor:pointer;font-size:1.2rem;">&#10005;</button>
        </div>
        <input id="ann-title" type="text" placeholder="Title" style="width:100%%;background:#111;border:1px solid #222;border-radius:6px;padding:.5rem;color:#fff;margin-bottom:.75rem;box-sizing:border-box">
        <div class="ann-toolbar">
          <button type="button" onclick="annCmd('bold')" title="Bold"><b>B</b></button>
          <button type="button" onclick="annCmd('italic')" title="Italic"><i>I</i></button>
          <button type="button" onclick="annCmd('underline')" title="Underline"><u>U</u></button>
          <button type="button" onclick="annInsertLink()" title="Link">&#128279;</button>
          <button type="button" onclick="annCmd('insertUnorderedList')" title="List">&#8226; List</button>
          <label class="ann-toolbar-upload" title="Upload image">
            &#128444; Image
            <input type="file" id="ann-img-input" accept="image/*" style="display:none" onchange="annUploadImg(this)">
          </label>
        </div>
        <div id="ann-editor" contenteditable="true" class="ann-editor" oninput="annUpdatePreview()" placeholder="Write your announcement here..."></div>
        <div style="margin-top:.75rem;color:#555;font-size:.75rem;margin-bottom:.25rem">Preview:</div>
        <div id="ann-preview" class="ann-preview"></div>
        <div style="margin-top:.9rem;padding:.7rem;background:#071020;border:1px solid rgba(30,144,255,0.25);border-radius:7px;">
          <div style="font-size:.75rem;color:#5a7a9a;margin-bottom:.4rem">Image de couverture (optionnel) :</div>
          <div style="display:flex;align-items:center;gap:.5rem;flex-wrap:wrap;">
            <label style="display:inline-flex;align-items:center;gap:4px;background:#1e1e1e;border:1px solid #1e90ff44;border-radius:5px;padding:4px 10px;color:#aaa;cursor:pointer;font-size:.8rem;">
              &#128248; Parcourir
              <input type="file" id="ann-cover-input" accept="image/jpeg,image/png,image/gif,image/webp" style="display:none" onchange="annUploadCover(this)">
            </label>
            <span id="ann-cover-name" style="font-size:.75rem;color:#7a8fa6;"></span>
            <button id="ann-cover-remove" onclick="annRemoveCover()" style="display:none;background:none;border:none;color:#e55;cursor:pointer;font-size:.75rem;">&#10005; Retirer</button>
          </div>
          <div id="ann-cover-preview" style="margin-top:.5rem;"></div>
        </div>
        <div style="display:flex;gap:.5rem;margin-top:1rem">
          <button class="btn-green" onclick="submitAnn()">Publish</button>
          <button onclick="closeAnnModal()" style="background:#1e1e1e;border:none;border-radius:6px;padding:.4rem .9rem;color:#aaa;cursor:pointer">Cancel</button>
        </div>
      </div>
    </div>

    <!-- Modal commentaires -->
    <div id="comments-modal" class="modal-overlay hidden" onclick="if(event.target===this)closeCommentsModal()">
      <div class="modal-box" style="max-width:480px;max-height:80vh;overflow-y:auto;">
        <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:1rem;">
          <span style="font-weight:700">`+i18n.T(lang, "common.comments")+`</span>
          <button onclick="closeCommentsModal()" style="background:none;border:none;color:var(--text2);cursor:pointer;font-size:1.2rem;">&#10005;</button>
        </div>
        <div id="comments-list"></div>
      </div>
    </div>
    <!-- Context menu block/mute -->
    <div class="chat-ctx-menu hidden" id="chat-ctx-menu"></div>
    <!-- Reaction picker popover -->
    <div class="emoji-picker hidden" id="react-picker"></div>
  </div>


</div>

<script>
// ── Activité réseau ──
async function loadActivity() {
  const res = await fetch('/api/activity');
  const items = await res.json() || [];
  const el = document.getElementById('activity-feed');
  if (!items.length) { el.innerHTML='<div class="af-loading">'+I18N['common.no_activity']+'</div>'; return; }
  el.innerHTML = items.map(item => `+"`"+`
    <div class="af-item">
      <div class="af-dot ${item.type}"></div>
      <div class="af-text">${item.text}</div>
      <div class="af-time">${item.time}</div>
    </div>
  `+"`"+`).join('');
}

// ── Leaderboard ──
async function loadLeaderboard() {
  const res = await fetch('/api/leaderboard');
  const data = await res.json() || [];
  const el = document.getElementById('leaderboard');
  if (!data.length) { el.innerHTML='<div class="af-loading">'+I18N['common.no_miners']+'</div>'; return; }
  const medals = ['🥇','🥈','🥉'];
  el.innerHTML = data.slice(0,8).map((m,i) => `+"`"+`
    <div class="lb-row" onclick="showProfile('${m.pseudo}')">
      <span class="lb-rank">${medals[i]||('#'+(i+1))}</span>
      <span class="lb-avatar">${m.avatar_svg}</span>
      <span class="lb-pseudo">@${m.pseudo}</span>
      <span class="lb-blocks">${m.blocks} `+"`"+`+I18N['common.blocks']+`+"`"+`</span>
    </div>
  `+"`"+`).join('');
}

// ── Chat WebSocket ──
let chatWS;
function connectChatWS() {
  chatWS = new WebSocket((location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/ws/chat');
  chatWS.onmessage = (e) => { try { handleChatWS(JSON.parse(e.data)); } catch(_){} };
  chatWS.onclose = () => setTimeout(connectChatWS, 3000);
  chatWS.onerror = () => chatWS && chatWS.close();
}
function handleChatWS(msg) {
  if (msg.type === 'history') { renderChatHistory(msg.messages||[]); return; }
  if (msg.type === 'message') { appendChatMsg(msg.data); return; }
  if (msg.type === 'online_count') { const el=document.getElementById('chat-online'); if(el) el.textContent='● '+msg.count+' online'; return; }
  if (msg.type === 'reaction') { updateReactionsDOM(msg.message_id, msg.reactions); return; }
}
function renderChatHistory(msgs) {
  const el = document.getElementById('chat-messages');
  if (!el) return;
  el.innerHTML = '';
  msgs.forEach(m => appendChatMsg(m, false));
  el.scrollTop = el.scrollHeight;
}
function appendChatMsg(m, scroll=true) {
  const el = document.getElementById('chat-messages');
  if (!el) return;
  const div = document.createElement('div');
  div.className = 'chat-msg';
  div.dataset.id = m.id;
  div.dataset.userid = m.user_id;
  div.dataset.pseudo = m.pseudo;
  const ts = new Date(m.created_at);
  const timeStr = ts.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
  const minerBadge = m.is_miner ? '<img src="/static/mining_icon.png" style="height:14px;width:auto;vertical-align:middle;margin-left:3px;" title="Miner">' : '';
  let content = renderChatContent(m.content);
  let gif = m.gif_url ? `+"`"+`<div class="chat-gif"><img src="${m.gif_url}" alt="gif" loading="lazy"></div>`+"`"+` : '';
  let reactions = renderReactions(m.reactions||{}, m.id);
  div.innerHTML = `+"`"+`
    <div class="chat-msg-header">
      ${renderChatAvatar(m.avatar_url, m.pseudo, 36)}
      <span class="chat-pseudo">${escHtml(m.pseudo)}</span>${m.pseudo === 'Yousse' ? '<span class="scorbits-badge" title="Scorbits Official"><span>S</span></span>' : ''}${minerBadge}
      <span class="chat-time">${timeStr}</span>
      <span class="chat-actions">
        <button class="chat-react-btn" onclick="openReactPicker(event,'${m.id}')" title="React">😊</button>
        <button class="chat-menu-btn" onclick="openChatMenu(event,'${m.id}','${escHtml(m.pseudo)}','${m.user_id}','${escHtml(m.avatar_url||'')}')">⋯</button>
        ${IS_ADMIN ? '<button class="admin-del" onclick="adminDeleteChatMsg(\''+m.id+'\')" title="Supprimer (admin)">✕</button>' : ''}
      </span>
    </div>
    <div class="chat-msg-body">${content}${gif}${m.shared_post ? renderSharedPost(m.shared_post) : ''}</div>
    <div class="chat-reactions" id="reactions-${m.id}">${reactions}</div>
  `+"`"+`;
  if (m.content) {
    const _tBtn = document.createElement('button');
    _tBtn.textContent = LANG === 'fr' ? 'Traduire' : 'Translate';
    _tBtn.style.cssText = 'background:none;border:none;color:#444;font-size:.72rem;cursor:pointer;padding:1px 4px;margin-top:2px;display:block;';
    _tBtn.dataset.t = m.content;
    _tBtn.onclick = function(){ translateMsg(this.dataset.t, this); };
    const _body = div.querySelector('.chat-msg-body');
    if (_body) _body.insertAdjacentElement('afterend', _tBtn);
  }
  el.appendChild(div);
  if (scroll) el.scrollTop = el.scrollHeight;
}
function renderChatContent(text) {
  if (!text) return '';
  // Inline chat image uploads
  if (text.startsWith('/static/uploads/chat/')) {
    return `+"`"+`<a href="${escHtml(text)}" target="_blank" rel="noopener"><img src="${escHtml(text)}" class="chat-img" loading="lazy"></a>`+"`"+`;
  }
  let s = linkify(text);
  s = s.replace(/:sco:/g, '<img src="/static/scorbits_logo.png" style="height:18px;width:auto;vertical-align:middle">');
  return s;
}
function renderTextLinks(text) {
  if (!text) return '';
  return linkify(text);
}
function renderChatAvatar(avatarUrl, pseudo, size) {
  size = size || 36;
  const initial = pseudo ? pseudo.charAt(0).toUpperCase() : '?';
  const fs = Math.floor(size / 2.5);
  const base = 'width:'+size+'px;height:'+size+'px;border-radius:50%%;flex-shrink:0;';
  if (avatarUrl && avatarUrl.trim() !== '') {
    return '<img src="'+escHtml(avatarUrl)+'" '+
      'style="'+base+'object-fit:cover;display:block;background:#0d2010;" '+
      'onerror="this.style.display=\'none\';this.nextElementSibling.style.display=\'flex\';" '+
      'alt="'+escHtml(pseudo||'')+'">' +
      '<div style="display:none;'+base+'background:#0d2010;align-items:center;justify-content:center;'+
      'font-weight:700;color:#00e85a;font-size:'+fs+'px;">'+initial+'</div>';
  } else {
    return '<div style="display:flex;'+base+'background:#0d2010;align-items:center;justify-content:center;'+
      'font-weight:700;color:#00e85a;font-size:'+fs+'px;">'+initial+'</div>';
  }
}
function renderReactions(reactions, msgId) {
  if (!reactions || !Object.keys(reactions).length) return '';
  return Object.entries(reactions).filter(([,u])=>u&&u.length).map(([emoji,users])=>{
    const isMine = CURRENT_USER_ID && users.includes(CURRENT_USER_ID);
    const cls = isMine ? 'chat-reaction-chip chat-reaction-mine' : 'chat-reaction-chip';
    return `+"`"+`<button class="${cls}" onclick="toggleReaction('${msgId}','${escHtml(emoji)}')">${emoji} <span>${users.length}</span></button>`+"`"+`;
  }).join('');
}
let pendingSharedPostId = null;
let _pendingChatImgUrl = ''; // uploaded image URL waiting to be sent
let _pendingChatGifUrl = ''; // gif URL waiting to be sent

function _showChatMediaPreview(url) {
  const box = document.getElementById('chat-media-preview');
  const img = document.getElementById('chat-media-preview-img');
  if (!box || !img) return;
  img.src = url;
  box.classList.remove('hidden');
}
function cancelChatMedia() {
  _pendingChatImgUrl = '';
  _pendingChatGifUrl = '';
  const box = document.getElementById('chat-media-preview');
  const img = document.getElementById('chat-media-preview-img');
  if (box) box.classList.add('hidden');
  if (img) img.src = '';
  const inp = document.getElementById('chat-img-input');
  if (inp) inp.value = '';
}
async function sendChatMsg() {
  if (!IS_LOGGED_IN) { window.location='/wallet'; return; }
  const inp = document.getElementById('chat-input');
  const text = inp.value.trim();
  const gifUrl = _pendingChatGifUrl || '';
  const imgUrl = _pendingChatImgUrl || '';
  if (!text && !gifUrl && !imgUrl && !pendingSharedPostId) return;
  try {
    if (imgUrl) {
      // Send image as content, then text separately if present
      await fetch('/api/chat', {method:'POST', headers:{'Content-Type':'application/json'},
        body: JSON.stringify({content: imgUrl, gif_url: '', shared_post_id: pendingSharedPostId || ''})});
      if (text) {
        await fetch('/api/chat', {method:'POST', headers:{'Content-Type':'application/json'},
          body: JSON.stringify({content: text, gif_url: '', shared_post_id: ''})});
      }
    } else {
      await fetch('/api/chat', {method:'POST', headers:{'Content-Type':'application/json'},
        body: JSON.stringify({content: text, gif_url: gifUrl, shared_post_id: pendingSharedPostId || ''})});
    }
    inp.value = '';
    cancelChatMedia();
    document.getElementById('gif-picker').classList.add('hidden');
    cancelSharePost();
  } catch(e) {}
}
async function chatUploadImage(input) {
  if (!IS_LOGGED_IN) { window.location='/wallet'; return; }
  if (!input.files || !input.files[0]) return;
  const file = input.files[0];
  if (file.size > 5 * 1024 * 1024) { alert('Image too large (max 5MB)'); input.value = ''; return; }
  const fd = new FormData();
  fd.append('image', file);
  try {
    const res = await fetch('/api/chat/upload-image', {method:'POST', body:fd});
    const data = await res.json();
    if (data.url) {
      _pendingChatImgUrl = data.url;
      _pendingChatGifUrl = '';
      _showChatMediaPreview(data.url);
      document.getElementById('chat-input').focus();
    } else {
      alert('Upload failed');
    }
  } catch(e) { alert('Upload error'); }
  input.value = '';
}
window.chatUploadImage = chatUploadImage;
window.cancelChatMedia = cancelChatMedia;
document.addEventListener('DOMContentLoaded', () => {
  const inp = document.getElementById('chat-input');
  if (inp) {
    inp.addEventListener('keydown', e => { if(e.key==='Enter'&&!e.shiftKey){e.preventDefault();sendChatMsg();} });
    setupMentionAC(inp, document.getElementById('chat-mentions'));
  }
  // Wire mention autocomplete for publish modal textarea
  const pubTa = document.getElementById('publish-content');
  if (pubTa) setupMentionAC(pubTa, document.getElementById('publish-mentions'));
  connectChatWS();
  buildEmojiPicker('emoji-picker', emoji => {
    const inp = document.getElementById('chat-input');
    if (inp) { inp.value += emoji; inp.focus(); }
    document.getElementById('emoji-picker').classList.add('hidden');
  });
  buildEmojiPicker('react-picker', emoji => {
    if (reactPickerTarget) toggleReaction(reactPickerTarget, emoji);
    document.getElementById('react-picker').classList.add('hidden');
    reactPickerTarget = null;
  });
});
// ── Generic @mention autocomplete ──
let _macInp = null, _macDrop = null, _macTimer = null;
function setupMentionAC(inp, drop) {
  if (!inp || !drop || inp._mentionACSet) return;
  inp._mentionACSet = true;
  inp.addEventListener('input', () => triggerMentionAC(inp, drop));
  inp.addEventListener('keydown', e => { if (e.key === 'Escape') drop.classList.add('hidden'); });
  inp.addEventListener('blur', () => setTimeout(() => { if (drop) drop.classList.add('hidden'); }, 200));
}
async function triggerMentionAC(inp, drop) {
  _macInp = inp; _macDrop = drop;
  const pos = inp.selectionStart !== undefined ? inp.selectionStart : inp.value.length;
  const before = inp.value.slice(0, pos);
  const atIdx = before.lastIndexOf('@');
  if (atIdx < 0) { drop.classList.add('hidden'); return; }
  const partial = before.slice(atIdx + 1);
  if (partial.includes(' ') || partial.length < 1) { drop.classList.add('hidden'); return; }
  clearTimeout(_macTimer);
  _macTimer = setTimeout(async () => {
    try {
      const res = await fetch('/api/users/search?q=' + encodeURIComponent(partial));
      const users = await res.json();
      if (!Array.isArray(users) || !users.length) { drop.classList.add('hidden'); return; }
      drop.innerHTML = users.slice(0, 6).map(u =>
        '<div class="mention-item" onmousedown="insertMentionAC(event,\'' + escHtml(u.pseudo) + '\')">@' + escHtml(u.pseudo) + '</div>'
      ).join('');
      drop.classList.remove('hidden');
    } catch(e) { drop.classList.add('hidden'); }
  }, 200);
}
function insertMentionAC(event, pseudo) {
  event.preventDefault();
  if (!_macInp) return;
  const pos = _macInp.selectionStart !== undefined ? _macInp.selectionStart : _macInp.value.length;
  const val = _macInp.value;
  const before = val.slice(0, pos);
  const atIdx = before.lastIndexOf('@');
  const newVal = val.slice(0, atIdx) + '@' + pseudo + ' ' + val.slice(pos);
  _macInp.value = newVal;
  const newPos = atIdx + pseudo.length + 2;
  try { _macInp.setSelectionRange(newPos, newPos); } catch(e) {}
  _macInp.focus();
  if (_macDrop) _macDrop.classList.add('hidden');
}
function insertMention(pseudo) {
  if (_macInp) { insertMentionAC({preventDefault:()=>{}}, pseudo); }
}
window.insertMentionAC = insertMentionAC;
window.insertMention = insertMention;
function positionEmojiPicker(triggerBtn, pickerEl) {
  const rect = triggerBtn.getBoundingClientRect();
  const pickerHeight = 380;
  const pickerWidth = 320;
  let top = rect.top - pickerHeight - 8;
  let left = rect.left;
  if (top < 10) {
    top = rect.bottom + 8;
  }
  if (left + pickerWidth > window.innerWidth - 10) {
    left = window.innerWidth - pickerWidth - 10;
  }
  if (left < 10) left = 10;
  pickerEl.style.position = 'fixed';
  pickerEl.style.top = top + 'px';
  pickerEl.style.left = left + 'px';
  pickerEl.style.bottom = '';
  pickerEl.style.right = '';
  pickerEl.style.zIndex = '99999';
}
function toggleEmojiPicker(e) {
  e.stopPropagation();
  const picker = document.getElementById('emoji-picker');
  const isHidden = picker.classList.contains('hidden');
  document.getElementById('react-picker').classList.add('hidden');
  document.getElementById('gif-picker').classList.add('hidden');
  if (isHidden) {
    positionEmojiPicker(e.currentTarget, picker);
    picker.classList.remove('hidden');
  } else {
    picker.classList.add('hidden');
  }
}
let reactPickerTarget = null;
function openReactPicker(e, msgId) {
  e.stopPropagation();
  const picker = document.getElementById('react-picker');
  reactPickerTarget = msgId;
  document.getElementById('emoji-picker').classList.add('hidden');
  document.getElementById('gif-picker').classList.add('hidden');
  positionEmojiPicker(e.currentTarget, picker);
  picker.classList.remove('hidden');
  // Highlight current user's reaction in the picker
  const reactionsEl = document.getElementById('reactions-' + msgId);
  let currentEmoji = null;
  if (reactionsEl) {
    const mineChip = reactionsEl.querySelector('.chat-reaction-mine');
    if (mineChip) currentEmoji = mineChip.textContent.trim().replace(/\s\d+$/, '').trim();
  }
  picker.querySelectorAll('.epk-item').forEach(item => {
    item.classList.toggle('epk-item-active', !!currentEmoji && item.textContent.trim() === currentEmoji);
  });
}
async function toggleReaction(msgId, emoji) {
  if (!IS_LOGGED_IN) { window.location='/wallet'; return; }
  console.log('[reaction] toggle', msgId, emoji);
  try {
    const res = await fetch('/api/chat/reaction', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({message_id: msgId, emoji: emoji})
    });
    console.log('[reaction] status', res.status);
    if (res.status === 401) { alert('Login to react'); return; }
    if (res.ok) {
      const data = await res.json();
      console.log('[reaction] data', data);
      if (data.reactions) updateReactionsDOM(msgId, data.reactions);
    }
  } catch(e) { console.error('[reaction] error', e); }
}
function updateReactionsDOM(msgId, reactions) {
  const el = document.getElementById('reactions-' + msgId);
  if (el) el.innerHTML = renderReactions(reactions, msgId);
}
function openChatMenu(e, msgId, pseudo, userId, avatar) {
  e.stopPropagation();
  const menu = document.getElementById('chat-ctx-menu');
  let html = `+"`"+`<div class="ctx-item" onclick="blockChatUser('${userId}','${escHtml(pseudo)}')">Block @${escHtml(pseudo)}</div>`+"`"+`;
  if (userId && userId !== CURRENT_USER_ID && document.getElementById('dm-window')) {
    html = '<div class="ctx-item" data-action="dm" data-user-id="'+userId+'" data-pseudo="'+escHtml(pseudo)+'" data-avatar="'+(avatar||'')+'">✉️ Send Private Message</div>' + html;
  }
  if (IS_ADMIN) {
    html += `+"`"+`
      <div class="ctx-sep"></div>
      <div class="ctx-label">Mute ${escHtml(pseudo)}</div>
      <div class="ctx-item" onclick="muteChatUser('${userId}','5m')">5 min</div>
      <div class="ctx-item" onclick="muteChatUser('${userId}','1h')">1h</div>
      <div class="ctx-item" onclick="muteChatUser('${userId}','24h')">24h</div>
      <div class="ctx-item" onclick="muteChatUser('${userId}','240h')">10 jours</div>
      <div class="ctx-item" onclick="muteChatUser('${userId}','permanent')">Permanent</div>
    `+"`"+`;
  }
  menu.innerHTML = html;
  const dmItem = menu.querySelector('[data-action="dm"]');
  if (dmItem) {
    dmItem.addEventListener('click', function() {
      const uid = this.dataset.userId;
      const ps = this.dataset.pseudo;
      const av = this.dataset.avatar || '';
      menu.classList.add('hidden');
      const dmWin = document.getElementById('dm-window');
      if (dmWin) {
        if (!dmWindowOpen) toggleDMWindow();
        openDMConversation(uid, ps, av);
      }
    });
  }
  menu.classList.remove('hidden');
  const rect = e.currentTarget.getBoundingClientRect();
  menu.style.top = (rect.bottom + 4) + 'px';
  menu.style.left = rect.left + 'px';
}
document.addEventListener('click', () => {
  document.getElementById('chat-ctx-menu')?.classList.add('hidden');
  document.getElementById('emoji-picker')?.classList.add('hidden');
  document.getElementById('react-picker')?.classList.add('hidden');
  document.getElementById('gif-picker')?.classList.add('hidden');
});
async function blockChatUser(userId, pseudo) {
  document.getElementById('chat-ctx-menu').classList.add('hidden');
  if (!confirm('Block @' + pseudo + '?')) return;
  try { await fetch('/api/chat/block', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({blocked_id: userId})}); } catch(e) {}
}
async function muteChatUser(userId, duration) {
  document.getElementById('chat-ctx-menu').classList.add('hidden');
  try { await fetch('/api/chat/mute', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({user_id: userId, duration: duration})}); } catch(e) {}
}

// ── GIF picker ──
let gifSearchTimeout;
function toggleGifPicker(e) {
  e.stopPropagation();
  const p = document.getElementById('gif-picker');
  const isHidden = p.classList.toggle('hidden');
  document.getElementById('emoji-picker').classList.add('hidden');
  document.getElementById('react-picker').classList.add('hidden');
  if (!isHidden) {
    // Panel just opened: focus search and load trending
    setTimeout(() => { const inp = document.getElementById('gif-search'); if (inp) inp.focus(); }, 50);
    const grid = document.getElementById('gif-grid');
    if (grid && grid.innerHTML === '') loadGifs('trending');
  }
}
async function loadGifs(q) {
  const grid = document.getElementById('gif-grid');
  if (!grid) return;
  console.log('[GIF] recherche:', q);
  grid.innerHTML = '<div style="color:var(--muted);font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">Loading...</div>';
  try {
    const res = await fetch('/api/chat/gif?q=' + encodeURIComponent(q));
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = await res.json();
    console.log('[GIF] résultats:', data);
    if (!Array.isArray(data) || data.length === 0) {
      grid.innerHTML = '<div style="color:var(--muted);font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">No GIFs found for \'' + escHtml(q) + '\'</div>';
      return;
    }
    grid.innerHTML = data.map(g => `+"`"+`<img
      src="${escHtml(g.preview||g.url)}"
      data-gif="${escHtml(g.url)}"
      class="chat-gif-item"
      loading="lazy"
      style="width:100%%;height:80px;object-fit:cover;cursor:pointer;border-radius:4px;border:2px solid transparent;"
      onmouseover="this.style.borderColor='#00e85a'"
      onmouseout="this.style.borderColor='transparent'"
      onerror="this.style.display='none'"
      onclick="selectGif(this.dataset.gif)">`+"`"+`).join('');
  } catch(err) {
    console.error('[GIF] erreur:', err);
    grid.innerHTML = '<div style="color:#ff6464;font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">Error loading GIFs</div>';
  }
}
function searchGifs(q) {
  clearTimeout(gifSearchTimeout);
  const grid = document.getElementById('gif-grid');
  if (!q) { if (grid) grid.innerHTML = ''; return; }
  const searching = '<div style="color:var(--muted);font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">Searching...</div>';
  if (grid) grid.innerHTML = searching;
  gifSearchTimeout = setTimeout(() => loadGifs(q), 600);
}
function selectGif(url) {
  _pendingChatGifUrl = url;
  _pendingChatImgUrl = '';
  _showChatMediaPreview(url);
  document.getElementById('gif-picker').classList.add('hidden');
  const inp = document.getElementById('chat-input');
  if (inp) inp.focus();
}

// ── Emoji picker builder ──
const EMOJI_CATEGORIES = [
  {icon:'⭐',name:'Scorbits',emojis:['<img src="/static/scorbits_logo.png" style="height:20px;width:auto;vertical-align:middle" data-emoji=":sco:" title="Scorbits">']},
  {icon:'😀',name:'Smileys',emojis:['😀','😃','😄','😁','😆','😅','🤣','😂','🙂','🙃','😉','😊','😇','🥰','😍','🤩','😘','😗','😚','😙','🥲','😋','😛','😜','🤪','😝','🤑','🤗','🤭','🤫','🤔','🤐','🤨','😐','😑','😶','😏','😒','🙄','😬','🤥','😌','😔','😪','🤤','😴','😷','🤒','🤕','🤢','🤮','🤧','🥵','🥶','🥴','😵','💫','🤯','🤠','🥸','🤡','👹','👺','💀','☠️','👻','👽','👾','🤖','😺','😸','😹','😻','😼','😽','🙀','😿','😾']},
  {icon:'👋',name:'Gestures',emojis:['👋','🤚','🖐','✋','🖖','🤙','👌','🤌','🤏','✌️','🤞','🤟','🤘','🤙','👈','👉','👆','🖕','👇','☝️','👍','👎','✊','👊','🤛','🤜','🤝','👏','🙌','🫶','👐','🤲','🙏','✍️','💅','🤳','💪','🦾','🦿','🦵','🦶','👂','🦻','👃','🫀','🫁','🧠','👁','👀','🫦']},
  {icon:'🐶',name:'Animals',emojis:['🐶','🐱','🐭','🐹','🐰','🦊','🐻','🐼','🐨','🐯','🦁','🐮','🐷','🐸','🐵','🙈','🙉','🙊','🐔','🐧','🐦','🦅','🦆','🦉','🦇','🐺','🐗','🐴','🦄','🐝','🪱','🐛','🦋','🐌','🐞','🐜','🦟','🦗','🕷','🦂','🐢','🐍','🦎','🦖','🦕','🐙','🦑','🦐','🦞','🦀','🐡','🐠','🐟','🐬','🐳','🐋','🦈','🦭','🐊','🐅','🐆','🦓','🦍','🦧','🦣','🐘','🦛','🦏','🐪','🐫','🦒','🦘','🦬','🐃','🐂','🐄','🐎','🐖','🐏','🐑','🦙','🐐','🦌','🐕','🐩','🦮','🐕‍🦺','🐈','🐈‍⬛','🪶','🐓','🦃','🦤','🦚','🦜','🦢','🕊','🐇','🦝','🦨','🦡','🦫','🦦','🦥','🐁','🐀','🐿','🦔','🐾','🐉','🐲','🌵','🎄','🌲','🌳','🌴','🪵','🌱','🌿','☘️','🍀','🎋','🎍','🪴','🍃','🍂','🍁','🍄','🐚','🌾','💐','🌷','🌹','🥀','🌺','🌸','🌼','🌻','🌞','🌝','🌛','🌜','🌚','🌕','🌖','🌗','🌘','🌑','🌒','🌓','🌔','🌙','🌟','⭐','💫','✨','🌤','⛅','🌥','☁️','🌦','🌧','⛈','🌩','🌨','❄️','☃️','⛄','🌬','🌀','🌈','🌂','☂️','☔','⛱','⚡','🌊','🌫']},
  {icon:'🍎',name:'Food',emojis:['🍎','🍊','🍋','🍇','🍓','🫐','🍈','🍒','🍑','🥭','🍍','🥥','🥝','🍅','🍆','🥑','🥦','🥬','🥒','🫑','🌶','🫒','🧄','🧅','🥕','🌽','🥗','🥙','🥪','🌮','🌯','🫔','🥫','🍝','🍜','🍲','🍛','🍣','🍱','🥟','🦪','🍤','🍙','🍚','🍘','🍥','🥮','🍢','🧁','🍰','🎂','🍮','🍭','🍬','🍫','🍿','🍩','🍪','🌰','🥜','🍯','🧃','🥤','🧋','☕','🍵','🍶','🍺','🍻','🥂','🍷','🥃','🍸','🍹','🧉','🍾','🧊','🥄','🍴','🍽','🥢','🧂']},
  {icon:'🚗',name:'Travel',emojis:['🚗','🚕','🚙','🚌','🚎','🏎','🚓','🚑','🚒','🚐','🛻','🚚','🚛','🚜','🏍','🛵','🚲','🛴','🛹','🛼','🚏','🛺','🚨','🚔','🚍','🚘','🚖','🚡','🚠','🚟','🚃','🚋','🚝','🚄','🚅','🚈','🚂','🚆','🚇','🚊','🚞','🚉','🛬','🛫','✈️','🛩','💺','🛸','🚁','🛶','⛵','🚤','🛥','🛳','⛴','🚢','⚓','🪝','⛽','🚧','🚦','🚥','🛑','🗺','🧭','🏔','⛰','🌋','🗻','🏕','🏖','🏜','🏝','🏞','🏟','🏛','🏗','🧱','🪞','🪟','🏠','🏡','🏢','🏣','🏤','🏥','🏦','🏨','🏩','🏪','🏫','🏭','🏯','🏰','💒','🗼','🗽','⛪','🕌','🕍','⛩','🛕']},
  {icon:'⚽',name:'Activities',emojis:['⚽','🏀','🏈','⚾','🥎','🎾','🏐','🏉','🎱','🪀','🏓','🏸','🥊','🥋','🎯','🪃','🏹','🎣','🤿','🎽','🎿','🛷','🥌','🪁','⛸','🤸','🤼','🤺','🥇','🥈','🥉','🏆','🎖','🏅','🎗','🎫','🎟','🎪','🤹','🎭','🩰','🎨','🎬','🎤','🎧','🎼','🎵','🎶','🎹','🪘','🥁','🎷','🎺','🎸','🪕','🎻','🎲','♟','🎯','🎳','🎮','🕹','🎰']},
  {icon:'💌',name:'Objects',emojis:['💌','📩','📨','📧','💬','💭','🗯','📱','📲','💻','⌨','🖥','🖨','🖱','🖲','💾','💿','📀','🧮','📷','📸','📹','🎥','📽','🎞','📞','☎️','📟','📠','📺','📻','🧭','⏱','⏲','⏰','🕰','⌚','⏳','🌡','🧿','🪬','🔮','🧲','🪄','🔭','🔬','🩺','🩻','💉','🩸','💊','🩹','🩼','🪒','🧴','🧷','🧹','🧺','🧻','🧼','🫧','🪥','🧽','🪣','🪤','🛒','🚪','🪑','🛋','🛏','🛁','🪞','🪟','🧸','🪆','🖼','🛍','🎁','🎀','🎊','🎉']},
  {icon:'❤️',name:'Symbols',emojis:['❤️','🧡','💛','💚','💙','💜','🖤','🤍','🤎','💔','❣️','💕','💞','💓','💗','💖','💘','💝','💟','☮️','✝️','☪️','🕉','☸️','✡️','🔯','🕎','☯️','☦️','🛐','⛎','♈','♉','♊','♋','♌','♍','♎','♏','♐','♑','♒','♓','🆔','⚛️','🉑','☢️','☣️','📴','📳','🈶','🈚','🈸','🈺','🈷','✴️','🆚','💮','🉐','㊙️','㊗️','🈴','🈵','🈹','🈲','🅰','🅱','🆎','🆑','🅾','🆘','❌','⭕','🛑','⛔','📛','🚫','💯','🔞','📵','🚳','🚭','🚯','🚱','🚷','🔇','🔕']},
];
function buildEmojiPicker(pickerId, onSelect) {
  var picker = document.getElementById(pickerId);
  if (!picker) return;
  picker._onSelect = onSelect;
  if (picker._built) return;
  picker._built = true;
  var sid = pickerId+'-srch';
  var gid = pickerId+'-grid';
  var catBar = EMOJI_CATEGORIES.map(function(cat,i){
    return '<span class="epk-cat-btn" title="'+cat.name+'" onclick="epkScrollTo(\''+pickerId+'\','+i+')">'+cat.icon+'</span>';
  }).join('');
  var grid = EMOJI_CATEGORIES.map(function(cat,i){
    var emojis = cat.emojis.map(function(e){
      if(e.charAt(0)==='<'){
        var tmp=document.createElement('div');tmp.innerHTML=e;
        var img=tmp.querySelector('img');
        var de=img?img.getAttribute('data-emoji'):'';
        var ttl=img?img.getAttribute('title'):'';
        return '<span class="epk-item" onclick="onEmojiSelect(event,\''+de+'\',\''+pickerId+'\')" title="'+ttl+'">'+e+'</span>';
      }
      return '<span class="epk-item" onclick="onEmojiSelect(event,\''+e+'\',\''+pickerId+'\')">'+e+'</span>';
    }).join('');
    return '<div class="epk-section" id="'+pickerId+'-c'+i+'"><div class="epk-cat-label">'+cat.icon+' '+cat.name+'</div><div class="epk-emojis">'+emojis+'</div></div>';
  }).join('');
  picker.innerHTML = '<input id="'+sid+'" type="text" placeholder="Search emoji..." class="epk-search" oninput="epkFilter(\''+pickerId+'\',this.value)">'
    +'<div class="epk-cat-bar">'+catBar+'</div>'
    +'<div class="epk-grid-wrap" id="'+gid+'">'+grid+'</div>';
}
function epkScrollTo(pickerId, i) {
  var el=document.getElementById(pickerId+'-c'+i);
  var grid=document.getElementById(pickerId+'-grid');
  if(el&&grid) grid.scrollTop=el.offsetTop;
}
function epkFilter(pickerId, q) {
  var grid=document.getElementById(pickerId+'-grid');
  if(!grid) return;
  var sections=grid.querySelectorAll('.epk-section');
  if(!q){
    sections.forEach(function(s){s.style.display='';s.querySelectorAll('.epk-item').forEach(function(it){it.style.display='';});});
    return;
  }
  q=q.toLowerCase();
  sections.forEach(function(s){
    var any=false;
    s.querySelectorAll('.epk-item').forEach(function(it){
      var m=it.getAttribute('title').toLowerCase().indexOf(q)>=0||it.textContent.toLowerCase().indexOf(q)>=0;
      it.style.display=m?'':'none';
      if(m) any=true;
    });
    s.style.display=any?'':'none';
  });
}
function onEmojiSelect(e, emoji, pickerId) {
  e.stopPropagation();
  var picker = document.getElementById(pickerId);
  if (picker && picker._onSelect) picker._onSelect(emoji);
}
function relAge(iso) {
  const sec = Math.floor((Date.now() - new Date(iso)) / 1000);
  if(sec < 60) return I18N['common.instantly'];
  if(sec < 3600) return Math.floor(sec/60)+(I18N['common.ago_min']||'min');
  if(sec < 86400) return Math.floor(sec/3600)+(I18N['common.ago_h']||'h');
  return Math.floor(sec/86400)+(I18N['common.ago_d']||'d');
}
async function loadPosts() {
  const el = document.getElementById('posts-feed');
  if(!el) return;
  try {
    const res = await fetch('/api/posts');
    const posts = await res.json() || [];
    if(!posts.length) { el.innerHTML='<div class="posts-empty" style="text-align:center;color:var(--green);padding:2rem 0">'+I18N['common.no_posts']+'</div>'; return; }
    el.innerHTML = posts.map(p => {
      const isOwn = IS_LOGGED_IN && p.pseudo === CURRENT_PSEUDO;
      const likeActive = p.user_reaction==='like' ? ' active' : '';
      const dislikeActive = p.user_reaction==='dislike' ? ' active' : '';
      const delBtn = isOwn ? '<button class="post-delete-btn" onclick="deletePost(\''+p.id+'\')" title="Supprimer">&#128465;</button>' : (IS_ADMIN ? '<button class="admin-del" onclick="adminDeletePost(\''+p.id+'\')" title="Supprimer (admin)">✕</button>' : '');
      return '<div class="post-card" id="post-'+p.id+'">'+
        '<div class="post-header">'+
          '<span class="post-avatar" onclick="openProfileModal(\''+escHtml(p.pseudo)+'\')" title="@'+escHtml(p.pseudo)+'">'+p.avatar_svg+'</span>'+
          '<div class="post-meta">'+
            '<span class="post-pseudo" onclick="openProfileModal(\''+escHtml(p.pseudo)+'\')">@'+escHtml(p.pseudo)+(p.pseudo==='Yousse'?'<span class="scorbits-badge" title="Scorbits Official"><span>S</span></span>':'')+'</span>'+
            '<span class="post-age">'+relAge(p.created_at)+'</span>'+
          '</div>'+
          delBtn+
        '</div>'+
        '<div class="post-content">'+renderTextLinks(p.content)+'</div>'+
        (p.content ? '<button onclick="translateMsg(this.dataset.t,this)" data-t="'+escHtml(p.content||'').replace(/"/g,'&quot;')+'" style="background:none;border:none;color:#444;font-size:.72rem;cursor:pointer;padding:1px 4px;margin-top:2px;display:block;">'+(LANG==='fr'?'Traduire':'Translate')+'</button>' : '')+
        (p.image_url?'<img class="post-image" src="'+escHtml(p.image_url)+'" loading="lazy" onclick="openImageLightbox(this.src)">':'')+
        (p.gif_url?'<img class="post-image" src="'+escHtml(p.gif_url)+'" loading="lazy" onclick="openImageLightbox(this.src)">':'')+
        '<div class="post-actions">'+
          '<button class="post-react-btn'+likeActive+'" id="like-'+p.id+'" onclick="reactPost(\''+p.id+'\',\'like\')">&#128077; <span>'+p.likes+'</span></button>'+
          '<button class="post-react-btn'+dislikeActive+'" id="dislike-'+p.id+'" onclick="reactPost(\''+p.id+'\',\'dislike\')">&#128078; <span>'+p.dislikes+'</span></button>'+
          '<button class="post-comment-btn" onclick="toggleCommentBox(\''+p.id+'\')">&#128172; '+I18N['common.comment_btn']+'</button>'+
          '<button class="post-view-btn" onclick="openCommentsModal(\''+p.id+'\')">'+I18N['common.view_comments']+(p.comments_count>0?' ('+p.comments_count+')':'')+'</button>'+
          (IS_LOGGED_IN ? '<button class="post-share-btn" onclick="sharePostToChat(\''+p.id+'\',\''+escHtml(p.pseudo)+'\')" title="'+I18N['chat.share_to_chat']+'">&#x1F4AC; '+I18N['chat.share_to_chat']+'</button>' : '')+
        '</div>'+
        '<div class="post-comment-box hidden" id="pcb-'+p.id+'">'+
          '<div style="display:flex;gap:0.4rem;margin-top:0.5rem;align-items:center;">'+
            '<div style="flex:1;position:relative;">'+
              '<input type="text" class="post-comment-input" id="pci-'+p.id+'" placeholder="'+I18N['common.comment_placeholder']+'" maxlength="200" onkeydown="if(event.key===\'Enter\')submitComment(\''+p.id+'\')">'+
              '<div class="chat-mentions-list hidden" id="pcm-'+p.id+'" style="bottom:auto;top:100%%;z-index:100"></div>'+
            '</div>'+
            '<button class="post-comment-submit" onclick="submitComment(\''+p.id+'\')">'+I18N['common.send_btn']+'</button>'+
          '</div>'+
        '</div>'+
      '</div>';
    }).join('');
  } catch(e) { el.innerHTML='<div class="posts-empty">'+I18N['common.loading_error']+'</div>'; }
}
async function reactPost(postId, reaction) {
  if(!IS_LOGGED_IN){window.location='/wallet';return;}
  try {
    const res = await fetch('/api/posts/react',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({post_id:postId,reaction})});
    if(!res.ok) return;
    const d = await res.json();
    const lb = document.getElementById('like-'+postId);
    const db2 = document.getElementById('dislike-'+postId);
    if(lb){lb.innerHTML='&#128077; <span>'+d.likes+'</span>';lb.classList.toggle('active',d.user_reaction==='like');}
    if(db2){db2.innerHTML='&#128078; <span>'+d.dislikes+'</span>';db2.classList.toggle('active',d.user_reaction==='dislike');}
  } catch(e) {}
}
async function deletePost(postId) {
  if(!confirm(I18N['common.confirm_delete_post'])) return;
  try {
    const res = await fetch('/api/posts?id='+postId,{method:'DELETE'});
    if(res.ok) {
      const card = document.getElementById('post-'+postId);
      if(card) card.remove();
    }
  } catch(e) {}
}
async function deleteComment(commentId, btn) {
  if(!confirm(I18N['common.confirm_delete_comment'])) return;
  try {
    const res = await fetch('/api/posts/comments?id='+commentId,{method:'DELETE'});
    if(res.ok) {
      const item = document.getElementById('cmt-'+commentId);
      if(item) item.remove();
    }
  } catch(e) {}
}
async function adminDeletePost(postId) {
  if(!confirm('Supprimer cette publication (admin) ?')) return;
  try {
    const res = await fetch('/api/admin/post/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:postId})});
    if(res.ok) {
      const card = document.getElementById('post-'+postId);
      if(card) card.remove();
    }
  } catch(e) {}
}
async function adminDeleteComment(commentId, btn) {
  if(!confirm('Supprimer ce commentaire (admin) ?')) return;
  try {
    const res = await fetch('/api/admin/comment/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:commentId})});
    if(res.ok) {
      const item = document.getElementById('cmt-'+commentId);
      if(item) item.remove();
    }
  } catch(e) {}
}
async function adminDeleteChatMsg(msgId) {
  if(!confirm('Supprimer ce message (admin) ?')) return;
  try {
    const res = await fetch('/api/admin/chat/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:msgId})});
    if(res.ok) {
      const el = document.querySelector('.chat-msg[data-id="'+msgId+'"]');
      if(el) el.remove();
    }
  } catch(e) {}
}
function toggleCommentBox(postId) {
  const box = document.getElementById('pcb-'+postId);
  if(!box) return;
  const hidden = box.classList.toggle('hidden');
  if(!hidden) {
    const inp = document.getElementById('pci-'+postId);
    if(inp) {
      inp.focus();
      setupMentionAC(inp, document.getElementById('pcm-'+postId));
    }
  }
}
async function submitComment(postId) {
  const inp = document.getElementById('pci-'+postId);
  if(!inp || !inp.value.trim()) return;
  try {
    await fetch('/api/posts/comments',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({post_id:postId,content:inp.value.trim()})});
    inp.value='';
    document.getElementById('pcb-'+postId).classList.add('hidden');
  } catch(e) {}
}
function renderCommentItem(c, postId, isReply) {
  const isOwn = IS_LOGGED_IN && c.pseudo === CURRENT_PSEUDO;
  const likes = c.likes || [];
  const likeCount = likes.length;
  const isLiked = IS_LOGGED_IN && likes.includes(CURRENT_USER_ID);
  const likeStyle = isLiked ? 'color:#00e85a;font-weight:700' : 'color:#888';
  const indent = isReply ? 'margin-left:2rem;border-left:2px solid #1e1e1e;padding-left:.75rem;' : '';
  const replies = c.replies || [];

  const delBtn = !isReply && (isOwn
    ? '<button class="post-delete-btn" onclick="deleteComment(\''+c.id+'\',this)" title="Supprimer" style="margin-left:auto;align-self:flex-start">&#128465;</button>'
    : (IS_ADMIN ? '<button class="admin-del" onclick="adminDeleteComment(\''+c.id+'\',this)" title="Supprimer (admin)" style="margin-left:auto;align-self:flex-start">✕</button>' : ''));

  const actionBtns = '<div class="cmt-actions">'+
    '<button class="cmt-like-btn" id="cmt-like-'+c.id+'" style="'+likeStyle+'" onclick="toggleCommentLike(\''+c.id+'\',\''+postId+'\')">'+
      '👍 <span id="cmt-like-count-'+c.id+'">'+likeCount+'</span>'+
    '</button>'+
    (!isReply && IS_LOGGED_IN ? '<button class="cmt-reply-btn" onclick="toggleReplyBox(\''+c.id+'\',\''+postId+'\')">↩ '+(LANG==='fr'?'Répondre':'Reply')+'</button>' : '')+
  '</div>';

  const replyBox = !isReply ? '<div id="cmt-reply-box-'+c.id+'" class="cmt-reply-box hidden">'+
    '<div style="flex:1;position:relative;">'+
      '<input type="text" id="cmt-reply-inp-'+c.id+'" class="post-comment-input" placeholder="'+(LANG==='fr'?'Votre réponse...':'Your reply...')+'" maxlength="500" '+
        'onkeydown="if(event.key===\'Enter\')submitCommentReply(\''+c.id+'\',\''+postId+'\')">'+
      '<div class="chat-mentions-list hidden" id="cmt-reply-drop-'+c.id+'" style="bottom:auto;top:100%%;z-index:100"></div>'+
    '</div>'+
    '<button class="post-comment-submit" onclick="submitCommentReply(\''+c.id+'\',\''+postId+'\')">'+I18N['common.send_btn']+'</button>'+
  '</div>' : '';

  const repliesHtml = replies.length
    ? '<div id="cmt-replies-'+c.id+'">'+replies.map(r => renderCommentItem(r, postId, true)).join('')+'</div>'
    : '<div id="cmt-replies-'+c.id+'"></div>';

  return '<div class="comment-item" id="cmt-'+c.id+'" style="'+indent+'">'+
    '<span class="comment-avatar">'+c.avatar_svg+'</span>'+
    '<div class="comment-body" style="flex:1">'+
      '<span class="comment-pseudo">@'+escHtml(c.pseudo)+(c.pseudo==='Yousse'?'<span class="scorbits-badge" title="Scorbits Official"><span>S</span></span>':'')+'</span>'+
      '<span class="comment-age">'+relAge(c.created_at)+'</span>'+
      '<div class="comment-text">'+linkify(c.content)+'</div>'+
      '<button onclick="translateMsg(this.dataset.t,this)" data-t="'+escHtml(c.content||'').replace(/"/g,'&quot;')+'" style="background:none;border:none;color:#444;font-size:.72rem;cursor:pointer;padding:1px 4px;margin-top:2px;display:block;">'+(LANG==='fr'?'Traduire':'Translate')+'</button>'+
      actionBtns+
      replyBox+
      repliesHtml+
    '</div>'+
    delBtn+
  '</div>';
}
async function toggleCommentLike(commentId, postId) {
  if (!IS_LOGGED_IN) { window.location='/wallet'; return; }
  try {
    const res = await fetch('/api/comment/like', {method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({comment_id: commentId})});
    const d = await res.json();
    const btn = document.getElementById('cmt-like-'+commentId);
    const countEl = document.getElementById('cmt-like-count-'+commentId);
    if (btn) btn.style.cssText = d.liked ? 'color:#00e85a;font-weight:700' : 'color:#888';
    if (countEl) countEl.textContent = d.count;
  } catch(e) {}
}
function toggleReplyBox(commentId, postId) {
  const box = document.getElementById('cmt-reply-box-'+commentId);
  if (!box) return;
  const hidden = box.classList.toggle('hidden');
  if (!hidden) {
    const inp = document.getElementById('cmt-reply-inp-'+commentId);
    if (inp) {
      inp.focus();
      setupMentionAC(inp, document.getElementById('cmt-reply-drop-'+commentId));
    }
  }
}
async function submitCommentReply(commentId, postId) {
  const inp = document.getElementById('cmt-reply-inp-'+commentId);
  if (!inp || !inp.value.trim()) return;
  try {
    await fetch('/api/comment/reply', {method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({comment_id: commentId, post_id: postId, content: inp.value.trim()})});
    inp.value = '';
    document.getElementById('cmt-reply-box-'+commentId).classList.add('hidden');
    // Reload comments to show the new reply
    await openCommentsModal(postId);
  } catch(e) {}
}
let _currentCommentsPostId = null;
async function openCommentsModal(postId) {
  _currentCommentsPostId = postId;
  const modal = document.getElementById('comments-modal');
  const list = document.getElementById('comments-list');
  if(!modal||!list) return;
  list.innerHTML='<div style="color:var(--muted);text-align:center;padding:1rem">'+I18N['common.loading']+'</div>';
  modal.classList.remove('hidden');
  try {
    const res = await fetch('/api/posts/comments?post_id='+postId);
    const comments = await res.json() || [];
    if(!comments.length) { list.innerHTML='<div style="color:var(--muted);font-size:0.82rem;text-align:center;padding:0.8rem">'+I18N['common.no_comments']+'</div>'; return; }
    list.innerHTML = comments.map(c => renderCommentItem(c, postId, false)).join('');
  } catch(e) { list.innerHTML='<div style="color:#ff6464;font-size:0.82rem">'+I18N['common.error']+'</div>'; }
}
function closeCommentsModal() {
  document.getElementById('comments-modal').classList.add('hidden');
}

// ── Share post to chat ──
function renderSharedPost(sp) {
  if (!sp) return '';
  const avatarHtml = sp.author_avatar
    ? '<span style="width:20px;height:20px;display:inline-flex;flex-shrink:0;vertical-align:middle">'+sp.author_avatar+'</span>'
    : '<span style="width:20px;height:20px;border-radius:50%%;background:var(--green3);display:inline-block;flex-shrink:0"></span>';
  const imageHtml = sp.image_url
    ? '<img src="'+escHtml(sp.image_url)+'" class="chat-post-card-image" loading="lazy" onerror="this.style.display=\'none\'">'
    : '';
  return '<div class="chat-post-card" onclick="scrollToPost(\''+escHtml(sp.post_id)+'\')">'+
    '<div class="chat-post-card-header">'+avatarHtml+
    '<span class="chat-post-card-author">@'+escHtml(sp.author_pseudo)+'</span>'+
    '<span class="chat-post-card-date">'+escHtml(sp.created_at)+'</span>'+
    '</div>'+imageHtml+
    '<div class="chat-post-card-content">'+linkify(sp.content)+'</div>'+
    '<div class="chat-post-card-footer">'+I18N['chat.click_to_view_post']+'</div>'+
    '</div>';
}
function sharePostToChat(postId, pseudo) {
  pendingSharedPostId = postId;
  const preview = document.getElementById('chat-share-preview');
  if (preview) {
    preview.innerHTML = '<span>📎 '+I18N['chat.sharing_post_from']+'@'+escHtml(pseudo||'')+'</span><button onclick="cancelSharePost()">✕</button>';
    preview.classList.remove('hidden');
  }
  const panel = document.querySelector('.chat-panel');
  if (panel) panel.scrollIntoView({behavior: 'smooth', block: 'end'});
  const inp = document.getElementById('chat-input');
  if (inp) setTimeout(() => inp.focus(), 300);
}
function cancelSharePost() {
  pendingSharedPostId = null;
  const preview = document.getElementById('chat-share-preview');
  if (preview) preview.classList.add('hidden');
}
function scrollToPost(postId) {
  const el = document.getElementById('post-' + postId);
  if (el) {
    el.scrollIntoView({behavior: 'smooth', block: 'center'});
    const prev = el.style.borderColor;
    el.style.transition = 'border-color 0.3s';
    el.style.borderColor = '#00e85a';
    setTimeout(() => { el.style.borderColor = prev; }, 2000);
  } else {
    window.location.href = '/#posts';
  }
}

function escHtml(t) {
  return t.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}
function linkify(text) {
  if (!text) return '';
  text = text.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
  text = text.replace(/(https?:\/\/[^\s<>"]+|(?<![/@])\b(?:[a-zA-Z0-9-]+\.)+(?:com|net|org|io|fr|be|co|uk|ca|dev|app|xyz|info|me|tv|gg)[^\s<>"]*)/gi, function(url) {
    const href = url.startsWith('http') ? url : 'https://' + url;
    return '<a href="' + href + '" target="_blank" rel="noopener noreferrer" style="color:#00e85a;text-decoration:underline;word-break:break-all;">' + url + '</a>';
  });
  text = text.replace(/@(\w+)/g, '<a class="mention-link" onclick="showProfile(\'$1\')">@$1</a>');
  return text;
}
console.log('linkify test:', linkify('scorbits.com'));

// ── Profil popup ──
async function showProfile(pseudo) {
  const res = await fetch('/api/profile/' + pseudo);
  const p = await res.json();
  if (p.error) return;
  const modal = document.getElementById('profile-modal');
  const up = p.change_24h >= 0;
  document.getElementById('pm-content').innerHTML = `+"`"+`
    <div class="pm-header">
      <div class="pm-avatar">${p.avatar_svg}</div>
      <div class="pm-info">
        <div class="pm-pseudo">@${p.pseudo}</div>
        <div class="pm-addr mono sm muted">${p.address ? p.address.substring(0,20)+'...' : ''}</div>
        <div class="pm-since muted sm">`+"`"+`+I18N['common.since']+`+"`"+` ${p.since}</div>
      </div>
    </div>
    <div class="pm-stats">
      <div class="pm-stat"><div class="pm-stat-val green">${p.blocks_mined}</div><div class="pm-stat-label">`+"`"+`+I18N['common.blocks_mined']+`+"`"+`</div></div>
      <div class="pm-stat"><div class="pm-stat-val">${p.balance}</div><div class="pm-stat-label">SCO</div></div>
      <div class="pm-stat"><div class="pm-stat-val">${p.days_active}`+"`"+`+(I18N['common.ago_d']||'d')+`+"`"+`</div><div class="pm-stat-label">`+"`"+`+I18N['common.since']+`+"`"+`</div></div>
    </div>
    <div class="pm-badges">${(p.badges||[]).map(b=>'<span class="pm-badge">'+b+'</span>').join('')}</div>
  `+"`"+`;
  modal.classList.remove('hidden');
}

function closeProfile() { document.getElementById('profile-modal').classList.add('hidden'); }

// ── Traduction MyMemory ──
async function translateMsg(text, btn) {
  const targetLang = document.documentElement.lang === 'fr' ? 'fr' : 'en';
  btn.textContent = '...';
  btn.disabled = true;
  try {
    const url = 'https://api.mymemory.translated.net/get?q=' + encodeURIComponent(text) + '&langpair=autodetect|' + targetLang;
    const r = await fetch(url);
    const d = await r.json();
    if (d.responseStatus === 200 && d.responseData && d.responseData.translatedText) {
      const box = document.createElement('div');
      box.style.cssText = 'font-size:.78rem;color:#888;margin-top:5px;font-style:italic;border-left:2px solid #2a2a2a;padding-left:6px;line-height:1.5;';
      box.textContent = d.responseData.translatedText;
      btn.parentNode.insertBefore(box, btn);
      btn.remove();
    } else {
      btn.textContent = targetLang === 'fr' ? 'Traduire' : 'Translate';
      btn.disabled = false;
    }
  } catch(e) {
    btn.textContent = targetLang === 'fr' ? 'Traduire' : 'Translate';
    btn.disabled = false;
  }
}
window.translateMsg = translateMsg;

// ── Bloc en cours ──
let _bpIdx=%d,_bpTs=%d;
function _bpUpdate(){
  const now=Math.floor(Date.now()/1000);
  const elapsed=Math.max(0,now-_bpTs);
  const el=document.getElementById('bp-elapsed');
  if(el){
    const em=Math.floor(elapsed/60),es=elapsed%%60;
    const timeStr=em>0?(em+'m '+es+'s'):(es+'s');
    const label=LANG==='fr'?('Dernier bloc il y a\u00a0: '+timeStr):('Last block: '+timeStr+' ago');
    el.textContent=label;
    el.style.color=elapsed>600?'var(--red, #e84040)':'var(--text2)';
  }
}
async function _bpRefresh(){
  try{
    const d=await fetch('/api/stats').then(r=>r.json());
    if(d.last_block!==_bpIdx){
      _bpIdx=d.last_block;_bpTs=d.last_block_timestamp;
      const n=document.getElementById('bp-next'),df=document.getElementById('bp-diff');
      if(n)n.textContent='#'+(d.last_block+1);if(df)df.textContent=d.difficulty;
    }
    _bpUpdate();
  }catch(e){}
}
_bpUpdate();setInterval(_bpUpdate,1000);setInterval(_bpRefresh,5000);

// ── Init ──
loadActivity();
loadLeaderboard();
loadPosts();
setInterval(loadActivity, 30000);
setInterval(loadPosts, 30000);

// ── Active miners ──
async function loadActiveMiners() {
  try {
    const res = await fetch('/api/active-miners');
    const d = await res.json();
    const el = document.getElementById('home-active-miners');
    if (el) el.textContent = d.active_miners;
  } catch(e) {}
}
loadActiveMiners();
setInterval(loadActiveMiners, 60000);

// ── Announcements ──
let annEditId = null;
let annCoverURL = '';
async function loadAnnouncements() {
  const res = await fetch('/api/announcements');
  const list = await res.json() || [];
  const el = document.getElementById('announcements-list');
  if (!el) return;
  if (!list.length) { el.innerHTML = '<div style="color:#5a7a9a;font-size:.85rem;padding:.5rem 0">Aucune annonce pour le moment.</div>'; return; }
  el.innerHTML = list.map(a => {
    const d = new Date(a.created_at);
    const dateStr = d.toLocaleDateString(undefined, {year:'numeric',month:'short',day:'numeric'});
    const adminBtns = IS_ADMIN ? `+"`"+`<div style="display:flex;gap:.5rem;margin-top:.75rem">
        <button onclick="editAnn('${a.id}','${escHtml(a.title)}',${JSON.stringify(a.content)},'${a.media_url||''}')" style="background:#132035;border:1px solid #1e90ff44;border-radius:4px;padding:3px 10px;color:#7ab8e0;cursor:pointer;font-size:.78rem">Modifier</button>
        <button onclick="deleteAnn('${a.id}')" style="background:#132035;border:1px solid #e5533344;border-radius:4px;padding:3px 10px;color:#e55;cursor:pointer;font-size:.78rem">Supprimer</button>
      </div>`+"`"+` : '';
    const coverImg = a.media_url ? `+"`"+`<img class="ann-cover-img" src="${a.media_url}" alt="">`+"`"+` : '';
    return `+"`"+`<div class="ann-card">
      <div class="ann-card-header">
        <span class="ann-official-badge">📢 ANNONCE OFFICIELLE</span>
        <span class="ann-card-date">${dateStr}</span>
      </div>
      <div class="ann-title">${escHtml(a.title)}</div>
      <div class="ann-content">${a.content}</div>
      ${coverImg}
      ${adminBtns}
    </div>`+"`"+`;
  }).join('');
}
function _annResetCoverUI() {
  annCoverURL = '';
  const prev = document.getElementById('ann-cover-preview');
  const name = document.getElementById('ann-cover-name');
  const btn  = document.getElementById('ann-cover-remove');
  const inp  = document.getElementById('ann-cover-input');
  if (prev) prev.innerHTML = '';
  if (name) name.textContent = '';
  if (btn)  btn.style.display = 'none';
  if (inp)  inp.value = '';
}
function openAnnModal() {
  annEditId = null;
  document.getElementById('ann-title').value = '';
  document.getElementById('ann-editor').innerHTML = '';
  document.getElementById('ann-preview').innerHTML = '';
  _annResetCoverUI();
  document.getElementById('ann-modal').classList.remove('hidden');
}
function closeAnnModal() {
  document.getElementById('ann-modal').classList.add('hidden');
  annEditId = null;
}
function editAnn(id, title, content, mediaUrl) {
  annEditId = id;
  annCoverURL = mediaUrl || '';
  document.getElementById('ann-title').value = title;
  document.getElementById('ann-editor').innerHTML = content;
  annUpdatePreview();
  const prev = document.getElementById('ann-cover-preview');
  const name = document.getElementById('ann-cover-name');
  const btn  = document.getElementById('ann-cover-remove');
  if (annCoverURL) {
    if (prev) prev.innerHTML = '<img src="'+annCoverURL+'" style="max-height:80px;border-radius:5px;margin-top:2px">';
    if (name) name.textContent = 'Image jointe';
    if (btn)  btn.style.display = 'inline';
  } else {
    if (prev) prev.innerHTML = '';
    if (name) name.textContent = '';
    if (btn)  btn.style.display = 'none';
  }
  document.getElementById('ann-modal').classList.remove('hidden');
}
function annRemoveCover() { _annResetCoverUI(); }
async function annUploadCover(input) {
  if (!input.files || !input.files[0]) return;
  const fd = new FormData();
  fd.append('image', input.files[0]);
  const name = document.getElementById('ann-cover-name');
  const prev = document.getElementById('ann-cover-preview');
  const btn  = document.getElementById('ann-cover-remove');
  if (name) name.textContent = 'Envoi en cours…';
  try {
    const res = await fetch('/api/announcements/upload', {method:'POST', body:fd});
    const data = await res.json();
    if (data.url) {
      annCoverURL = data.url;
      if (prev) prev.innerHTML = '<img src="'+data.url+'" style="max-height:80px;border-radius:5px;margin-top:2px">';
      if (name) name.textContent = input.files[0].name;
      if (btn)  btn.style.display = 'inline';
    } else {
      if (name) name.textContent = 'Erreur upload';
    }
  } catch(e) { if (name) name.textContent = 'Erreur réseau'; }
  input.value = '';
}
function annCmd(cmd) {
  document.getElementById('ann-editor').focus();
  document.execCommand(cmd, false, null);
  annUpdatePreview();
}
function annInsertLink() {
  const url = prompt('Enter URL:');
  if (!url) return;
  document.getElementById('ann-editor').focus();
  document.execCommand('createLink', false, url);
  annUpdatePreview();
}
async function annUploadImg(input) {
  if (!input.files || !input.files[0]) return;
  const fd = new FormData();
  fd.append('image', input.files[0]);
  try {
    const res = await fetch('/api/announcements/upload', {method:'POST', body:fd});
    const data = await res.json();
    if (data.url) {
      document.getElementById('ann-editor').focus();
      document.execCommand('insertImage', false, data.url);
      annUpdatePreview();
    } else {
      alert('Upload failed');
    }
  } catch(e) { alert('Upload error'); }
  input.value = '';
}
function annUpdatePreview() {
  const html = document.getElementById('ann-editor').innerHTML;
  document.getElementById('ann-preview').innerHTML = html;
}
async function submitAnn() {
  const title = document.getElementById('ann-title').value.trim();
  const content = document.getElementById('ann-editor').innerHTML.trim();
  if (!title || !content || content === '<br>') { alert('Title and content required'); return; }
  const url = annEditId ? `+"`"+`/api/announcements/${annEditId}`+"`"+` : '/api/announcements';
  const method = annEditId ? 'PUT' : 'POST';
  await fetch(url, {method, headers:{'Content-Type':'application/json'}, body:JSON.stringify({title, content, media_url: annCoverURL})});
  closeAnnModal();
  loadAnnouncements();
}
async function deleteAnn(id) {
  if (!confirm('Delete this announcement?')) return;
  await fetch(`+"`"+`/api/announcements/${id}`+"`"+`, {method:'DELETE'});
  loadAnnouncements();
}
loadAnnouncements();
window.openAnnModal=openAnnModal; window.closeAnnModal=closeAnnModal; window.editAnn=editAnn; window.submitAnn=submitAnn; window.deleteAnn=deleteAnn;
window.annCmd=annCmd; window.annInsertLink=annInsertLink; window.annUploadImg=annUploadImg; window.annUpdatePreview=annUpdatePreview;
window.annUploadCover=annUploadCover; window.annRemoveCover=annRemoveCover;

// ── Expose all chat onclick functions to global scope ──
window.sendChatMsg        = sendChatMsg;
window.toggleEmojiPicker  = toggleEmojiPicker;
window.toggleGifPicker    = toggleGifPicker;
window.openReactPicker    = openReactPicker;
window.toggleReaction     = toggleReaction;
window.openChatMenu       = openChatMenu;
window.openReactions      = openReactPicker;
window.openMsgMenu        = openChatMenu;
window.sendReaction       = toggleReaction;
window.blockChatUser      = blockChatUser;
window.muteChatUser       = muteChatUser;
window.insertMention      = insertMention;
window.searchGifs         = searchGifs;
window.selectGif          = selectGif;
window.loadGifs           = loadGifs;
window.updateReactionsDOM = updateReactionsDOM;
window.escHtml            = escHtml;
window.showProfile        = showProfile;
window.reactPost          = reactPost;
window.deletePost         = deletePost;
window.deleteComment        = deleteComment;
window.adminDeletePost      = adminDeletePost;
window.adminDeleteComment   = adminDeleteComment;
window.adminDeleteChatMsg   = adminDeleteChatMsg;
window.toggleCommentBox     = toggleCommentBox;
window.submitComment        = submitComment;
window.openCommentsModal    = openCommentsModal;
window.closeCommentsModal   = closeCommentsModal;
window.toggleCommentLike    = toggleCommentLike;
window.toggleReplyBox       = toggleReplyBox;
window.submitCommentReply   = submitCommentReply;
window.renderSharedPost   = renderSharedPost;
window.sharePostToChat    = sharePostToChat;
window.cancelSharePost    = cancelSharePost;
window.scrollToPost       = scrollToPost;
window.loadPosts          = loadPosts;
window.renderChatAvatar   = renderChatAvatar;
window.renderTextLinks    = renderTextLinks;
window.positionEmojiPicker = positionEmojiPicker;
window.buildEmojiPicker   = buildEmojiPicker;
window.epkScrollTo        = epkScrollTo;
window.epkFilter          = epkFilter;
window.onEmojiSelect      = onEmojiSelect;
</script>
<script>document.addEventListener('DOMContentLoaded',function(){if(window.updateHalvingBar)window.updateHalvingBar(%d);});</script>`,
		totalBlocks, totalSupply, last.Difficulty,
		time.Unix(last.Timestamp, 0).Format("02/01/2006 15:04"),
		last.Index+1, last.Difficulty,
		rows,
		last.Index, last.Timestamp, last.Index)
}

func (e *Explorer) handleBlock(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.Redirect(w, r, "/", 302)
		return
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil || idx < 0 || idx >= len(e.bc.Blocks) {
		http.Error(w, i18n.T(lang, "block.not_found"), 404)
		return
	}
	b := e.bc.Blocks[idx]
	confirmations := len(e.bc.Blocks) - 1 - idx
	txRows := ""
	for _, tx := range b.Transactions {
		txRows += fmt.Sprintf(`<tr><td class="mono sm wrap">%s</td></tr>`, tx)
	}
	prevLink, nextLink := "", ""
	if idx > 0 {
		prevLink = fmt.Sprintf(`<a href="/block/%d" class="navbtn">← %s #%d</a>`, idx-1, i18n.T(lang, "home.col_block"), idx-1)
	}
	if idx < len(e.bc.Blocks)-1 {
		nextLink = fmt.Sprintf(`<a href="/block/%d" class="navbtn">%s #%d →</a>`, idx+1, i18n.T(lang, "home.col_block"), idx+1)
	}
	minerDisplay := b.MinerAddress
	u, err2 := db.GetUserByAddress(b.MinerAddress)
	if err2 == nil {
		minerDisplay = fmt.Sprintf(`<span class="pseudo-link" onclick="showProfile('%s')">@%s</span> <span class="muted sm">(%s)</span>`, u.Pseudo, u.Pseudo, b.MinerAddress)
	}
	confClass := "muted"
	confLabel := fmt.Sprintf("%d confirmation(s)", confirmations)
	if confirmations >= 6 {
		confClass = "green"
		confLabel = fmt.Sprintf("%d %s", confirmations, i18n.T(lang, "block.final"))
	}
	content := fmt.Sprintf(`
	<div class="bnav">%s %s</div>
	<div class="stitle">%s #%d</div>
	<div class="dbox">
		<div class="drow"><span class="dlabel">%s</span><span class="mono green wrap">%s</span></div>
		<div class="drow"><span class="dlabel">%s</span><span class="mono muted wrap">%s</span></div>
		<div class="drow"><span class="dlabel">%s</span><span>%s</span></div>
		<div class="drow"><span class="dlabel">%s</span><span class="%s">%s</span></div>
		<div class="drow"><span class="dlabel">%s</span><span class="green">%d</span></div>
		<div class="drow"><span class="dlabel">%s</span><span class="muted">%d</span></div>
		<div class="drow"><span class="dlabel">%s</span><span class="big-reward">%d SCO</span></div>
		<div class="drow"><span class="dlabel">%s</span><span>%s</span></div>
	</div>
	<div class="confirm-tip">
		<svg width="14" height="14" viewBox="0 0 14 14"><circle cx="7" cy="7" r="6" fill="none" stroke="currentColor" stroke-width="1.5"/><line x1="7" y1="5" x2="7" y2="9" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/><circle cx="7" cy="11" r="0.8" fill="currentColor"/></svg>
		%s
	</div>
	<div class="stitle">%s (%d)</div>
	<div class="tbox"><table><thead><tr><th>%s</th></tr></thead><tbody>%s</tbody></table></div>`,
		prevLink, nextLink,
		i18n.T(lang, "home.col_block"), b.Index,
		i18n.T(lang, "block.hash"), b.Hash,
		i18n.T(lang, "block.prev_hash"), b.PreviousHash,
		i18n.T(lang, "block.timestamp"), time.Unix(b.Timestamp, 0).Format("02/01/2006 15:04:05"),
		i18n.T(lang, "block.confirmations"), confClass, confLabel,
		i18n.T(lang, "block.difficulty"), b.Difficulty,
		i18n.T(lang, "block.nonce"), b.Nonce,
		i18n.T(lang, "block.reward"), b.Reward,
		i18n.T(lang, "block.miner"), minerDisplay,
		i18n.T(lang, "block.confirmation_tip"),
		i18n.T(lang, "block.transactions"), len(b.Transactions),
		i18n.T(lang, "block.data"), txRows)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(fmt.Sprintf("%s #%d", i18n.T(lang, "home.col_block"), b.Index), content, "explorer", lang, user))
}

func (e *Explorer) handleAddress(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.Redirect(w, r, "/", 302)
		return
	}
	address := parts[2]
	balance := e.bc.GetBalance(address)
	pseudoLine := ""
	u, err := db.GetUserByAddress(address)
	if err == nil {
		pseudoLine = fmt.Sprintf(`<div class="drow"><span class="dlabel">%s</span><span class="green fw6 pseudo-link" onclick="showProfile('%s')">@%s</span></div>`, i18n.T(lang, "address.pseudo"), u.Pseudo, u.Pseudo)
	}
	rows := ""
	for _, b := range e.bc.Blocks {
		if b.MinerAddress == address {
			rows += fmt.Sprintf(`<tr onclick="window.location='/block/%d'">
				<td><span class="badge">#%d</span></td>
				<td class="sm muted">%s</td>
				<td><span class="green fw6">%d SCO</span></td>
			</tr>`, b.Index, b.Index,
				time.Unix(b.Timestamp, 0).Format("02/01/2006 15:04:05"), b.Reward)
		}
	}
	content := fmt.Sprintf(`
	<div class="stitle">%s</div>
	<div class="dbox">
		<div class="drow"><span class="dlabel">%s</span><span class="mono green wrap">%s</span></div>
		%s
		<div class="drow"><span class="dlabel">%s</span><span class="bigbal">%d SCO</span></div>
	</div>
	<div class="stitle">%s</div>
	<div class="tbox"><table>
		<thead><tr><th>%s</th><th>%s</th><th>%s</th></tr></thead>
		<tbody>%s</tbody>
	</table></div>`,
		i18n.T(lang, "address.title"),
		i18n.T(lang, "address.sco"), address,
		pseudoLine,
		i18n.T(lang, "address.balance"), balance,
		i18n.T(lang, "address.mined_blocks"),
		i18n.T(lang, "home.col_block"), i18n.T(lang, "home.col_time"), i18n.T(lang, "home.col_reward"),
		rows)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "address.title"), content, "explorer", lang, user))
}

func (e *Explorer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/", 302)
		return
	}
	if idx, err := strconv.Atoi(q); err == nil {
		http.Redirect(w, r, fmt.Sprintf("/block/%d", idx), 302)
		return
	}
	if strings.HasPrefix(q, "SCO") {
		http.Redirect(w, r, "/address/"+q, 302)
		return
	}
	if strings.HasPrefix(q, "@") {
		u, err := db.GetUserByPseudo(q[1:])
		if err == nil {
			http.Redirect(w, r, "/address/"+u.Address, 302)
			return
		}
	}
	for _, b := range e.bc.Blocks {
		if b.Hash == q {
			http.Redirect(w, r, fmt.Sprintf("/block/%d", b.Index), 302)
			return
		}
	}
	// Aucun résultat — retour silencieux à l'accueil
	http.Redirect(w, r, "/", 302)
}

// ─── ACTIVE MINERS ────────────────────────────────────────────────────────────

func (e *Explorer) getActiveMiners() int {
	cutoff := time.Now().Unix() - 300
	e.activeMinersMu.Lock()
	defer e.activeMinersMu.Unlock()
	count := 0
	for _, ts := range e.activeMiners {
		if ts >= cutoff {
			count++
		}
	}
	return count
}

func (e *Explorer) apiActiveMiners(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"active_miners": e.getActiveMiners()})
}

// ─── API PUBLIQUE ─────────────────────────────────────────────────────────────

func (e *Explorer) apiStats(w http.ResponseWriter, r *http.Request) {
	last := e.bc.GetLastBlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"blocks": len(e.bc.Blocks), "total_supply": e.bc.TotalSupply,
		"max_supply": blockchain.MaxSupply, "difficulty": e.bc.Difficulty,
		"last_block": last.Index, "last_block_timestamp": last.Timestamp, "valid": e.bc.IsValid(),
		"active_miners": e.getActiveMiners(),
	})
}

func (e *Explorer) apiBlocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(e.bc.Blocks)
}

func (e *Explorer) apiBlock(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "index manquant", 400)
		return
	}
	idx, err := strconv.Atoi(parts[3])
	if err != nil || idx < 0 || idx >= len(e.bc.Blocks) {
		http.Error(w, "bloc introuvable", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(e.bc.Blocks[idx])
}

func (e *Explorer) apiAddress(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "adresse manquante", 400)
		return
	}
	address := parts[3]
	pseudo := ""
	u, err := db.GetUserByAddress(address)
	if err == nil {
		pseudo = u.Pseudo
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"address": address, "balance": e.bc.GetBalance(address), "pseudo": pseudo,
	})
}

func (e *Explorer) apiHistory(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "adresse manquante", 400)
		return
	}
	address := parts[3]
	type TxEntry struct {
		Block     int    `json:"block"`
		Type      string `json:"type"`
		Amount    int    `json:"amount"`
		Timestamp int64  `json:"timestamp"`
		Hash      string `json:"hash"`
	}
	var entries []TxEntry
	for _, b := range e.bc.Blocks {
		if b.MinerAddress == address {
			entries = append(entries, TxEntry{Block: b.Index, Type: "minage", Amount: b.Reward, Timestamp: b.Timestamp, Hash: b.Hash})
		}
		for _, tx := range b.Transactions {
			if strings.HasPrefix(tx, "PREMINE:") {
				parts := strings.Split(tx, ":")
				if len(parts) == 3 && parts[1] == address {
					if amount, err := strconv.Atoi(parts[2]); err == nil {
						entries = append(entries, TxEntry{Block: b.Index, Type: "premine", Amount: amount, Timestamp: b.Timestamp, Hash: b.Hash})
					}
				}
				continue
			}
			if strings.Contains(tx, address) {
				t := "réception"
				if strings.HasPrefix(tx, address+"->") {
					t = "envoi"
				}
				// Format: FROM->TO:AMOUNTsSCO:feeX:treasuryY — parser le montant
				txAmount := 0
				txParts := strings.Split(tx, ":")
				if len(txParts) >= 2 {
					if v, err := strconv.Atoi(strings.TrimSuffix(txParts[1], "SCO")); err == nil {
						txAmount = v
					}
				}
				entries = append(entries, TxEntry{Block: b.Index, Type: t, Amount: txAmount, Timestamp: b.Timestamp, Hash: b.Hash})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(entries)
}

// ─── CHAT API ─────────────────────────────────────────────────────────────────

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (e *Explorer) apiTransactions(w http.ResponseWriter, r *http.Request) {
	type TxEntry struct {
		Block     int    `json:"block"`
		From      string `json:"from"`
		To        string `json:"to"`
		Amount    int    `json:"amount"`
		Fee       int    `json:"fee"`
		Timestamp int64  `json:"timestamp"`
		Hash      string `json:"hash"`
	}
	var entries []TxEntry
	for _, b := range e.bc.Blocks {
		for _, tx := range b.Transactions {
			if !strings.Contains(tx, "->") {
				continue
			}
			parts := strings.SplitN(tx, "->", 2)
			if len(parts) != 2 {
				continue
			}
			from := parts[0]
			rest := strings.SplitN(parts[1], ":", 3)
			if len(rest) < 2 {
				continue
			}
			to := rest[0]
			amountStr := strings.TrimSuffix(rest[1], "SCO")
			amount, _ := strconv.Atoi(amountStr)
			fee := 0
			if len(rest) > 2 && strings.HasPrefix(rest[2], "fee") {
				feeStr := strings.TrimPrefix(rest[2], "fee")
				feeStr = strings.Split(feeStr, ":")[0]
				fee, _ = strconv.Atoi(feeStr)
			}
			entries = append(entries, TxEntry{
				Block: b.Index, From: from, To: to,
				Amount: amount, Fee: fee,
				Timestamp: b.Timestamp, Hash: b.Hash,
			})
		}
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func (e *Explorer) wsChat(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	user, _ := auth.GetSession(r)
	userID := ""
	if user != nil {
		userID = user.ID.Hex()
	}
	client := &ChatClient{
		hub:    e.chatHub,
		conn:   conn,
		send:   make(chan []byte, 64),
		userID: userID,
	}
	e.chatHub.register <- client

	// Send history
	msgs, err := db.GetChatMessages()
	if err == nil {
		// Filter blocked users if logged in
		var blockedIDs []bson.ObjectID
		if user != nil {
			blockedIDs, _ = db.GetBlockedUsers(user.ID)
		}
		blockedSet := make(map[string]bool)
		for _, id := range blockedIDs {
			blockedSet[id.Hex()] = true
		}
		filtered := make([]*db.ChatMessage, 0, len(msgs))
		for _, m := range msgs {
			if !blockedSet[m.UserID.Hex()] {
				filtered = append(filtered, m)
			}
		}
		if filtered == nil {
			filtered = []*db.ChatMessage{}
		}
		data, _ := json.Marshal(map[string]interface{}{"type": "history", "messages": filtered})
		client.send <- data
	}

	go client.writePump()

	// Read pump (keep connection alive, handle close)
	conn.SetReadLimit(wsMaxMsgSize)
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	e.chatHub.unregister <- client
}

func (e *Explorer) apiGetChat(w http.ResponseWriter, r *http.Request) {
	msgs, err := db.GetChatMessages()
	if err != nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	if msgs == nil {
		msgs = []*db.ChatMessage{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

func stripHTML(s string) string {
	result := strings.Builder{}
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
		} else if r == '>' {
			inTag = false
		} else if !inTag {
			result.WriteRune(r)
		}
	}
	return result.String()
}

func (e *Explorer) apiChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		e.apiGetChat(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	// Check mute
	muted, expiresAt, _ := db.IsUserMuted(user.ID)
	if muted {
		http.Error(w, fmt.Sprintf(`{"error":"muted_until","%s":"%s"}`, "until", expiresAt.Format(time.RFC3339)), 403)
		return
	}
	var req struct {
		Content      string `json:"content"`
		GifURL       string `json:"gif_url"`
		SharedPostID string `json:"shared_post_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	content := stripHTML(req.Content)
	if len([]rune(content)) > 5000 {
		content = string([]rune(content)[:5000])
	}
	if content == "" && req.GifURL == "" && req.SharedPostID == "" {
		http.Error(w, `{"error":"empty"}`, 400)
		return
	}
	var sharedPost *db.SharedPostPreview
	if req.SharedPostID != "" {
		postID, pErr := bson.ObjectIDFromHex(req.SharedPostID)
		if pErr == nil {
			post, pErr2 := db.GetPostByID(postID)
			if pErr2 == nil {
				pc := post.Content
				if len([]rune(pc)) > 300 {
					pc = string([]rune(pc)[:300]) + "..."
				}
				sharedPost = &db.SharedPostPreview{
					PostID:       post.ID.Hex(),
					AuthorPseudo: post.Pseudo,
					AuthorAvatar: post.AvatarSVG,
					Content:      pc,
					CreatedAt:    post.CreatedAt.Format("02/01/2006 15:04"),
				}
			}
		}
	}
	isMiner := e.bc.GetBalance(user.Address) > 11000000
	avatarURL := user.ProfilePic
	msg := &db.ChatMessage{
		UserID:     user.ID,
		Pseudo:     user.Pseudo,
		AvatarURL:  avatarURL,
		IsMiner:    isMiner,
		Content:    content,
		GifURL:     req.GifURL,
		SharedPost: sharedPost,
	}
	if err := db.SaveChatMessage(msg); err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	if mentions := extractMentions(content); len(mentions) > 0 {
		go sendMentionNotifs(user, mentions, bson.NilObjectID, content)
	}
	e.chatHub.BroadcastMessage(map[string]interface{}{"type": "message", "data": msg})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiChatReaction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	var req struct {
		MessageID string `json:"message_id"`
		Emoji     string `json:"emoji"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	msgID, err := bson.ObjectIDFromHex(req.MessageID)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, 400)
		return
	}
	// 1 reaction max per user per message — toggle or switch emoji
	existingEmoji, err := db.GetUserReactionOnMessage(msgID, user.ID.Hex())
	if err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	if existingEmoji == req.Emoji {
		// Same emoji → toggle off
		db.RemoveReaction(msgID, req.Emoji, user.ID.Hex())
	} else {
		if existingEmoji != "" {
			// Different emoji → remove old first
			db.RemoveReaction(msgID, existingEmoji, user.ID.Hex())
		}
		// Add new reaction
		db.AddReaction(msgID, req.Emoji, user.ID.Hex())
	}
	// Fetch updated message and broadcast reaction update to all clients
	updated, err := db.GetChatMessageByID(msgID)
	if err == nil && updated != nil {
		e.chatHub.BroadcastMessage(map[string]interface{}{
			"type":       "reaction",
			"message_id": req.MessageID,
			"reactions":  updated.Reactions,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reactions": updated.Reactions})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiChatBlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	var req struct {
		BlockedID string `json:"blocked_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	blockedID, err := bson.ObjectIDFromHex(req.BlockedID)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, 400)
		return
	}
	db.BlockUser(user.ID, blockedID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiChatUnblock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	var req struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	unblockedID, err := bson.ObjectIDFromHex(req.UserID)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, 400)
		return
	}
	db.UnblockUser(user.ID, unblockedID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiProfileBlockedUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	blockedIDs, err := db.GetBlockedUsers(user.ID)
	if err != nil || len(blockedIDs) == 0 {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	type BlockedUser struct {
		ID        string `json:"id"`
		Pseudo    string `json:"pseudo"`
		AvatarSVG string `json:"avatar_svg"`
	}
	result := make([]BlockedUser, 0, len(blockedIDs))
	for _, id := range blockedIDs {
		u, err := db.GetUserByID(id)
		if err != nil || u == nil {
			continue
		}
		result = append(result, BlockedUser{
			ID:        u.ID.Hex(),
			Pseudo:    u.Pseudo,
			AvatarSVG: renderProfilePic(u, 36),
		})
	}
	json.NewEncoder(w).Encode(result)
}

func (e *Explorer) apiChatMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	// Simple admin check: balance > 11M SCO (genesis premine holder)
	if e.bc.GetBalance(user.Address) <= 11000000 {
		http.Error(w, `{"error":"not admin"}`, 403)
		return
	}
	var req struct {
		UserID   string `json:"user_id"`
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	targetID, err := bson.ObjectIDFromHex(req.UserID)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, 400)
		return
	}
	var dur time.Duration
	switch req.Duration {
	case "5m":
		dur = 5 * time.Minute
	case "1h":
		dur = time.Hour
	case "24h":
		dur = 24 * time.Hour
	case "240h":
		dur = 240 * time.Hour
	case "permanent":
		dur = 100 * 365 * 24 * time.Hour
	default:
		http.Error(w, `{"error":"invalid duration"}`, 400)
		return
	}
	db.MuteUser(targetID, user.ID, dur)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiGifSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		query = "trending"
	}
	apiURL := fmt.Sprintf("https://api.giphy.com/v1/gifs/search?api_key="+os.Getenv("GIPHY_API_KEY")+"&q=%s&limit=12&rating=g&lang=en",
		url.QueryEscape(query))
	fmt.Printf("[GIF] Appel Giphy : %s\n", apiURL)
	resp, err := http.Get(apiURL)
	if err != nil {
		fmt.Printf("[GIF] Erreur HTTP : %v\n", err)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[GIF] Erreur lecture body : %v\n", err)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
		return
	}
	fmt.Printf("[GIF] Réponse Giphy status : %d\n", resp.StatusCode)
	preview := body
	if len(preview) > 200 {
		preview = preview[:200]
	}
	fmt.Printf("[GIF] Body (premiers 200 chars) : %s\n", string(preview))
	var giphyResp struct {
		Data []struct {
			Images struct {
				FixedHeight struct {
					URL string `json:"url"`
				} `json:"fixed_height"`
				FixedHeightSmall struct {
					URL string `json:"url"`
				} `json:"fixed_height_small"`
			} `json:"images"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &giphyResp); err != nil {
		fmt.Printf("[GIF] Erreur parsing JSON : %v\n", err)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
		return
	}
	type GifResult struct {
		URL     string `json:"url"`
		Preview string `json:"preview"`
	}
	results := []GifResult{}
	for _, g := range giphyResp.Data {
		gifURL := g.Images.FixedHeight.URL
		prev := g.Images.FixedHeightSmall.URL
		if prev == "" {
			prev = gifURL
		}
		if gifURL != "" {
			results = append(results, GifResult{URL: gifURL, Preview: prev})
		}
	}
	fmt.Printf("[GIF] %d résultats retournés\n", len(results))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (e *Explorer) startChatCleanup() {
	for range time.Tick(time.Hour) {
		if err := db.DeleteOldChatMessages(); err != nil {
			fmt.Printf("[Chat] Erreur nettoyage messages: %v\n", err)
		}
	}
}

// ─── POSTS API ────────────────────────────────────────────────────────────────

func (e *Explorer) apiPosts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		user, err := auth.GetSession(r)
		if err != nil || user == nil {
			http.Error(w, `{"error":"non connecté"}`, 401)
			return
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, `{"error":"contenu invalide"}`, 400)
			return
		}
		content := strings.TrimSpace(r.FormValue("content"))
		gifURL := strings.TrimSpace(r.FormValue("gif_url"))
		if len([]rune(content)) == 0 && gifURL == "" {
			// check image
			if _, _, ferr := r.FormFile("image"); ferr != nil {
				http.Error(w, `{"error":"contenu invalide"}`, 400)
				return
			}
		}
		maxLen := 500
		if user.Pseudo == "Yousse" {
			maxLen = 5000
		}
		if len([]rune(content)) > maxLen {
			http.Error(w, `{"error":"trop long"}`, 400)
			return
		}
		var imageURL string
		if file, hdr, ferr := r.FormFile("image"); ferr == nil {
			defer file.Close()
			_ = hdr
			imgData, ierr := io.ReadAll(file)
			if ierr == nil {
				src, _, ierr2 := image.Decode(bytes.NewReader(imgData))
				if ierr2 == nil {
					const maxW = 1200
					b := src.Bounds()
					w2, h2 := b.Dx(), b.Dy()
					if w2 > maxW {
						h2 = h2 * maxW / w2
						w2 = maxW
					}
					dst := image.NewRGBA(image.Rect(0, 0, w2, h2))
					xdraw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
					fname := fmt.Sprintf("./static/uploads/posts/%s.jpg", bson.NewObjectID().Hex())
					if f, ferr2 := os.Create(fname); ferr2 == nil {
						jpeg.Encode(f, dst, &jpeg.Options{Quality: 85})
						f.Close()
						imageURL = strings.TrimPrefix(fname, ".")
					}
				}
			}
		}
		p := &db.Post{
			UserID:    user.ID,
			Pseudo:    user.Pseudo,
			AvatarSVG: renderProfilePic(user, 36),
			Content:   content,
			ImageURL:  imageURL,
			GifURL:    gifURL,
		}
		if err := db.CreatePost(p); err != nil {
			http.Error(w, `{"error":"erreur serveur"}`, 500)
			return
		}
		if mentions := extractMentions(content); len(mentions) > 0 {
			go sendMentionNotifs(user, mentions, p.ID, content)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "post": p})
		return
	}
	if r.Method == http.MethodDelete {
		user, err := auth.GetSession(r)
		if err != nil || user == nil {
			http.Error(w, `{"error":"non connecté"}`, 401)
			return
		}
		postID, err := bson.ObjectIDFromHex(r.URL.Query().Get("id"))
		if err != nil {
			http.Error(w, `{"error":"id invalide"}`, 400)
			return
		}
		if err := db.DeletePost(postID, user.ID); err != nil {
			http.Error(w, `{"error":"non autorisé"}`, 403)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		return
	}
	var userID bson.ObjectID
	if user, err := auth.GetSession(r); err == nil && user != nil {
		userID = user.ID
	}
	posts, err := db.GetPosts(50, userID)
	if err != nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	if posts == nil {
		posts = []*db.Post{}
	}
	type postWithCount struct {
		*db.Post
		CommentsCount int64 `json:"comments_count"`
	}
	result := make([]postWithCount, len(posts))
	for i, p := range posts {
		count, _ := db.CountPostComments(p.ID)
		result[i] = postWithCount{Post: p, CommentsCount: count}
	}
	json.NewEncoder(w).Encode(result)
}

func (e *Explorer) apiPostsReact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"méthode invalide"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	var req struct {
		PostID   string `json:"post_id"`
		Reaction string `json:"reaction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"requête invalide"}`, 400)
		return
	}
	if req.Reaction != "like" && req.Reaction != "dislike" {
		http.Error(w, `{"error":"reaction invalide"}`, 400)
		return
	}
	postID, err := bson.ObjectIDFromHex(req.PostID)
	if err != nil {
		http.Error(w, `{"error":"id invalide"}`, 400)
		return
	}
	post, activeReaction, err := db.TogglePostReaction(postID, user.ID, req.Reaction)
	if err != nil {
		http.Error(w, `{"error":"erreur serveur"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"likes": post.Likes, "dislikes": post.Dislikes, "user_reaction": activeReaction,
	})
}

func (e *Explorer) apiPostsComments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		user, err := auth.GetSession(r)
		if err != nil || user == nil {
			http.Error(w, `{"error":"non connecté"}`, 401)
			return
		}
		var req struct {
			PostID  string `json:"post_id"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" {
			http.Error(w, `{"error":"contenu invalide"}`, 400)
			return
		}
		postID, err := bson.ObjectIDFromHex(req.PostID)
		if err != nil {
			http.Error(w, `{"error":"id invalide"}`, 400)
			return
		}
		c := &db.Comment{
			PostID:    postID,
			UserID:    user.ID,
			Pseudo:    user.Pseudo,
			AvatarSVG: renderProfilePic(user, 24),
			Content:   req.Content,
		}
		if err := db.AddComment(c); err != nil {
			http.Error(w, `{"error":"erreur serveur"}`, 500)
			return
		}
		if mentions := extractMentions(req.Content); len(mentions) > 0 {
			go sendMentionNotifs(user, mentions, postID, req.Content)
		}
		// Notify post owner asynchronously (skip if commenter == owner)
		go func(commenterUser *db.User, pid bson.ObjectID, content string) {
			post, err2 := db.GetPostByID(pid)
			if err2 != nil {
				fmt.Printf("[NOTIF] GetPostByID(%s) erreur: %v\n", pid.Hex(), err2)
				return
			}
			if post.UserID == commenterUser.ID {
				fmt.Printf("[NOTIF] Commentaire sur sa propre publication, pas de notif\n")
				return
			}
			fmt.Printf("[NOTIF] Création notif pour owner userID=%s address=%s (commentaire de %s)\n", post.UserID.Hex(), post.Pseudo, commenterUser.Pseudo)
			preview := content
			if len([]rune(preview)) > 80 {
				preview = string([]rune(preview)[:80])
			}
			err3 := db.CreateNotification(&db.Notification{
				UserID:         post.UserID,
				FromID:         commenterUser.ID,
				FromPseudo:     commenterUser.Pseudo,
				FromAvatarSVG:  renderProfilePic(commenterUser, 28),
				PostID:         pid,
				CommentPreview: preview,
				Read:           false,
			})
			fmt.Printf("[NOTIF] InsertOne résultat: err=%v\n", err3)
		}(user, postID, req.Content)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		return
	}
	if r.Method == http.MethodDelete {
		user, err := auth.GetSession(r)
		if err != nil || user == nil {
			http.Error(w, `{"error":"non connecté"}`, 401)
			return
		}
		commentID, err := bson.ObjectIDFromHex(r.URL.Query().Get("id"))
		if err != nil {
			http.Error(w, `{"error":"id invalide"}`, 400)
			return
		}
		if err := db.DeleteComment(commentID, user.ID); err != nil {
			http.Error(w, `{"error":"non autorisé"}`, 403)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		return
	}
	postIDStr := r.URL.Query().Get("post_id")
	postID, err := bson.ObjectIDFromHex(postIDStr)
	if err != nil {
		http.Error(w, `{"error":"id invalide"}`, 400)
		return
	}
	comments, err := db.GetComments(postID)
	if err != nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	if comments == nil {
		comments = []*db.Comment{}
	}
	json.NewEncoder(w).Encode(comments)
}

func (e *Explorer) apiNews(w http.ResponseWriter, r *http.Request) {
	newsCacheMu.RLock()
	cached := newsCache
	newsCacheMu.RUnlock()
	if cached.data != nil && time.Since(cached.cachedAt) < 15*time.Minute {
		w.Header().Set("Content-Type", "application/json")
		w.Write(cached.data)
		return
	}
	resp, err := http.Get("https://min-api.cryptocompare.com/data/v2/news/?lang=EN&sortOrder=latest")
	if err != nil {
		if cached.data != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Write(cached.data)
			return
		}
		jsonError(w, "Erreur API news", 500)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	newsCacheMu.Lock()
	newsCache = newsCacheEntry{data: body, cachedAt: time.Now()}
	newsCacheMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (e *Explorer) apiNewsReact(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}
	_, err := auth.GetSession(r)
	if err != nil {
		jsonError(w, "Non authentifié", 401)
		return
	}
	var req struct {
		URL      string `json:"url"`
		Reaction string `json:"reaction"` // "like" or "dislike"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	if req.URL == "" || (req.Reaction != "like" && req.Reaction != "dislike") {
		jsonError(w, "Paramètres invalides", 400)
		return
	}
	result, err := db.UpsertArticleReaction(req.URL, req.Reaction)
	if err != nil {
		jsonError(w, "Erreur DB", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (e *Explorer) apiCheckEmail(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("email")))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"available": !db.EmailExists(email)})
}

func (e *Explorer) apiNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	fmt.Printf("[NOTIF] GET /api/notifications — userID=%s pseudo=%s address=%s\n", user.ID.Hex(), user.Pseudo, user.Address)
	notifs, err := db.GetNotifications(user.ID)
	fmt.Printf("[NOTIF] Résultat MongoDB: %d notifications, err=%v\n", len(notifs), err)
	if err != nil || notifs == nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	json.NewEncoder(w).Encode(notifs)
}

func (e *Explorer) apiNotificationsReadAll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	db.MarkAllNotificationsRead(user.ID)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (e *Explorer) apiNotificationsReadOne(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/notifications/read/")
	notifID, err := bson.ObjectIDFromHex(idStr)
	if err != nil {
		http.Error(w, `{"error":"id invalide"}`, 400)
		return
	}
	db.MarkNotificationRead(notifID, user.ID)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (e *Explorer) apiNotificationsPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	sinceStr := r.URL.Query().Get("since")
	var since time.Time
	if sinceStr != "" {
		if ts, err := strconv.ParseInt(sinceStr, 10, 64); err == nil {
			since = time.Unix(ts, 0)
		}
	}
	if since.IsZero() {
		since = time.Now().Add(-10 * time.Second)
	}
	notifs, err := db.GetNotificationsSince(user.ID, since)
	if err != nil || notifs == nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	json.NewEncoder(w).Encode(notifs)
}

func (e *Explorer) apiLeaderboard(w http.ResponseWriter, r *http.Request) {
	type LbEntry struct {
		Pseudo    string `json:"pseudo"`
		Blocks    int    `json:"blocks"`
		Balance   int    `json:"balance"`
		AvatarSVG string `json:"avatar_svg"`
	}
	counts := make(map[string]int)
	for _, b := range e.bc.Blocks {
		if b.MinerAddress != "" && b.MinerAddress != "genesis" {
			counts[b.MinerAddress]++
		}
	}
	var entries []LbEntry
	for addr, cnt := range counts {
		u, err := db.GetUserByAddress(addr)
		if err != nil {
			continue
		}
		entries = append(entries, LbEntry{
			Pseudo:    u.Pseudo,
			Blocks:    cnt,
			Balance:   e.bc.GetBalance(addr),
			AvatarSVG: renderProfilePic(u, 28),
		})
	}
	// Tri par blocs décroissant
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Blocks > entries[i].Blocks {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	if len(entries) > 10 {
		entries = entries[:10]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func (e *Explorer) apiProfile(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	// /api/profile/{pseudo} → parts[3]
	// /api/user/profile/{pseudo} → parts[4]
	pseudo := ""
	if len(parts) >= 5 && parts[2] == "user" {
		pseudo = parts[4]
	} else if len(parts) >= 4 {
		pseudo = parts[3]
	}
	if pseudo == "" {
		jsonError(w, "pseudo manquant", 400)
		return
	}
	u, err := db.GetUserByPseudo(pseudo)
	if err != nil {
		jsonError(w, "Utilisateur introuvable", 404)
		return
	}
	blocksMined := 0
	for _, b := range e.bc.Blocks {
		if b.MinerAddress == u.Address {
			blocksMined++
		}
	}
	daysActive := int(time.Since(u.CreatedAt).Hours() / 24)
	badges := computeBadges(blocksMined, daysActive)
	since := u.CreatedAt.Format("Jan 2006")
	type ProfileResp struct {
		Pseudo      string   `json:"pseudo"`
		Address     string   `json:"address"`
		AvatarSVG   string   `json:"avatar_svg"`
		BlocksMined int      `json:"blocks_mined"`
		Balance     int      `json:"balance"`
		DaysActive  int      `json:"days_active"`
		Since       string   `json:"since"`
		Badges      []string `json:"badges"`
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ProfileResp{
		Pseudo:      u.Pseudo,
		Address:     u.Address,
		AvatarSVG:   renderProfilePic(u, 60),
		BlocksMined: blocksMined,
		Balance:     e.bc.GetBalance(u.Address),
		DaysActive:  daysActive,
		Since:       since,
		Badges:      badges,
	})
}

func computeBadges(blocks, days int) []string {
	var b []string
	if blocks >= 1 {
		b = append(b, "Premier bloc")
	}
	if blocks >= 10 {
		b = append(b, "10 blocs")
	}
	if blocks >= 100 {
		b = append(b, "Centurion")
	}
	if blocks >= 500 {
		b = append(b, "Vétéran")
	}
	if blocks >= 1000 {
		b = append(b, "Légende")
	}
	if days >= 30 {
		b = append(b, "1 mois")
	}
	if days >= 365 {
		b = append(b, "1 an")
	}
	return b
}

func (e *Explorer) apiActivity(w http.ResponseWriter, r *http.Request) {
	type ActivityItem struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Time string `json:"time"`
	}
	var items []ActivityItem
	blocks := e.bc.Blocks
	start := len(blocks) - 50
	if start < 1 {
		start = 1
	}
	for i := len(blocks) - 1; i >= start; i-- {
		b := blocks[i]
		miner := b.MinerAddress
		if len(miner) > 16 {
			miner = miner[:16] + "..."
		}
		u, err := db.GetUserByAddress(b.MinerAddress)
		if err == nil {
			miner = "@" + u.Pseudo
		}
		elapsed := time.Since(time.Unix(b.Timestamp, 0))
		var timeStr string
		if elapsed < time.Minute {
			timeStr = "à l'instant"
		} else if elapsed < time.Hour {
			timeStr = fmt.Sprintf("il y a %dm", int(elapsed.Minutes()))
		} else {
			timeStr = fmt.Sprintf("il y a %dh", int(elapsed.Hours()))
		}
		items = append(items, ActivityItem{
			Type: "block",
			Text: fmt.Sprintf("Bloc #%d trouvé par %s (+%d SCO)", b.Index, miner, b.Reward),
			Time: timeStr,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// ─── API MESSAGERIE ───────────────────────────────────────────────────────────

// ─── API AUTH ─────────────────────────────────────────────────────────────────

func (e *Explorer) apiRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}
	if !e.rlRegister.Allow(clientIP(r)) {
		jsonError(w, "Trop de tentatives, réessayez dans 10 minutes", 429)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Pseudo   string `json:"pseudo"`
		Address  string `json:"address"`
		PubKey   string `json:"pub_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Pseudo = strings.TrimSpace(req.Pseudo)
	if req.Email == "" || req.Password == "" || req.Pseudo == "" || req.Address == "" {
		jsonError(w, "Tous les champs sont requis", 400)
		return
	}
	if len(req.Password) < 8 {
		jsonError(w, "Mot de passe trop court", 400)
		return
	}
	if len(req.Pseudo) < 3 || len(req.Pseudo) > 24 {
		jsonError(w, "Pseudo invalide (3-24 caractères)", 400)
		return
	}
	if !strings.HasPrefix(req.Address, "SCO") {
		jsonError(w, "Adresse SCO invalide", 400)
		return
	}
	if db.EmailExists(req.Email) {
		jsonError(w, "Cet email est déjà utilisé", 409)
		return
	}
	if db.PseudoExists(req.Pseudo) {
		jsonError(w, "Ce pseudo est déjà pris", 409)
		return
	}
	verifyToken := auth.GenerateToken()
	user := &db.User{
		Email:        req.Email,
		PasswordHash: auth.HashPassword(req.Password),
		Pseudo:       req.Pseudo,
		Address:      req.Address,
		PubKey:       req.PubKey,
		Verified:     false,
		VerifyToken:  verifyToken,
	}
	if err := db.CreateUser(user); err != nil {
		jsonError(w, "Erreur lors de la création du compte", 500)
		return
	}
	go func() {
		if err := email.SendVerification(user.Email, user.Pseudo, verifyToken); err != nil {
			fmt.Printf("[Email] Erreur envoi vérification: %v\n", err)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "pending": true,
		"message": "Un email de confirmation a été envoyé à " + req.Email,
	})
}

func (e *Explorer) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}
	if !e.rlLogin.Allow(clientIP(r)) {
		jsonError(w, "Trop de tentatives, réessayez dans 1 minute", 429)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	user, err := db.GetUserByEmail(strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil || user.PasswordHash != auth.HashPassword(req.Password) {
		jsonError(w, "Email ou mot de passe incorrect", 401)
		return
	}
	if !user.Verified {
		jsonError(w, "Veuillez confirmer votre adresse email avant de vous connecter.", 403)
		return
	}
	auth.CreateSession(w, user.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "pseudo": user.Pseudo, "address": user.Address,
	})
}

// ─── API WALLET ───────────────────────────────────────────────────────────────

func (e *Explorer) apiWalletSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil {
		jsonError(w, "Non authentifié", 401)
		return
	}
	if !e.rlSend.Allow(clientIP(r)) {
		jsonError(w, "Trop de tentatives", 429)
		return
	}
	var req struct {
		To     string `json:"to"`
		Amount int    `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	req.To = strings.TrimSpace(req.To)
	if req.To == "" || !strings.HasPrefix(req.To, "SCO") {
		jsonError(w, "Adresse destinataire invalide", 400)
		return
	}
	if req.Amount <= 0 || req.Amount > 10000000 {
		jsonError(w, "Montant invalide", 400)
		return
	}
	if req.To == user.Address {
		jsonError(w, "Vous ne pouvez pas vous envoyer des SCO à vous-même", 400)
		return
	}
	balance := e.bc.GetBalance(user.Address)
	fee := 1
	if req.Amount > 100 {
		fee = 2
	}
	treasury := req.Amount / 1000 // 0.1%
	if balance < req.Amount+fee+treasury {
		jsonError(w, fmt.Sprintf("Solde insuffisant (%d SCO disponible)", balance), 400)
		return
	}
	tx := transaction.NewTransaction(user.Address, req.To, req.Amount, fee)
	if !e.mp.Add(tx) {
		jsonError(w, "Transaction invalide ou doublon", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "tx_id": tx.ID, "amount": req.Amount, "fee": fee, "treasury": tx.Treasury,
	})
}

func (e *Explorer) apiWalletBalance(w http.ResponseWriter, r *http.Request) {
	user, err := auth.GetSession(r)
	if err != nil {
		jsonError(w, "Non authentifié", 401)
		return
	}
	balance := e.bc.GetBalance(user.Address)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"address": user.Address, "balance": balance, "pseudo": user.Pseudo,
		"is_admin": adminUsernames[user.Pseudo],
	})
}

func (e *Explorer) apiUpdateAvatar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(403)
	json.NewEncoder(w).Encode(map[string]string{"error": "Avatar non modifiable"})
}

// ─── PROFILE API ───────────────────────────────────────────────────────────────

func (e *Explorer) apiProfileUploadPic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	if err := r.ParseMultipartForm(5 << 20); err != nil {
		http.Error(w, `{"error":"fichier trop volumineux (max 5 MB)"}`, 413)
		return
	}
	file, hdr, err := r.FormFile("pic")
	if err != nil {
		http.Error(w, `{"error":"fichier manquant"}`, 400)
		return
	}
	defer file.Close()
	ct := hdr.Header.Get("Content-Type")
	if ct != "image/jpeg" && ct != "image/png" && ct != "image/webp" {
		// Fallback: detect by extension
		name := strings.ToLower(hdr.Filename)
		if !strings.HasSuffix(name, ".jpg") && !strings.HasSuffix(name, ".jpeg") && !strings.HasSuffix(name, ".png") && !strings.HasSuffix(name, ".webp") {
			http.Error(w, `{"error":"format non supporté (jpg/png/webp)"}`, 400)
			return
		}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, `{"error":"lecture fichier"}`, 500)
		return
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		http.Error(w, `{"error":"image invalide"}`, 400)
		return
	}
	// Center-crop to square then scale to 256x256
	bounds := src.Bounds()
	w0, h0 := bounds.Dx(), bounds.Dy()
	side := w0
	if h0 < side {
		side = h0
	}
	x0 := (w0 - side) / 2
	y0 := (h0 - side) / 2
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	var cropped image.Image
	if si, ok := src.(subImager); ok {
		cropped = si.SubImage(image.Rect(x0, y0, x0+side, y0+side))
	} else {
		cropped = src
	}
	dst := image.NewRGBA(image.Rect(0, 0, 256, 256))
	xdraw.BiLinear.Scale(dst, dst.Bounds(), cropped, cropped.Bounds(), xdraw.Over, nil)
	// Encode to JPEG
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		http.Error(w, `{"error":"encodage image"}`, 500)
		return
	}
	// Save to disk
	filePath := fmt.Sprintf("./static/uploads/profiles/%s.jpg", user.ID.Hex())
	if err := os.WriteFile(filePath, buf.Bytes(), 0644); err != nil {
		http.Error(w, `{"error":"sauvegarde image"}`, 500)
		return
	}
	picURL := fmt.Sprintf("/static/uploads/profiles/%s.jpg", user.ID.Hex())
	if err := db.UpdateUserProfilePic(user.ID, picURL); err != nil {
		http.Error(w, `{"error":"mise à jour base de données"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"url": picURL})
}

func (e *Explorer) apiProfileDeletePic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"DELETE requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	filePath := fmt.Sprintf("./static/uploads/profiles/%s.jpg", user.ID.Hex())
	os.Remove(filePath)
	if err := db.UpdateUserProfilePic(user.ID, ""); err != nil {
		http.Error(w, `{"error":"mise à jour base de données"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (e *Explorer) apiProfileChangePassword(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	var req struct {
		Current     string `json:"current"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"JSON invalide"}`, 400)
		return
	}
	if req.Current == "" || req.NewPassword == "" {
		http.Error(w, `{"error":"champs manquants"}`, 400)
		return
	}
	if len(req.NewPassword) < 8 {
		http.Error(w, `{"error":"mot de passe trop court (min. 8 caractères)"}`, 400)
		return
	}
	if auth.HashPassword(req.Current) != user.PasswordHash {
		http.Error(w, `{"error":"mot de passe actuel incorrect"}`, 400)
		return
	}
	if err := db.UpdateUserPassword(user.ID, auth.HashPassword(req.NewPassword)); err != nil {
		http.Error(w, `{"error":"erreur mise à jour"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ─── MINE ─────────────────────────────────────────────────────────────────────

// apiMineJob retourne le job de minage pour l'utilisateur connecté.
// Contrairement au GET /api/mine/submit, il inclut miner_address depuis la session.
func (e *Explorer) apiMineJob(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		jsonError(w, "Non connecté", 401)
		return
	}
	last := e.bc.GetLastBlock()
	pending := e.mp.GetPending(50)
	txStrings := make([]string, 0, len(pending))
	for _, tx := range pending {
		txStrings = append(txStrings, tx.ToString())
	}
	if len(txStrings) == 0 {
		txStrings = []string{"wasm-browser-mine"}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"last_index":    last.Index,
		"last_hash":     last.Hash,
		"difficulty":    e.bc.Difficulty,
		"reward":        blockchain.CalculateReward(last.Index + 1),
		"miner_address": user.Address,
		"transactions":  txStrings,
	})
}

func (e *Explorer) apiMineSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		last := e.bc.GetLastBlock()
		// Calcul de la vraie difficulté du prochain bloc (même logique que AddBlock)
		// Inclure les transactions en attente dans le job
		pending := e.mp.GetPending(50)
		txStrings := make([]string, 0, len(pending))
		for _, tx := range pending {
			txStrings = append(txStrings, tx.ToString())
		}
		if len(txStrings) == 0 {
			txStrings = []string{"wasm-browser-mine"}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"last_hash": last.Hash, "last_index": last.Index,
			"difficulty": e.bc.Difficulty, "reward": blockchain.CalculateReward(last.Index + 1),
			"transactions": txStrings,
		})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "GET ou POST requis", 405)
		return
	}
	if !e.rlMine.Allow(clientIP(r)) {
		jsonError(w, "Trop de soumissions", 429)
		return
	}
	var req struct {
		Index        int      `json:"index"`
		Timestamp    int64    `json:"timestamp"`
		PreviousHash string   `json:"previousHash"`
		Hash         string   `json:"hash"`
		Nonce        int      `json:"nonce"`
		MinerAddress string   `json:"minerAddress"`
		Difficulty   int      `json:"difficulty"`
		Transactions []string `json:"transactions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	if req.MinerAddress == "" || !strings.HasPrefix(req.MinerAddress, "SCO") {
		jsonError(w, "Adresse mineur invalide", 400)
		return
	}

	// Validation timestamp : fenêtre ±120s autour du temps serveur
	now := time.Now().Unix()
	if req.Timestamp < now-120 || req.Timestamp > now+120 {
		jsonError(w, "Timestamp invalide", 400)
		return
	}

	// Pré-vérification légère sans lock (évite un scrypt inutile si déjà obsolète)
	e.mineMu.Lock()
	last := e.bc.GetLastBlock()
	if req.Index != last.Index+1 {
		e.mineMu.Unlock()
		jsonError(w, "Index de bloc invalide — un autre bloc a été trouvé avant vous", 409)
		return
	}
	if req.PreviousHash != last.Hash {
		e.mineMu.Unlock()
		jsonError(w, "Hash précédent invalide", 400)
		return
	}
	// Timestamp doit être strictement supérieur au dernier bloc
	if req.Timestamp <= last.Timestamp {
		e.mineMu.Unlock()
		jsonError(w, "Timestamp antérieur ou égal au bloc précédent", 400)
		return
	}
	// Anti-spike basé sur le temps réel du serveur, pas le timestamp du mineur
	if time.Now().Unix()-atomic.LoadInt64(&e.lastBlockAcceptedAt) < 120 {
		e.mineMu.Unlock()
		jsonError(w, "Bloc trop rapide", 400)
		return
	}
	e.mineMu.Unlock()

	// Recalcul SHA256 côté serveur — format identique à blockchain/block.go CalculateHash()
	txData := strings.Join(req.Transactions, ";")
	input := fmt.Sprintf("%d%d%s%s%d%s",
		req.Index, req.Timestamp, txData, req.PreviousHash, req.Nonce, req.MinerAddress)
	hashBytes := sha256.Sum256([]byte(input))
	expectedHash := hex.EncodeToString(hashBytes[:])
	if expectedHash != req.Hash {
		jsonError(w, "Hash invalide", 400)
		return
	}

	// Section critique : re-vérifier l'état sous lock avant d'écrire
	e.mineMu.Lock()
	defer e.mineMu.Unlock()

	last = e.bc.GetLastBlock()
	if req.Index != last.Index+1 {
		jsonError(w, "Index de bloc invalide — un autre bloc a été trouvé avant vous", 409)
		return
	}
	if req.PreviousHash != last.Hash {
		jsonError(w, "Hash précédent invalide", 400)
		return
	}

	target := strings.Repeat("0", e.bc.Difficulty)
	if !strings.HasPrefix(expectedHash, target) {
		jsonError(w, "Difficulté non atteinte", 400)
		return
	}

	// Construire la liste de transactions du bloc avec frais de trésorerie
	blockTxs := make([]string, 0, len(req.Transactions)+10)
	blockTxs = append(blockTxs, req.Transactions...)
	if len(blockTxs) == 0 {
		blockTxs = append(blockTxs, "wasm-browser-mine")
	}
	// Ajouter les entrées de frais de trésorerie (0.1%) pour chaque transaction réelle
	for _, txStr := range req.Transactions {
		if strings.Contains(txStr, "->") && strings.Contains(txStr, "SCO:fee") {
			parts := strings.SplitN(txStr, ":treasury", 2)
			if len(parts) == 2 {
				if treasury, err := strconv.Atoi(parts[1]); err == nil && treasury > 0 {
					blockTxs = append(blockTxs, fmt.Sprintf("TREASURY:%s:%d", blockchain.TreasuryAddress, treasury))
				}
			}
		}
	}

	newBlock := &blockchain.Block{
		Index:        req.Index,
		Timestamp:    req.Timestamp,
		Transactions: blockTxs,
		PreviousHash: req.PreviousHash,
		Hash:         req.Hash,
		Nonce:        req.Nonce,
		Difficulty:   e.bc.Difficulty,
		Reward:       blockchain.CalculateReward(req.Index),
		MinerAddress: req.MinerAddress,
	}
	e.bc.Blocks = append(e.bc.Blocks, newBlock)
	e.bc.TotalSupply += newBlock.Reward
	e.bc.Save()
	atomic.StoreInt64(&e.lastBlockAcceptedAt, time.Now().Unix())

	// Vider les transactions confirmées de la mempool
	e.mp.ClearByStrings(req.Transactions)

	fmt.Printf("[Mine] Bloc #%d trouvé par %s (navigateur)\n", newBlock.Index, req.MinerAddress[:16])
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "block": newBlock.Index, "reward": newBlock.Reward, "hash": newBlock.Hash,
	})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// extractMentions retourne la liste dédupliquée des @usernames trouvés dans text.
func extractMentions(text string) []string {
	seen := make(map[string]bool)
	var result []string
	i := 0
	for i < len(text) {
		if text[i] == '@' {
			j := i + 1
			for j < len(text) && (text[j] == '_' || (text[j] >= 'a' && text[j] <= 'z') || (text[j] >= 'A' && text[j] <= 'Z') || (text[j] >= '0' && text[j] <= '9')) {
				j++
			}
			if j > i+1 {
				name := text[i+1 : j]
				if !seen[name] {
					seen[name] = true
					result = append(result, name)
				}
			}
			i = j
		} else {
			i++
		}
	}
	return result
}

// sendMentionNotifs envoie une notification à chaque utilisateur mentionné.
// ctx = "post" | "comment" | "chat". Exécuté en goroutine.
func sendMentionNotifs(mentioner *db.User, mentions []string, postID bson.ObjectID, preview string) {
	if len(preview) > 80 {
		preview = string([]rune(preview)[:80])
	}
	avatar := renderProfilePic(mentioner, 28)
	for _, username := range mentions {
		if strings.EqualFold(username, mentioner.Pseudo) {
			continue // pas de self-notif
		}
		target, err := db.GetUserByPseudo(username)
		if err != nil || target == nil {
			continue
		}
		db.CreateNotification(&db.Notification{
			UserID:         target.ID,
			FromID:         mentioner.ID,
			FromPseudo:     mentioner.Pseudo,
			FromAvatarSVG:  avatar,
			PostID:         postID,
			CommentPreview: "@mention : " + preview,
			Read:           false,
		})
	}
}

// ─── EXTERNAL MINER API ───────────────────────────────────────────────────────

// GET /mining/work — retourne le travail de minage pour le mineur externe (sans auth)
func (e *Explorer) apiMiningWork(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "GET requis", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Enregistre le mineur actif (par adresse si fournie, sinon par IP)
	minerKey := r.URL.Query().Get("address")
	if minerKey == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		minerKey = host
	}
	e.activeMinersMu.Lock()
	e.activeMiners[minerKey] = time.Now().Unix()
	e.activeMinersMu.Unlock()

	last := e.bc.GetLastBlock()
	pending := e.mp.GetPending(50)
	txStrings := make([]string, 0, len(pending))
	for _, tx := range pending {
		txStrings = append(txStrings, tx.ToString())
	}
	if len(txStrings) == 0 {
		txStrings = []string{"empty-block"}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"block_index":    last.Index + 1,
		"previous_hash":  last.Hash,
		"difficulty":     e.bc.Difficulty,
		"reward":         blockchain.CalculateReward(last.Index + 1),
		"timestamp":      time.Now().Unix(),
		"last_timestamp": last.Timestamp,
		"transactions":   txStrings,
	})
}

// POST /mining/submit — valide et ajoute un bloc miné par le mineur externe
func (e *Explorer) apiMiningSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !e.rlMine.Allow(clientIP(r)) {
		jsonError(w, "Trop de soumissions", 429)
		return
	}

	var req struct {
		BlockIndex   int      `json:"block_index"`
		Nonce        int      `json:"nonce"`
		Hash         string   `json:"hash"`
		MinerAddress string   `json:"miner_address"`
		Timestamp    int64    `json:"timestamp"`
		Transactions []string `json:"transactions"`
		Difficulty   int      `json:"difficulty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	if req.MinerAddress == "" || !strings.HasPrefix(req.MinerAddress, "SCO") {
		jsonError(w, "Adresse mineur invalide", 400)
		return
	}

	// Validation timestamp : fenêtre ±120s autour du temps serveur
	now := time.Now().Unix()
	if req.Timestamp < now-120 || req.Timestamp > now+120 {
		jsonError(w, "Timestamp invalide", 400)
		return
	}

	e.mineMu.Lock()
	defer e.mineMu.Unlock()

	last := e.bc.GetLastBlock()
	if req.BlockIndex != last.Index+1 {
		jsonError(w, "Index invalide — bloc déjà trouvé", 409)
		return
	}
	if req.Timestamp <= last.Timestamp {
		jsonError(w, "Timestamp antérieur ou égal au bloc précédent", 400)
		return
	}
	// Anti-spike basé sur le temps réel du serveur, pas le timestamp du mineur
	if time.Now().Unix()-atomic.LoadInt64(&e.lastBlockAcceptedAt) < 120 {
		jsonError(w, "Bloc trop rapide (anti-spike)", 400)
		return
	}
	// Cooldown par adresse : 2 minutes entre chaque bloc accepté
	e.minerMu.Lock()
	lastSubmit, exists := e.minerLastSubmit[req.MinerAddress]
	if exists && time.Now().Unix()-lastSubmit < 120 {
		e.minerMu.Unlock()
		jsonError(w, "Cooldown : attendez 2 minutes entre chaque bloc", 400)
		return
	}
	e.minerMu.Unlock()
	// Anti-consecutive : refuser si ce mineur a déjà trouvé les 2 derniers blocs
	if e.bc.IsConsecutiveMiner(req.MinerAddress) {
		fmt.Printf("[Anti-consecutive] Bloc refusé pour %s — 2 blocs consécutifs atteints\n", req.MinerAddress)
		jsonError(w, "Consecutive block limit reached — wait for another miner", 400)
		return
	}
	// Recalcul SHA256 — format identique à blockchain/block.go CalculateHash()
	extTxStrings := req.Transactions
	if len(extTxStrings) == 0 {
		extTxStrings = []string{"empty-block"}
	}
	extTxData := strings.Join(extTxStrings, ";")
	inp := fmt.Sprintf("%d%d%s%s%d%s",
		req.BlockIndex, req.Timestamp, extTxData, last.Hash, req.Nonce, req.MinerAddress)
	hh := sha256.Sum256([]byte(inp))
	expectedHash := hex.EncodeToString(hh[:])

	if expectedHash != req.Hash {
		jsonError(w, "Hash invalide", 400)
		return
	}

	if !strings.HasPrefix(req.Hash, strings.Repeat("0", e.bc.Difficulty)) {
		jsonError(w, "Difficulté non atteinte", 400)
		return
	}

	// Utiliser les transactions soumises par le mineur (identiques à celles hashées)
	txStrings := extTxStrings

	newBlock := &blockchain.Block{
		Index:        req.BlockIndex,
		Timestamp:    req.Timestamp,
		Transactions: txStrings,
		PreviousHash: last.Hash,
		Hash:         req.Hash,
		Nonce:        req.Nonce,
		Difficulty:   e.bc.Difficulty,
		Reward:       blockchain.CalculateReward(req.BlockIndex),
		MinerAddress: req.MinerAddress,
	}
	newBlock.AcceptedAt = time.Now().Unix()
	e.bc.Blocks = append(e.bc.Blocks, newBlock)
	e.bc.TotalSupply += newBlock.Reward

	// Ajustement de difficulté basé sur le temps réel serveur (AcceptedAt, pas Timestamp mineur)
	if !e.bc.DifficultyForced && (len(e.bc.Blocks)-1)%blockchain.DifficultyInterval == 0 {
		firstBlock := e.bc.Blocks[len(e.bc.Blocks)-blockchain.DifficultyInterval]
		elapsed := time.Now().Unix() - firstBlock.AcceptedAt
		target := int64(120 * blockchain.DifficultyInterval)
		if elapsed < target {
			e.bc.Difficulty += 1
			if e.bc.Difficulty > blockchain.MaxDifficulty {
				e.bc.Difficulty = blockchain.MaxDifficulty
			}
		} else {
			e.bc.Difficulty -= 1
			if e.bc.Difficulty < blockchain.MinDifficulty {
				e.bc.Difficulty = blockchain.MinDifficulty
			}
		}
	}
	e.bc.Save()
	atomic.StoreInt64(&e.lastBlockAcceptedAt, time.Now().Unix())
	e.minerMu.Lock()
	e.minerLastSubmit[req.MinerAddress] = time.Now().Unix()
	e.minerMu.Unlock()
	e.mp.ClearByStrings(txStrings)

	if e.node != nil {
		go e.node.BroadcastBlock(newBlock)
	}

	fmt.Printf("[Mine] Bloc #%d trouvé par %.16s (mineur externe)\n", newBlock.Index, req.MinerAddress)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "block_index": newBlock.Index, "reward": newBlock.Reward, "hash": newBlock.Hash,
	})
}

// ─── PEERS API ────────────────────────────────────────────────────────────────

// GET /api/peers — liste des pairs connus
func (e *Explorer) apiPeers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	peers := e.node.GetPeers()
	json.NewEncoder(w).Encode(map[string]interface{}{"peers": peers})
}

// POST /api/peers/add — ajoute un pair et se synchronise immédiatement
func (e *Explorer) apiPeersAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Address == "" {
		jsonError(w, "body invalide: champ 'address' requis", http.StatusBadRequest)
		return
	}
	e.node.AddPeer(req.Address)
	// S'annoncer au pair et récupérer sa liste de pairs (auto-annonce réseau)
	regErr := e.node.RegisterWithPeer(req.Address)
	// Synchroniser la chaîne immédiatement
	go e.node.SendToPeer(req.Address, network.Message{Type: "GET_BLOCKS"})

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"ok":    regErr == nil,
		"peers": e.node.GetPeers(),
	}
	if regErr != nil {
		resp["warning"] = regErr.Error()
	}
	json.NewEncoder(w).Encode(resp)
}

// GET /api/peers/status — statut détaillé de chaque pair (online/offline, nb blocs, nb pairs)
func (e *Explorer) apiPeersStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	statuses := e.node.GetPeersStatus()
	json.NewEncoder(w).Encode(map[string]interface{}{"peers": statuses})
}

// ─── NODE PAGE ────────────────────────────────────────────────────────────────

func (e *Explorer) apiAdminPostDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != "POST" {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || !adminUsernames[user.Pseudo] {
		http.Error(w, `{"error":"non autorisé"}`, 403)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"JSON invalide"}`, 400)
		return
	}
	postID, err := bson.ObjectIDFromHex(req.ID)
	if err != nil {
		http.Error(w, `{"error":"id invalide"}`, 400)
		return
	}
	db.AdminDeletePost(postID)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (e *Explorer) apiAdminCommentDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != "POST" {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || !adminUsernames[user.Pseudo] {
		http.Error(w, `{"error":"non autorisé"}`, 403)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"JSON invalide"}`, 400)
		return
	}
	commentID, err := bson.ObjectIDFromHex(req.ID)
	if err != nil {
		http.Error(w, `{"error":"id invalide"}`, 400)
		return
	}
	db.AdminDeleteComment(commentID)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// POST /api/comment/like — toggle like sur un commentaire
func (e *Explorer) apiCommentLike(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		jsonError(w, "POST requis", 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		jsonError(w, "Non connecté", 401)
		return
	}
	var req struct {
		CommentID string `json:"comment_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	commentID, err := bson.ObjectIDFromHex(req.CommentID)
	if err != nil {
		jsonError(w, "ID invalide", 400)
		return
	}
	liked, count, err := db.ToggleCommentLike(commentID, user.ID.Hex())
	if err != nil {
		jsonError(w, "Erreur serveur", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"liked": liked, "count": count})
}

// POST /api/comment/reply — ajouter une réponse à un commentaire
func (e *Explorer) apiCommentReply(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		jsonError(w, "POST requis", 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		jsonError(w, "Non connecté", 401)
		return
	}
	var req struct {
		CommentID string `json:"comment_id"`
		PostID    string `json:"post_id"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" {
		jsonError(w, "Contenu invalide", 400)
		return
	}
	if len([]rune(req.Content)) > 500 {
		jsonError(w, "Réponse trop longue (max 500)", 400)
		return
	}
	commentID, err := bson.ObjectIDFromHex(req.CommentID)
	if err != nil {
		jsonError(w, "ID invalide", 400)
		return
	}
	reply := db.CommentReply{
		UserID:    user.ID,
		Pseudo:    user.Pseudo,
		AvatarSVG: renderProfilePic(user, 24),
		Content:   req.Content,
	}
	if err := db.AddCommentReply(commentID, reply); err != nil {
		jsonError(w, "Erreur serveur", 500)
		return
	}
	// Notifier l'auteur du commentaire parent (hors goroutine)
	go func() {
		parent, err := db.GetCommentByID(commentID)
		if err != nil || parent.UserID == user.ID {
			return
		}
		postID, _ := bson.ObjectIDFromHex(req.PostID)
		preview := req.Content
		if len([]rune(preview)) > 80 {
			preview = string([]rune(preview)[:80])
		}
		db.CreateNotification(&db.Notification{
			UserID:         parent.UserID,
			FromID:         user.ID,
			FromPseudo:     user.Pseudo,
			FromAvatarSVG:  renderProfilePic(user, 28),
			PostID:         postID,
			CommentPreview: "↩ " + preview,
			Read:           false,
		})
	}()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiAdminChatDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != "POST" {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || !adminUsernames[user.Pseudo] {
		http.Error(w, `{"error":"non autorisé"}`, 403)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"JSON invalide"}`, 400)
		return
	}
	msgID, err := bson.ObjectIDFromHex(req.ID)
	if err != nil {
		http.Error(w, `{"error":"id invalide"}`, 400)
		return
	}
	db.AdminDeleteChatMessage(msgID)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// ─── CHAT IMAGE UPLOAD ────────────────────────────────────────────────────────

// POST /api/chat/upload-image — upload image dans le chat global (utilisateur connecté)
func (e *Explorer) apiChatUploadImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		jsonError(w, "POST requis", 405)
		return
	}
	if _, err := auth.GetSession(r); err != nil {
		jsonError(w, "Non autorisé", 403)
		return
	}
	if err := r.ParseMultipartForm(5 << 20); err != nil {
		jsonError(w, "Fichier trop grand (max 5MB)", 400)
		return
	}
	file, hdr, err := r.FormFile("image")
	if err != nil {
		jsonError(w, "Fichier manquant", 400)
		return
	}
	defer file.Close()
	ext := ".jpg"
	if hdr.Filename != "" {
		lower := strings.ToLower(hdr.Filename)
		if strings.HasSuffix(lower, ".png") {
			ext = ".png"
		} else if strings.HasSuffix(lower, ".gif") {
			ext = ".gif"
		}
	}
	fname := fmt.Sprintf("./static/uploads/chat/%s%s", bson.NewObjectID().Hex(), ext)
	data, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "Erreur lecture", 500)
		return
	}
	if err := os.WriteFile(fname, data, 0644); err != nil {
		jsonError(w, "Erreur écriture", 500)
		return
	}
	url := "/static/uploads/chat/" + fname[len("./static/uploads/chat/"):]
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}

// ─── ANNOUNCEMENTS API ────────────────────────────────────────────────────────

// POST /api/announcements/upload — upload image pour une annonce (admin)
func (e *Explorer) apiAnnouncementUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		jsonError(w, "POST requis", 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || !adminUsernames[user.Pseudo] {
		jsonError(w, "Non autorisé", 403)
		return
	}
	if err := r.ParseMultipartForm(5 << 20); err != nil {
		jsonError(w, "Fichier trop grand (max 5MB)", 400)
		return
	}
	file, hdr, err := r.FormFile("image")
	if err != nil {
		jsonError(w, "Fichier manquant", 400)
		return
	}
	defer file.Close()
	ext := ".jpg"
	if hdr.Filename != "" {
		if e := hdr.Filename[len(hdr.Filename)-4:]; e == ".png" || e == ".gif" {
			ext = e
		} else if len(hdr.Filename) > 5 && hdr.Filename[len(hdr.Filename)-5:] == ".jpeg" {
			ext = ".jpg"
		}
	}
	fname := fmt.Sprintf("./static/uploads/announcements/%s%s", bson.NewObjectID().Hex(), ext)
	data, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "Erreur lecture", 500)
		return
	}
	if err := os.WriteFile(fname, data, 0644); err != nil {
		jsonError(w, "Erreur écriture", 500)
		return
	}
	url := "/static/uploads/announcements/" + fname[len("./static/uploads/announcements/"):]
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}

// GET /api/announcements → liste | POST /api/announcements → créer (admin)
func (e *Explorer) apiAnnouncements(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		list, err := db.GetAnnouncements()
		if err != nil {
			jsonError(w, "Erreur lecture", 500)
			return
		}
		if list == nil {
			list = []*db.Announcement{}
		}
		json.NewEncoder(w).Encode(list)
	case http.MethodPost:
		user, err := auth.GetSession(r)
		if err != nil || !adminUsernames[user.Pseudo] {
			jsonError(w, "Non autorisé", 403)
			return
		}
		var req struct {
			Title    string `json:"title"`
			Content  string `json:"content"`
			MediaURL string `json:"media_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Content) == "" {
			jsonError(w, "Titre et contenu requis", 400)
			return
		}
		a := &db.Announcement{Title: strings.TrimSpace(req.Title), Content: strings.TrimSpace(req.Content), MediaURL: strings.TrimSpace(req.MediaURL)}
		if err := db.CreateAnnouncement(a); err != nil {
			jsonError(w, "Erreur création", 500)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(a)
	default:
		http.Error(w, "Méthode non supportée", 405)
	}
}

// PUT /api/announcements/:id → modifier (admin) | DELETE → supprimer (admin)
func (e *Explorer) apiAnnouncementByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	idStr := strings.TrimPrefix(r.URL.Path, "/api/announcements/")
	id, err := bson.ObjectIDFromHex(idStr)
	if err != nil {
		jsonError(w, "ID invalide", 400)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || !adminUsernames[user.Pseudo] {
		jsonError(w, "Non autorisé", 403)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Title    string `json:"title"`
			Content  string `json:"content"`
			MediaURL string `json:"media_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" {
			jsonError(w, "Titre requis", 400)
			return
		}
		if err := db.UpdateAnnouncement(id, strings.TrimSpace(req.Title), strings.TrimSpace(req.Content), strings.TrimSpace(req.MediaURL)); err != nil {
			jsonError(w, "Erreur mise à jour", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := db.DeleteAnnouncement(id); err != nil {
			jsonError(w, "Erreur suppression", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		http.Error(w, "Méthode non supportée", 405)
	}
}

// ─── POOL API ─────────────────────────────────────────────────────────────────

// GET /api/block-template — job de minage pour un pool externe
func (e *Explorer) apiBlockTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET requis", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	last := e.bc.GetLastBlock()
	pending := e.mp.GetPending(50)
	txStrings := make([]string, 0, len(pending))
	for _, tx := range pending {
		txStrings = append(txStrings, tx.ToString())
	}
	if len(txStrings) == 0 {
		txStrings = []string{"empty-block"}
	}
	txJSON, _ := json.Marshal(txStrings)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"index":        last.Index + 1,
		"previousHash": last.Hash,
		"transactions": string(txJSON),
		"difficulty":   e.bc.Difficulty,
		"blockReward":  11.0,
	})
}

// POST /api/submit-block — soumission de bloc par un pool externe
func (e *Explorer) apiSubmitBlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST requis", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var req struct {
		Index        int      `json:"index"`
		PreviousHash string   `json:"previousHash"`
		Transactions []string `json:"transactions"`
		Timestamp    int64    `json:"timestamp"`
		Nonce        int      `json:"nonce"`
		Hash         string   `json:"hash"`
		Difficulty   int      `json:"difficulty"`
		MinerAddress string   `json:"minerAddress"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	if req.MinerAddress == "" || !strings.HasPrefix(req.MinerAddress, "SCO") {
		jsonError(w, "Adresse mineur invalide", 400)
		return
	}

	// Anti-spike timestamp ±120s
	now := time.Now().Unix()
	if req.Timestamp < now-120 || req.Timestamp > now+120 {
		jsonError(w, "Timestamp invalide", 400)
		return
	}

	// Recalcul du hash côté serveur
	txStrings := req.Transactions
	if len(txStrings) == 0 {
		txStrings = []string{"empty-block"}
	}
	txData := strings.Join(txStrings, ";")
	inp := fmt.Sprintf("%d%d%s%s%d%s", req.Index, req.Timestamp, txData, req.PreviousHash, req.Nonce, req.MinerAddress)
	hh := sha256.Sum256([]byte(inp))
	expectedHash := hex.EncodeToString(hh[:])
	if expectedHash != req.Hash {
		jsonError(w, "Hash invalide", 400)
		return
	}

	e.mineMu.Lock()
	defer e.mineMu.Unlock()

	last := e.bc.GetLastBlock()
	if req.Index != last.Index+1 {
		jsonError(w, "Index invalide — bloc déjà trouvé", 409)
		return
	}
	if req.PreviousHash != last.Hash {
		jsonError(w, "PreviousHash invalide", 400)
		return
	}
	if req.Timestamp <= last.Timestamp {
		jsonError(w, "Timestamp antérieur ou égal au bloc précédent", 400)
		return
	}
	// Anti-spike global (temps réel serveur depuis le dernier bloc accepté)
	if time.Now().Unix()-atomic.LoadInt64(&e.lastBlockAcceptedAt) < 120 {
		jsonError(w, "Bloc trop rapide (anti-spike)", 400)
		return
	}
	// Vérification difficulté
	if req.Difficulty < e.bc.Difficulty {
		jsonError(w, "Difficulté insuffisante", 400)
		return
	}
	if !strings.HasPrefix(req.Hash, strings.Repeat("0", req.Difficulty)) {
		jsonError(w, "Difficulté non atteinte", 400)
		return
	}

	newBlock := &blockchain.Block{
		Index:        req.Index,
		Timestamp:    req.Timestamp,
		Transactions: txStrings,
		PreviousHash: req.PreviousHash,
		Hash:         req.Hash,
		Nonce:        req.Nonce,
		Difficulty:   req.Difficulty,
		Reward:       blockchain.CalculateReward(req.Index),
		MinerAddress: req.MinerAddress,
	}
	e.bc.Blocks = append(e.bc.Blocks, newBlock)
	e.bc.TotalSupply += newBlock.Reward
	e.bc.Save()
	atomic.StoreInt64(&e.lastBlockAcceptedAt, time.Now().Unix())
	e.mp.ClearByStrings(txStrings)

	if e.node != nil {
		go e.node.BroadcastBlock(newBlock)
	}

	fmt.Printf("[Pool] Bloc #%d soumis par %.16s\n", newBlock.Index, req.MinerAddress)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "block_index": newBlock.Index, "reward": newBlock.Reward, "hash": newBlock.Hash,
	})
}

// POST /api/pool-payout — paiement pool vers une adresse (nécessite POOL_SECRET)
func (e *Explorer) apiPoolPayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST requis", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	var req struct {
		To     string `json:"to"`
		Amount int    `json:"amount"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	poolSecret := os.Getenv("POOL_SECRET")
	if poolSecret == "" || req.Secret != poolSecret {
		jsonError(w, "Secret invalide", 403)
		return
	}
	if req.To == "" || !strings.HasPrefix(req.To, "SCO") {
		jsonError(w, "Adresse destinataire invalide", 400)
		return
	}
	if req.Amount <= 0 {
		jsonError(w, "Montant invalide", 400)
		return
	}
	poolAddress := os.Getenv("POOL_ADDRESS")
	if poolAddress == "" {
		jsonError(w, "POOL_ADDRESS non configuré", 500)
		return
	}
	tx := transaction.NewTransaction(poolAddress, req.To, req.Amount, 0)
	if !e.mp.Add(tx) {
		jsonError(w, "Transaction invalide ou doublon", 400)
		return
	}
	fmt.Printf("[Pool] Payout %d SCO → %s\n", req.Amount, req.To)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "tx_id": tx.ID})
}

func (e *Explorer) apiAdminSetDifficulty(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST requis", 405)
		return
	}
	// Localhost only
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host != "127.0.0.1" && host != "::1" {
		http.Error(w, "Forbidden", 403)
		return
	}
	var req struct {
		Difficulty int `json:"difficulty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Difficulty < 4 || req.Difficulty > 32 {
		jsonError(w, "difficulty invalide (4-32)", 400)
		return
	}
	e.bc.Difficulty = req.Difficulty
	e.bc.DifficultyForced = true
	e.bc.Save()
	fmt.Printf("[Admin] Difficulté forcée à %d\n", req.Difficulty)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "difficulty": req.Difficulty})
}

func (e *Explorer) apiAdminUnforceDifficulty(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST requis", 405)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host != "127.0.0.1" && host != "::1" {
		http.Error(w, "Forbidden", 403)
		return
	}
	e.bc.DifficultyForced = false
	e.bc.Save()
	fmt.Printf("[Admin] DifficultyForced désactivé\n")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (e *Explorer) handleNode(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page("Run a Node — Scorbits", nodeHTML(lang), "node", lang, user))
}

func nodeHTML(lang string) string {
	fr := lang == "fr"
	tl := func(frStr, enStr string) string {
		if fr {
			return frStr
		}
		return enStr
	}
	noPeersMsg := tl("Aucun pair connecté. Soyez le premier à lancer un nœud.", "No peers connected yet. Be the first to run a node.")
	offlineLabel := tl("hors ligne", "offline")
	blocksLabel := tl("blocs", "blocks")
	peersLabel := tl("pairs", "peers")
	return `
<style>
.node-wrap{max-width:900px;margin:0 auto;padding:2rem 1rem;}
.node-hero{margin-bottom:2.5rem;}
.node-hero h1{font-size:1.8rem;font-weight:700;color:#fff;margin-bottom:.5rem;}
.node-hero p{color:#888;font-size:1rem;max-width:560px;}
.node-grid{display:grid;grid-template-columns:1fr 1fr 1fr;gap:1rem;margin-bottom:2rem;}
.node-stat{background:#0d0d0d;border:1px solid #1e1e1e;border-radius:10px;padding:1.2rem 1rem;text-align:center;}
.node-stat-val{font-size:1.8rem;font-weight:700;color:#00e85a;}
.node-stat-lbl{font-size:.75rem;color:#555;text-transform:uppercase;letter-spacing:.08em;margin-top:.3rem;}
.node-section{margin-bottom:2rem;}
.node-section-title{font-size:.7rem;text-transform:uppercase;letter-spacing:.12em;color:#555;margin-bottom:1rem;padding-bottom:.5rem;border-bottom:1px solid #1a1a1a;}
.node-dl-grid{display:grid;grid-template-columns:1fr 1fr;gap:1rem;margin-bottom:2rem;}
.node-dl-card{background:#0d0d0d;border:1px solid #1e1e1e;border-radius:10px;padding:1.4rem;display:flex;flex-direction:column;gap:.8rem;}
.node-dl-card:hover{border-color:#2a2a2a;}
.node-dl-os{font-size:.95rem;font-weight:600;color:#ccc;}
.node-dl-arch{font-size:.75rem;color:#555;}
.node-dl-btn{display:inline-block;padding:.6rem 1.2rem;background:#00e85a;color:#000;font-weight:700;font-size:.85rem;border-radius:6px;text-decoration:none;text-align:center;transition:opacity .2s;}
.node-dl-btn:hover{opacity:.85;}
.node-steps{display:flex;flex-direction:column;gap:.75rem;}
.node-step{display:flex;gap:1rem;align-items:flex-start;}
.node-step-num{min-width:24px;height:24px;background:#0a2a1a;border:1px solid #00e85a22;color:#00e85a;border-radius:50%;display:flex;align-items:center;justify-content:center;font-size:.75rem;font-weight:700;flex-shrink:0;margin-top:2px;}
.node-step-body{color:#aaa;font-size:.9rem;line-height:1.6;}
.node-step-body strong{color:#ddd;}
.cmd-block{background:#080808;border:1px solid #1a1a1a;border-radius:6px;padding:.7rem 1rem;font-family:'Courier New',monospace;font-size:.82rem;color:#00e85a;margin:.5rem 0;white-space:pre;overflow-x:auto;}
.node-peers{background:#0d0d0d;border:1px solid #1e1e1e;border-radius:10px;overflow:hidden;}
.peer-row{display:flex;align-items:center;gap:.75rem;padding:.8rem 1rem;border-bottom:1px solid #111;font-size:.85rem;}
.peer-row:last-child{border-bottom:none;}
.peer-dot{width:7px;height:7px;border-radius:50%;flex-shrink:0;}
.peer-dot.on{background:#00e85a;}
.peer-dot.off{background:#2a2a2a;}
.peer-addr{font-family:monospace;color:#888;flex:1;}
.peer-info{color:#444;font-size:.78rem;}
.node-empty{padding:1.5rem;text-align:center;color:#444;font-size:.88rem;}
@media(max-width:600px){.node-grid{grid-template-columns:1fr 1fr;}.node-dl-grid{grid-template-columns:1fr;}}
</style>

<div class="node-wrap">
  <div class="node-hero">
    <h1>` + tl("Lancer un Nœud", "Run a Node") + `</h1>
    <p>` + tl("Renforcez le réseau Scorbits en lançant un nœud complet. Votre nœud synchronise la blockchain, valide les blocs et relaie les données aux pairs.", "Strengthen the Scorbits network by running a full node. Your node syncs the blockchain, validates blocks, and relays data to peers.") + `</p>
  </div>

  <div class="node-grid">
    <div class="node-stat"><div class="node-stat-val" id="nn-peers">—</div><div class="node-stat-lbl">` + tl("Pairs connus", "Known Peers") + `</div></div>
    <div class="node-stat"><div class="node-stat-val" id="nn-online">—</div><div class="node-stat-lbl">` + tl("Pairs en ligne", "Online Peers") + `</div></div>
    <div class="node-stat"><div class="node-stat-val" id="nn-blocks">—</div><div class="node-stat-lbl">` + tl("Blocs synchronisés", "Blocks Synced") + `</div></div>
  </div>

  <div class="node-section">
    <div class="node-section-title">` + tl("Télécharger", "Download") + `</div>
    <div class="node-dl-grid">
      <div class="node-dl-card">
        <div>
          <div class="node-dl-os">Linux</div>
          <div class="node-dl-arch">x86-64 — ` + tl("Binaire statique", "Static binary") + `</div>
        </div>
        <a href="/static/nodes/scorbits-node-linux" download class="node-dl-btn">` + tl("Télécharger pour Linux", "Download for Linux") + `</a>
      </div>
      <div class="node-dl-card">
        <div>
          <div class="node-dl-os">Windows</div>
          <div class="node-dl-arch">x86-64 — ` + tl("Exécutable", "Executable") + `</div>
        </div>
        <a href="/static/nodes/scorbits-node-windows.exe" download class="node-dl-btn">` + tl("Télécharger pour Windows", "Download for Windows") + `</a>
      </div>
    </div>
  </div>

  <div class="node-section">
    <div class="node-section-title">` + tl("Démarrage rapide", "Quick Start") + `</div>
    <div class="node-steps">
      <div class="node-step">
        <div class="node-step-num">1</div>
        <div class="node-step-body">
          <strong>Linux</strong> — ` + tl("Rendez le binaire exécutable, puis lancez-le :", "Make the binary executable, then launch it:") + `
          <div class="cmd-block">chmod +x scorbits-node-linux
./scorbits-node-linux --peers 51.91.122.48:3000</div>
        </div>
      </div>
      <div class="node-step">
        <div class="node-step-num">2</div>
        <div class="node-step-body">
          <strong>Windows</strong> — ` + tl("Ouvrez un terminal dans le dossier et lancez :", "Open a terminal in the folder and run:") + `
          <div class="cmd-block">scorbits-node-windows.exe --peers 51.91.122.48:3000</div>
        </div>
      </div>
      <div class="node-step">
        <div class="node-step-num">3</div>
        <div class="node-step-body">
          <strong>` + tl("Options", "Optional flags") + `</strong>
          <div class="cmd-block">--port 3000          # ` + tl("Port TCP d'écoute (défaut : 3000)", "TCP port to listen on (default: 3000)") + `
--peers ip:port      # ` + tl("Pairs de démarrage, séparés par virgules", "Bootstrap peers, comma-separated") + `
--data /path/to/dir  # ` + tl("Dossier de stockage de la chaîne", "Directory to store the chain file") + `</div>
        </div>
      </div>
      <div class="node-step">
        <div class="node-step-num">4</div>
        <div class="node-step-body">` + tl("Votre nœud synchronise automatiquement la blockchain, affiche des statistiques toutes les 30s et relaie les nouveaux blocs à tous les pairs connectés.", "Your node will automatically sync the blockchain, print stats every 30s, and relay new blocks to all connected peers.") + `</div>
      </div>
    </div>
  </div>

  <div class="node-section">
    <div class="node-section-title">` + tl("Pairs connectés", "Connected Peers") + `</div>
    <div class="node-peers" id="peer-list"><div class="node-empty">` + tl("Chargement...", "Loading...") + `</div></div>
  </div>
</div>

<script>
async function loadNodeStats() {
  try {
    const [pr, sr] = await Promise.all([fetch('/api/peers/status'), fetch('/api/stats')]);
    const pd = await pr.json();
    const sd = await sr.json();
    const peers = pd.peers || [];
    document.getElementById('nn-peers').textContent = peers.length;
    document.getElementById('nn-online').textContent = peers.filter(p => p.online).length;
    document.getElementById('nn-blocks').textContent = (sd.blocks || 0).toLocaleString();
    const list = document.getElementById('peer-list');
    if (!peers.length) {
      list.innerHTML = '<div class="node-empty">` + noPeersMsg + `</div>';
      return;
    }
    list.innerHTML = peers.map(p => ` + "`" + `
      <div class="peer-row">
        <span class="peer-dot ${p.online ? 'on' : 'off'}"></span>
        <span class="peer-addr">${p.address}</span>
        <span class="peer-info">${p.online ? p.blocks.toLocaleString() + ' ` + blocksLabel + ` &middot; ' + p.peers + ' ` + peersLabel + `' : '` + offlineLabel + `'}</span>
      </div>` + "`" + `).join('');
  } catch(e) {}
}
loadNodeStats();
setInterval(loadNodeStats, 30000);
</script>
`
}

// ─── AUTH PAGES ───────────────────────────────────────────────────────────────

func (e *Explorer) handleWalletLogin(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	if user, err := auth.GetSession(r); err == nil && user != nil {
		http.Redirect(w, r, "/wallet/dashboard", 302)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "wallet.page_login"), loginHTML(lang), "wallet", lang, nil))
}

func (e *Explorer) handleRegister(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	if user, err := auth.GetSession(r); err == nil && user != nil {
		http.Redirect(w, r, "/wallet/dashboard", 302)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "wallet.page_register"), registerHTML(lang), "wallet", lang, nil))
}

func (e *Explorer) handleLogout(w http.ResponseWriter, r *http.Request) {
	auth.DestroySession(w, r)
	http.Redirect(w, r, "/wallet", 302)
}

func (e *Explorer) handleVerify(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	token := r.URL.Query().Get("token")
	if token == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page(i18n.T(lang, "wallet.page_verify"), verifyErrorHTML(i18n.T(lang, "verify.token_missing"), lang), "wallet", lang, nil))
		return
	}
	user, err := db.GetUserByVerifyToken(token)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page(i18n.T(lang, "wallet.page_verify"), verifyErrorHTML(i18n.T(lang, "verify.invalid_link"), lang), "wallet", lang, nil))
		return
	}
	if err := db.VerifyUser(user.ID); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page(i18n.T(lang, "wallet.page_verify"), verifyErrorHTML(i18n.T(lang, "verify.error_verify"), lang), "wallet", lang, nil))
		return
	}
	auth.CreateSession(w, user.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "wallet.page_verified"), verifySuccessHTML(user.Pseudo, lang), "wallet", lang, user))
}

// ─── WALLET PAGES ─────────────────────────────────────────────────────────────

func (e *Explorer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/profile#wallet", http.StatusMovedPermanently)
}

func (e *Explorer) handleSend(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/profile#wallet", http.StatusMovedPermanently)
}

func (e *Explorer) handleReceive(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/profile#wallet", http.StatusMovedPermanently)
}

func (e *Explorer) handleHistory(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, ok := auth.RequireAuth(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "history.page_title"), historyHTML(user, lang), "wallet", lang, user))
}

func (e *Explorer) handleProfile(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, ok := auth.RequireAuth(w, r)
	if !ok {
		return
	}
	balance := e.bc.GetBalance(user.Address)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "profile.page_title"), profileHTML(user, balance, lang), "wallet", lang, user))
}

func (e *Explorer) handleMine(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page(i18n.T(lang, "mine.page_title"), mineHTML(user, lang), "mine", lang, user))
}

func (e *Explorer) handleDownloadCLIWindows(w http.ResponseWriter, r *http.Request) {
	toWin := func(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

	advancedBat := `@echo off
title Scorbits Miner
color 0A

:restart
cls
echo  ==========================================
echo   SCORBITS MINER - scorbits.com
echo  ==========================================
echo.

if not exist scorbits.exe (
    echo  ERROR: scorbits.exe not found in current directory.
    echo  Please run this .bat file from the folder containing scorbits.exe
    echo.
    pause
    exit /b 1
)

set ADDRESS=
if exist wallet.txt (
    set /p ADDRESS=<wallet.txt
)

:ask_address
if "%ADDRESS%"=="" (
    echo  Enter your SCO address to receive mining rewards.
    echo  Example: SCOxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
    echo.
    set /p ADDRESS=  Your address:
    if "%ADDRESS%"=="" goto :ask_address
    echo %ADDRESS%>wallet.txt
)

echo %ADDRESS% | findstr /B "SCO" >nul
if errorlevel 1 (
    echo.
    echo  Invalid address: must start with SCO.
    del wallet.txt >nul 2>&1
    set ADDRESS=
    goto :ask_address
)

echo.
echo  Address : %ADDRESS%
echo  Mining against : scorbits.com
echo  Press Ctrl+C to stop.
echo.
scorbits.exe mine %ADDRESS%

echo.
echo  Miner stopped. Restarting in 3 seconds... (Ctrl+C to exit)
timeout /t 3 /nobreak >nul
goto :restart`

	readmeTxt := `SCORBITS CLI - Windows
======================
1. Double-click ADVANCED.bat to start mining
   - First run: enter your SCO address
   - Address is saved in wallet.txt for next time

2. Create a wallet (optional):
   scorbits.exe wallet-new

3. Check balance:
   scorbits.exe balance SCOyouraddresshere

Website : https://scorbits.com
Support : info@scorbits.com`

	exeData, err := os.ReadFile("releases/scorbits-windows-amd64.exe")
	if err != nil {
		http.Error(w, "scorbits-windows-amd64.exe not found in releases/", 500)
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := []struct {
		name    string
		content []byte
	}{
		{"scorbits.exe", exeData},
		{"ADVANCED.bat", toWin(advancedBat)},
		{"README.txt", toWin(readmeTxt)},
	}
	for _, f := range files {
		fw, err := zw.Create(f.name)
		if err != nil {
			http.Error(w, "ZIP error", 500)
			return
		}
		fw.Write(f.content)
	}
	zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="scorbits-windows.zip"`)
	w.Write(buf.Bytes())
}

// ─── AVATAR ───────────────────────────────────────────────────────────────────

// renderProfilePic returns an <img> if the user has a profile photo, else an initials <div>.
func renderProfilePic(user *db.User, size int) string {
	if user != nil && user.ProfilePic != "" {
		return fmt.Sprintf(`<img src="%s" width="%d" height="%d" style="border-radius:50%%;object-fit:cover;display:block;flex-shrink:0;" loading="lazy">`, user.ProfilePic, size, size)
	}
	initials := "?"
	if user != nil && user.Pseudo != "" {
		r := []rune(strings.ToUpper(user.Pseudo))
		if len(r) >= 2 {
			initials = string(r[:2])
		} else {
			initials = string(r[:1])
		}
	}
	fontSize := size / 3
	if fontSize < 8 {
		fontSize = 8
	}
	return fmt.Sprintf(`<div style="width:%dpx;height:%dpx;border-radius:50%%;background:#0d2010;display:inline-flex;align-items:center;justify-content:center;color:#00e85a;font-weight:700;font-size:%dpx;flex-shrink:0;line-height:1;">%s</div>`, size, size, fontSize, initials)
}

// ─── WALLET/MINE HTML ──────────────────────────────────────────

func walletSidebar(user *db.User, active, lang string) string {
	avatarSVG := renderProfilePic(user, 52)
	addrShort := user.Address
	if len(addrShort) > 22 {
		addrShort = addrShort[:10] + "..." + addrShort[len(addrShort)-8:]
	}
	nav := func(href, label, key string) string {
		cls := "wnav-item"
		if key == active {
			cls += " active"
		}
		return fmt.Sprintf(`<a href="%s" class="%s">%s</a>`, href, cls, label)
	}
	return fmt.Sprintf(`<div class="wside">
  <div class="wside-user">
    <div class="wside-avatar">%s</div>
    <div class="wside-pseudo">@%s</div>
    <div class="wside-addr mono sm muted">%s</div>
  </div>
  <div class="wside-nav">
    %s
    %s
    %s
    %s
    %s
    <a href="/wallet/logout" class="wnav-item logout">%s</a>
  </div>
</div>`,
		avatarSVG, user.Pseudo, addrShort,
		nav("/wallet/dashboard", i18n.T(lang, "wallet.dashboard"), "dashboard"),
		nav("/wallet/send", i18n.T(lang, "wallet.send"), "send"),
		nav("/wallet/receive", i18n.T(lang, "wallet.receive"), "receive"),
		nav("/wallet/history", i18n.T(lang, "wallet.history"), "history"),
		nav("/profile", i18n.T(lang, "wallet.profile"), "profile"),
		i18n.T(lang, "wallet.logout"),
	)
}

func dashboardHTML(user *db.User, balance int, lang string) string {
	sidebar := walletSidebar(user, "dashboard", lang)
	return fmt.Sprintf(`<div class="wdash">
%s
<div class="wmain">
  <div class="wdash-balance">
    <div class="wdb-label">`+i18n.T(lang, "dashboard.available_balance")+`</div>
    <div class="wdb-amount">%d <span style="font-size:1.1rem;color:var(--text2)">SCO</span></div>
    <div class="wdb-eur mono sm">%s</div>
  </div>
  <div class="wdash-actions">
    <a href="/wallet/send" class="wact primary">
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
      <span class="wact-label">`+i18n.T(lang, "wallet.send")+`</span>
    </a>
    <a href="/wallet/receive" class="wact">
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="8 17 12 21 16 17"/><line x1="12" y1="12" x2="12" y2="21"/><path d="M20.88 18.09A5 5 0 0 0 18 9h-1.26A8 8 0 1 0 3 16.29"/></svg>
      <span class="wact-label">`+i18n.T(lang, "wallet.receive")+`</span>
    </a>
    <a href="/wallet/history" class="wact">
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
      <span class="wact-label">`+i18n.T(lang, "wallet.history")+`</span>
    </a>
  </div>
  <div class="stitle">`+i18n.T(lang, "dashboard.recent_tx")+`</div>
  <div class="txlist" id="tx-list"><div class="txlist-load">`+i18n.T(lang, "common.loading")+`</div></div>
</div>
</div>
<script>
(async()=>{
  const res=await fetch('/api/history/%s');
  const txs=await res.json()||[];
  const el=document.getElementById('tx-list');
  if(!txs.length){el.innerHTML='<div class="txlist-load">'+I18N['dashboard.no_tx']+'</div>';return;}
  el.innerHTML=txs.slice(0,15).map(t=>{
    const ismine=t.type==='minage',issend=t.type==='envoi';
    const icls=ismine?'mine':issend?'send':'recv';
    const sign=issend?'-':'+';
    const acls=issend?'neg':'pos';
    const label=ismine?I18N['history.mined_block']+t.block:issend?I18N['history.send']:I18N['history.recv'];
    const d=new Date(t.timestamp*1000).toLocaleDateString(I18N['common.locale']);
    const icon=ismine
      ?'<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/></svg>'
      :issend
        ?'<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>'
        :'<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="23 6 13.5 15.5 8.5 10.5 1 18"/><polyline points="17 6 23 6 23 12"/></svg>';
    return '<div class="tx-row" onclick="window.location=\'\/block\/'+t.block+'\'">'+
      '<div class="tx-icon '+icls+'">'+icon+'</div>'+
      '<div class="tx-info"><div class="tx-label">'+label+'</div><div class="tx-meta">Bloc #'+t.block+'</div></div>'+
      '<div class="tx-right"><div class="tx-amt '+acls+'">'+sign+t.amount+' SCO</div><div class="tx-date">'+d+'</div></div>'+
    '</div>';
  }).join('');
})();
</script>`, sidebar, balance, user.Address, user.Address)
}

func sendHTML(user *db.User, balance int, lang string) string {
	sidebar := walletSidebar(user, "send", lang)
	return fmt.Sprintf(`<div class="wdash">
%s
<div class="wmain">
  <div class="stitle">`+i18n.T(lang, "send.title")+`</div>
  <div class="send-card">
    <div class="send-bal">`+i18n.T(lang, "send.available_balance")+`&#160;: <strong class="green">%d SCO</strong></div>
    <div class="fgroup"><label>`+i18n.T(lang, "send.recipient")+`</label><input type="text" id="s-to" class="finput" placeholder="SCO..." autocomplete="off"></div>
    <div class="fgroup"><label>`+i18n.T(lang, "send.amount")+`</label><input type="number" id="s-amount" class="finput" placeholder="0" min="1" oninput="updateFee()"></div>
    <div class="fee-box">`+i18n.T(lang, "send.network_fee")+`&#160;: <strong id="fee-val">1 SCO</strong> &#8212; `+i18n.T(lang, "send.treasury")+`&#160;: <strong id="treasury-val">0 SCO</strong> &#8212; `+i18n.T(lang, "send.total")+`&#160;: <strong id="total-val">-- SCO</strong></div>
    <div id="s-error" class="ferror hidden"></div>
    <button class="btn-main" onclick="openSendModal()">`+i18n.T(lang, "send.check_send")+`</button>
  </div>
  <div id="send-modal" class="modal-overlay hidden" onclick="if(event.target===this)closeSendModal()">
    <div class="modal-box">
      <div class="modal-title">`+i18n.T(lang, "send.confirm_title")+`</div>
      <div class="modal-body">
        <div class="cb-row"><span>`+i18n.T(lang, "send.recipient_label")+`</span><span class="mono sm" id="m-to"></span></div>
        <div class="cb-row"><span>`+i18n.T(lang, "send.amount_label")+`</span><span class="green fw6" id="m-amount"></span></div>
        <div class="cb-row"><span>`+i18n.T(lang, "send.fee_label")+`</span><span id="m-fee"></span></div>
        <div class="cb-row"><span>`+i18n.T(lang, "send.treasury_label")+`</span><span id="m-treasury"></span></div>
        <div class="cb-row total"><span>`+i18n.T(lang, "send.total_deducted")+`</span><span id="m-total"></span></div>
      </div>
      <div class="modal-actions">
        <button class="btn-cancel" onclick="closeSendModal()">`+i18n.T(lang, "send.cancel")+`</button>
        <button class="btn-confirm" id="btn-confirm" onclick="doSend()">`+i18n.T(lang, "send.confirm")+`</button>
      </div>
    </div>
  </div>
  <div id="send-result" class="hidden" style="margin-top:1.2rem"></div>
</div>
</div>
<script>
let sFee=1;
function updateFee(){
  const amt=parseInt(document.getElementById('s-amount').value)||0;
  sFee=amt>100?2:1;
  const treasury=Math.floor(amt*0.001);
  document.getElementById('fee-val').textContent=sFee+' SCO';
  document.getElementById('treasury-val').textContent=treasury+' SCO';
  document.getElementById('total-val').textContent=amt>0?(amt+sFee+treasury)+' SCO':'-- SCO';
}
function openSendModal(){
  const to=document.getElementById('s-to').value.trim();
  const amt=parseInt(document.getElementById('s-amount').value)||0;
  const err=document.getElementById('s-error');
  err.classList.add('hidden');
  if(!to.startsWith('SCO')){err.textContent=I18N['send.invalid_address'];err.classList.remove('hidden');return;}
  if(amt<=0){err.textContent=I18N['send.invalid_amount'];err.classList.remove('hidden');return;}
  const treasury=Math.floor(amt*0.001);
  if(amt+sFee+treasury>%d){err.textContent=I18N['send.insufficient_balance'];err.classList.remove('hidden');return;}
  document.getElementById('m-to').textContent=to.length>20?to.substring(0,16)+'...':to;
  document.getElementById('m-amount').textContent=amt+' SCO';
  document.getElementById('m-fee').textContent=sFee+' SCO';
  document.getElementById('m-treasury').textContent=treasury+' SCO';
  document.getElementById('m-total').textContent=(amt+sFee+treasury)+' SCO';
  document.getElementById('send-modal').classList.remove('hidden');
}
function closeSendModal(){document.getElementById('send-modal').classList.add('hidden');}
async function doSend(){
  const btn=document.getElementById('btn-confirm');
  btn.disabled=true;btn.textContent=I18N['send.sending'];
  const to=document.getElementById('s-to').value.trim();
  const amt=parseInt(document.getElementById('s-amount').value)||0;
  const res=await fetch('/api/wallet/send',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({to,amount:amt})});
  const data=await res.json();
  closeSendModal();
  if(data.success){
    document.querySelector('.send-card').style.display='none';
    document.getElementById('send-result').innerHTML='<div class="success-card"><div class="success-icon"><svg width="48" height="48" viewBox="0 0 48 48"><circle cx="24" cy="24" r="22" fill="none" stroke="#00e85a" stroke-width="2"/><path d="M14 24l7 7 13-14" stroke="#00e85a" stroke-width="2.5" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg></div><div class="success-title">'+I18N['send.success_title']+'</div><div class="success-txid">TX&#160;: '+data.tx_id+'</div><div class="success-info">'+I18N['send.confirmed_next_block']+'</div></div>';
    document.getElementById('send-result').classList.remove('hidden');
  }else{
    const err=document.getElementById('s-error');
    err.textContent=data.error||I18N['send.error'];
    err.classList.remove('hidden');
  }
  btn.disabled=false;btn.textContent=I18N['send.confirm'];
}
</script>`, sidebar, balance, balance)
}

func receiveHTML(user *db.User, lang string) string {
	sidebar := walletSidebar(user, "receive", lang)
	return fmt.Sprintf(`<div class="wdash">
%s
<div class="wmain">
  <div class="stitle">`+i18n.T(lang, "receive.title")+`</div>
  <div class="recv-card">
    <div class="recv-label">`+i18n.T(lang, "receive.your_address")+`</div>
    <div class="recv-addr mono green">%s</div>
    <button class="btn-main" onclick="copyAddr()">`+i18n.T(lang, "receive.copy_address")+`</button>
    <div id="copy-ok" class="copy-confirm hidden">
      <svg width="14" height="14" viewBox="0 0 14 14"><path d="M2 7l3 3 7-7" stroke="#00e85a" stroke-width="2" fill="none" stroke-linecap="round"/></svg>
      `+i18n.T(lang, "receive.address_copied")+`
    </div>
    <div class="recv-info">
      `+i18n.T(lang, "receive.info_line1")+`<br>
      `+i18n.T(lang, "receive.info_line2")+`
    </div>
  </div>
</div>
</div>
<script>
function copyAddr(){
  navigator.clipboard.writeText('%s').then(()=>{
    const ok=document.getElementById('copy-ok');
    ok.classList.remove('hidden');
    setTimeout(()=>ok.classList.add('hidden'),2500);
  });
}
</script>`, sidebar, user.Address, user.Address)
}

func historyHTML(user *db.User, lang string) string {
	sidebar := walletSidebar(user, "history", lang)
	return fmt.Sprintf(`<div class="wdash">
%s
<div class="wmain">
  <div class="stitle">`+i18n.T(lang, "history.title")+`</div>
  <div class="txlist" id="hist-list"><div class="txlist-load">`+i18n.T(lang, "common.loading")+`</div></div>
</div>
</div>
<script>
(async()=>{
  const res=await fetch('/api/history/%s');
  const txs=await res.json()||[];
  const el=document.getElementById('hist-list');
  if(!txs.length){el.innerHTML='<div class="txlist-load">'+I18N['history.no_tx']+'</div>';return;}
  el.innerHTML=txs.map(t=>{
    const ismine=t.type==='minage',issend=t.type==='envoi';
    const icls=ismine?'mine':issend?'send':'recv';
    const sign=issend?'-':'+';
    const acls=issend?'neg':'pos';
    const label=ismine?I18N['history.mined_block']+t.block:issend?I18N['history.send']:I18N['history.recv'];
    const d=new Date(t.timestamp*1000).toLocaleString(I18N['common.locale'],{day:'2-digit',month:'2-digit',year:'numeric',hour:'2-digit',minute:'2-digit'});
    const icon=ismine
      ?'<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/></svg>'
      :issend
        ?'<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>'
        :'<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="23 6 13.5 15.5 8.5 10.5 1 18"/><polyline points="17 6 23 6 23 12"/></svg>';
    return '<div class="tx-row" onclick="window.location=\'\/block\/'+t.block+'\'">'+
      '<div class="tx-icon '+icls+'">'+icon+'</div>'+
      '<div class="tx-info"><div class="tx-label">'+label+'</div><div class="tx-meta">'+d+'</div></div>'+
      '<div class="tx-right"><div class="tx-amt '+acls+'">'+sign+t.amount+' SCO</div><div class="tx-date">Bloc #'+t.block+'</div></div>'+
    '</div>';
  }).join('');
})();
</script>`, sidebar, user.Address)
}

func profileHTML(user *db.User, balance int, lang string) string {
	picHTML := renderProfilePic(user, 120)
	hasPic := user.ProfilePic != ""
	deleteBtnStyle := ""
	if !hasPic {
		deleteBtnStyle = `style="display:none"`
	}
	addrShort := user.Address
	if len(addrShort) > 24 {
		addrShort = addrShort[:12] + "..." + addrShort[len(addrShort)-8:]
	}
	wgtl := func(frStr, enStr string) string {
		if lang == "fr" {
			return frStr
		}
		return enStr
	}
	periodD := wgtl("1J", "1D")
	periodW := wgtl("1S", "1W")
	periodM := "1M"
	periodY := wgtl("1A", "1Y")
	widgetPanel := `
<!-- WIDGET TAB -->
<div class="profile-panel" id="profile-panel-widget" style="display:none;">
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
<style>
.wg-filter-bar{display:flex;gap:.5rem;margin-bottom:1.5rem;flex-wrap:wrap;align-items:center;}
.wg-period-btn{padding:.35rem .9rem;border-radius:20px;border:1px solid #2a2a2a;background:transparent;color:#666;font-size:.8rem;cursor:pointer;transition:all .2s;}
.wg-period-btn.active{background:#00e85a;color:#000;border-color:#00e85a;font-weight:600;}
.wg-chart-wrap{background:#0d0d0d;border:1px solid #1e1e1e;border-radius:10px;padding:1.2rem;margin-bottom:1.5rem;height:260px;position:relative;}
.wg-stats{display:grid;grid-template-columns:repeat(4,1fr);gap:1rem;margin-bottom:1.5rem;}
.wg-stat{background:#0d0d0d;border:1px solid #1e1e1e;border-radius:10px;padding:1rem;}
.wg-stat-val{font-size:1.25rem;font-weight:700;color:#00e85a;word-break:break-word;}
.wg-stat-lbl{font-size:.7rem;color:#555;text-transform:uppercase;letter-spacing:.06em;margin-top:.25rem;}
.wg-table-head{display:flex;gap:.5rem;margin-bottom:.75rem;align-items:center;flex-wrap:wrap;}
.wg-type-btn{padding:.28rem .72rem;border-radius:4px;border:1px solid #2a2a2a;background:transparent;color:#666;font-size:.75rem;cursor:pointer;transition:all .2s;}
.wg-type-btn.active{border-color:#00e85a;color:#00e85a;background:#0a1a0a;}
.wg-table{width:100%;border-collapse:collapse;font-size:.82rem;}
.wg-table th{padding:.55rem .8rem;text-align:left;color:#555;font-size:.68rem;text-transform:uppercase;letter-spacing:.06em;border-bottom:1px solid #1a1a1a;font-weight:500;}
.wg-table td{padding:.6rem .8rem;border-bottom:1px solid #0f0f0f;vertical-align:middle;}
.wg-table tr:hover td{background:#111;cursor:pointer;}
.wg-table tr:nth-child(even) td{background:#0a0a0a;}
.wg-badge{display:inline-block;padding:.18rem .55rem;border-radius:4px;font-size:.7rem;font-weight:600;}
.wg-badge.minage{background:#0a2a1a;color:#00e85a;}
.wg-badge.reception,.wg-badge.r\00e9ception,.wg-badge.premine{background:#0a1a2a;color:#4a9eff;}
.wg-badge.envoi{background:#2a0a0a;color:#ff6060;}
.wg-hash{font-family:monospace;font-size:.74rem;color:#444;text-decoration:none;}
.wg-hash:hover{color:#00e85a;}
.wg-amt-pos{color:#00e85a;font-weight:600;}
.wg-amt-neg{color:#ff6060;font-weight:600;}
.wg-pagination{display:flex;gap:.5rem;justify-content:center;align-items:center;margin-top:1rem;}
.wg-page-btn{padding:.38rem .85rem;border-radius:6px;border:1px solid #2a2a2a;background:transparent;color:#888;font-size:.78rem;cursor:pointer;transition:all .2s;}
.wg-page-btn:hover:not(:disabled){border-color:#00e85a;color:#00e85a;}
.wg-page-btn:disabled{opacity:.3;cursor:not-allowed;}
.wg-page-info{color:#555;font-size:.78rem;min-width:60px;text-align:center;}
@media(max-width:640px){.wg-stats{grid-template-columns:1fr 1fr;}}
</style>
<div class="wg-filter-bar">
  <button class="wg-period-btn active" onclick="setWgPeriod('1d',this)">` + periodD + `</button>
  <button class="wg-period-btn" onclick="setWgPeriod('1w',this)">` + periodW + `</button>
  <button class="wg-period-btn" onclick="setWgPeriod('1m',this)">` + periodM + `</button>
  <button class="wg-period-btn" onclick="setWgPeriod('1y',this)">` + periodY + `</button>
  <button class="wg-period-btn" onclick="setWgPeriod('all',this)">MAX</button>
</div>
<div class="wg-chart-wrap"><canvas id="wg-chart"></canvas></div>
<div class="wg-stats">
  <div class="wg-stat"><div class="wg-stat-val" id="wg-balance">—</div><div class="wg-stat-lbl">` + wgtl("Solde actuel", "Current balance") + `</div></div>
  <div class="wg-stat"><div class="wg-stat-val" id="wg-received">—</div><div class="wg-stat-lbl">` + wgtl("Total reçu", "Total received") + `</div></div>
  <div class="wg-stat"><div class="wg-stat-val" id="wg-sent">—</div><div class="wg-stat-lbl">` + wgtl("Total envoyé", "Total sent") + `</div></div>
  <div class="wg-stat"><div class="wg-stat-val" id="wg-mined">—</div><div class="wg-stat-lbl">` + wgtl("Minage", "Mining") + `</div></div>
</div>
<div class="wg-table-head">
  <span style="color:#555;font-size:.78rem;font-weight:500;margin-right:.25rem;">` + wgtl("Filtre :", "Filter:") + `</span>
  <button class="wg-type-btn active" onclick="setWgFilter('all',this)">` + wgtl("Tous", "All") + `</button>
  <button class="wg-type-btn" onclick="setWgFilter('minage',this)">` + wgtl("Minage", "Mining") + `</button>
  <button class="wg-type-btn" onclick="setWgFilter('envoi',this)">` + wgtl("Envoi", "Sent") + `</button>
  <button class="wg-type-btn" onclick="setWgFilter('reception',this)">` + wgtl("Reçu", "Received") + `</button>
</div>
<table class="wg-table">
  <thead><tr><th>Date</th><th>Type</th><th>` + wgtl("Montant", "Amount") + `</th><th>` + wgtl("Bloc", "Block") + `</th><th>Hash</th></tr></thead>
  <tbody id="wg-tbody"></tbody>
</table>
<div class="wg-pagination">
  <button class="wg-page-btn" id="wg-prev" onclick="wgChangePage(-1)">&#x2190; ` + wgtl("Précédent", "Previous") + `</button>
  <span class="wg-page-info" id="wg-page-info"></span>
  <button class="wg-page-btn" id="wg-next" onclick="wgChangePage(1)">` + wgtl("Suivant", "Next") + ` &#x2192;</button>
</div>
</div>
</div>
`
	_ = addrShort
	buyPanel := fmt.Sprintf(`
<!-- BUY SCO TAB -->
<div class="profile-panel" id="profile-panel-buy" style="display:none;">
<style>
.buy-step{display:none;}
.buy-step.active{display:block;}
.buy-crypto-btns{display:flex;gap:0.8rem;margin-bottom:1.2rem;flex-wrap:wrap;}
.buy-crypto-btn{flex:1;min-width:80px;padding:0.8rem 0.5rem;border-radius:8px;border:2px solid #1e1e1e;background:#0d0d0d;color:#ccc;cursor:pointer;text-align:center;font-size:0.9rem;font-weight:600;transition:all 0.2s;}
.buy-crypto-btn.selected{border-color:#f5a623;color:#f5a623;box-shadow:0 0 10px rgba(245,166,35,0.2);}
.buy-crypto-btn svg{display:block;margin:0 auto 4px;}
.buy-step-card{background:#0d0d0d;border:1px solid #1e1e1e;border-radius:12px;padding:1.5rem;margin-bottom:1.2rem;}
.buy-step-title{font-size:1.1rem;font-weight:700;color:#cffadb;margin-bottom:0.3rem;}
.buy-step-sub{color:#555;font-size:0.82rem;margin-bottom:1.2rem;}
.buy-addr-box{background:#111;border:1px solid #1e1e1e;border-radius:8px;padding:0.9rem 1rem;font-family:monospace;font-size:0.78rem;word-break:break-all;color:#00e85a;margin-bottom:0.8rem;display:flex;align-items:center;gap:0.5rem;justify-content:space-between;}
.buy-copy-btn{background:#1a1a1a;border:1px solid #2a2a2a;color:#888;padding:4px 10px;border-radius:5px;cursor:pointer;font-size:0.75rem;white-space:nowrap;transition:all 0.2s;}
.buy-copy-btn:hover{border-color:#f5a623;color:#f5a623;}
.buy-timer{font-family:monospace;font-size:1.5rem;font-weight:700;color:#00e85a;margin:0.8rem 0;text-align:center;}
.buy-timer.urgent{color:#ff4444;}
.buy-status{text-align:center;padding:0.6rem;border-radius:6px;font-size:0.85rem;margin-top:0.8rem;background:#0a1a0a;color:#7ab98a;}
.buy-status.confirmed{background:#0a2a0a;color:#00e85a;}
.buy-qr{text-align:center;margin:1rem 0;}
.buy-success{text-align:center;padding:2rem 1rem;}
.buy-success-icon{font-size:3rem;margin-bottom:0.5rem;}
.buy-success-title{font-size:1.3rem;font-weight:700;color:#00e85a;margin-bottom:0.5rem;}
.buy-calc{background:#111;border:1px solid #1e1e1e;border-radius:8px;padding:0.75rem 1rem;margin-bottom:1rem;font-size:0.85rem;color:#7ab98a;}
</style>

<!-- STEP 1: Choose crypto + amount -->
<div class="buy-step active" id="buy-step-1">
  <div class="buy-step-card">
    <div class="buy-step-title">Acheter des SCO</div>
    <div class="buy-step-sub" id="buy-minimum-label">1 SCO = 0.07 EUR — Minimum 10 EUR</div>
    <div style="margin-bottom:0.8rem;font-size:0.82rem;color:#555;font-weight:600;text-transform:uppercase;letter-spacing:0.06em;">Choisir la crypto</div>
    <div class="buy-crypto-btns">
      <button class="buy-crypto-btn selected" id="buy-btn-btc" onclick="selectCrypto('btc')">
        <svg width="22" height="22" viewBox="0 0 32 32" fill="none"><circle cx="16" cy="16" r="16" fill="#F7931A"/><path d="M22.5 14.2c.3-2.1-1.3-3.2-3.5-3.9l.7-2.9-1.7-.4-.7 2.8-.9-.2.7-2.8-1.7-.4-.7 2.9-2.2-.5-.5 1.8 1.2.3-.5 2-.1.3.5.1-.5 2.2.5.1-.7 2.9c-.1.3-.4.4-.7.3l-1.2-.3-.8 1.9 2.1.5-.7 2.9 1.7.4.7-2.9.9.2-.7 2.9 1.7.4.7-2.9c3.2.6 5.6.4 6.6-2.5.8-2.3-.04-3.6-1.7-4.5.7-.2 1.5-.7 1.8-1.8zm-3.2 4.5c-.6 2.3-4.4 1.1-5.7.8l1-4c1.3.3 5.3.9 4.7 3.2zm.6-4.5c-.5 2.1-3.7 1-4.8.8l.9-3.6c1.1.3 4.4.8 3.9 2.8z" fill="#fff"/></svg>
        BTC
      </button>
      <button class="buy-crypto-btn" id="buy-btn-doge" onclick="selectCrypto('doge')">
        <svg width="22" height="22" viewBox="0 0 32 32" fill="none"><circle cx="16" cy="16" r="16" fill="#C2A633"/><text x="16" y="21" text-anchor="middle" fill="#fff" font-size="13" font-weight="bold">D</text></svg>
        DOGE
      </button>
      <button class="buy-crypto-btn" id="buy-btn-xrp" onclick="selectCrypto('xrp')">
        <svg width="22" height="22" viewBox="0 0 32 32" fill="none"><circle cx="16" cy="16" r="16" fill="#00AAE4"/><text x="16" y="21" text-anchor="middle" fill="#fff" font-size="11" font-weight="bold">XRP</text></svg>
        XRP
      </button>
    </div>
    <div class="fgroup"><label>Montant en EUR</label><input type="number" id="buy-eur" class="finput" placeholder="50" min="2" value="50" oninput="updateBuyCalc()"></div>
    <div class="buy-calc" id="buy-calc">Chargement des cours...</div>
    <div class="fgroup"><label>Votre adresse SCO</label><input type="text" id="buy-sco-addr" class="finput" placeholder="SCO..." value="%s"></div>
    <div class="fgroup"><label>Email de confirmation</label><input type="email" id="buy-email" class="finput" placeholder="votre@email.com" value="%s"></div>
    <div id="buy-step1-error" class="ferror hidden"></div>
    <button class="btn-main" onclick="submitBuyOrder()" style="background:#f5a623;color:#000;font-weight:700;">Générer l'adresse de paiement</button>
  </div>
</div>

<!-- STEP 2: Payment -->
<div class="buy-step" id="buy-step-2">
  <div class="buy-step-card">
    <div class="buy-step-title">Effectuer le paiement</div>
    <div class="buy-step-sub" id="buy-pay-instruction">Envoyez exactement X crypto à cette adresse.</div>
    <div class="buy-qr"><img id="buy-qr-img" src="" alt="QR Code" width="180" height="180" style="border-radius:8px;"></div>
    <div style="font-size:0.75rem;color:#555;margin-bottom:0.3rem;">Adresse de paiement</div>
    <div class="buy-addr-box">
      <span id="buy-pay-address" style="flex:1;word-break:break-all;"></span>
      <button class="buy-copy-btn" onclick="copyBuyAddr()">Copier</button>
    </div>
    <div id="buy-xrp-memo-wrap" style="display:none;margin-bottom:0.8rem;">
      <div style="font-size:0.75rem;color:#555;margin-bottom:0.3rem;">Mémo / Tag XRP</div>
      <div class="buy-addr-box">
        <span id="buy-pay-memo" style="flex:1;word-break:break-all;font-weight:700;color:#f5a623;"></span>
        <button class="buy-copy-btn" onclick="copyBuyMemo()">Copier</button>
      </div>
      <div style="font-size:0.78rem;color:#e84040;margin-top:0.4rem;font-weight:600;">⚠ IMPORTANT : Ce mémo est obligatoire. Sans lui vos XRP seront perdus.</div>
    </div>
    <div style="font-size:0.75rem;color:#555;margin-bottom:0.3rem;">Montant exact</div>
    <div class="buy-addr-box">
      <span id="buy-pay-amount" style="flex:1;"></span>
      <button class="buy-copy-btn" onclick="copyBuyAmount()">Copier</button>
    </div>
    <div class="buy-timer" id="buy-timer">30:00</div>
    <div class="buy-status" id="buy-pay-status">En attente du paiement...</div>
    <div style="text-align:center;margin-top:1rem;">
      <button onclick="cancelBuyOrder()" style="background:transparent;border:1px solid #444;color:#888;border-radius:6px;padding:.6rem 1.4rem;cursor:pointer;font-size:0.85rem;transition:all .2s;" onmouseover="this.style.borderColor='#888'" onmouseout="this.style.borderColor='#444'">Annuler</button>
    </div>
  </div>
</div>

<!-- STEP 3: Confirmation -->
<div class="buy-step" id="buy-step-3">
  <div class="buy-step-card">
    <div class="buy-success">
      <div class="buy-success-icon">&#x2705;</div>
      <div class="buy-success-title">Achat confirmé !</div>
      <p id="buy-confirm-msg" style="color:#7ab98a;margin-bottom:1rem;"></p>
      <a href="/profile#wallet" class="btn-main" style="display:inline-block;width:auto;padding:10px 24px;">Voir mon wallet</a>
    </div>
  </div>
</div>

</div>
`, user.Address, user.Email)

	return fmt.Sprintf(`<div class="profile-page">
<script src="https://cdnjs.cloudflare.com/ajax/libs/qrcodejs/1.0.0/qrcode.min.js"></script>
<div class="profile-tabs">
  <button class="profile-tab" id="profile-tab-profile" onclick="switchProfileTab('profile')">`+i18n.T(lang, "profile.tab_profile")+`</button>
  <button class="profile-tab" id="profile-tab-wallet" onclick="switchProfileTab('wallet')">`+i18n.T(lang, "profile.tab_wallet")+`</button>
  <button class="profile-tab" id="profile-tab-widget" onclick="switchProfileTab('widget')">&#x1F4CA; Widget</button>
  <button class="profile-tab" id="profile-tab-buy" onclick="switchProfileTab('buy')" style="background:#f5a623;color:#000;font-weight:700;">&#x1F4B0; Buy SCO</button>
</div>

<!-- MY PROFILE TAB -->
<div class="profile-panel" id="profile-panel-profile">

  <div class="profile-section">
    <div class="profile-section-title">`+i18n.T(lang, "profile.photo_section")+`</div>
    <div class="profile-photo-area">
      <div style="position:relative;display:inline-block;">
        <div id="profile-pic-preview">%s</div>
        <label for="pic-file-input" style="position:absolute;bottom:0;right:0;background:#00e85a;border-radius:50%%;width:28px;height:28px;display:flex;align-items:center;justify-content:center;font-size:14px;cursor:pointer;border:2px solid var(--bg);">✎</label>
      </div>
      <div id="profile-pic-new-preview" style="display:none;flex-direction:column;align-items:center;gap:8px;">
        <img id="profile-pic-new-img" src="" width="120" height="120" style="border-radius:50%%;object-fit:cover;">
        <div style="display:flex;gap:8px;">
          <button class="btn-sm" onclick="uploadProfilePic()">`+i18n.T(lang, "profile.confirm_upload")+`</button>
          <button class="btn-sm btn-danger" onclick="cancelPreview()">`+i18n.T(lang, "common.cancel")+`</button>
        </div>
      </div>
      <input type="file" id="pic-file-input" accept="image/jpeg,image/png,image/webp" style="display:none" onchange="previewProfilePic(this)">
      <div style="display:flex;gap:8px;flex-wrap:wrap;justify-content:center;">
        <label for="pic-file-input" class="btn-sm" style="cursor:pointer;">`+i18n.T(lang, "profile.upload_pic")+`</label>
        <button id="pic-delete-btn" class="btn-sm btn-danger" onclick="removeProfilePic()" %s>`+i18n.T(lang, "profile.delete_pic")+`</button>
      </div>
      <div id="pic-msg" style="font-size:0.8rem;min-height:1em;text-align:center;"></div>
    </div>
  </div>

  <div class="profile-section">
    <div class="profile-section-title">`+i18n.T(lang, "profile.change_password")+`</div>
    <div class="fgroup"><label>`+i18n.T(lang, "profile.current_password")+`</label><input type="password" id="pwd-current" class="finput" placeholder="••••••••" autocomplete="current-password"></div>
    <div class="fgroup"><label>`+i18n.T(lang, "profile.new_password")+`</label><input type="password" id="pwd-new" class="finput" placeholder="`+i18n.T(lang, "register.placeholder_pass")+`" autocomplete="new-password"></div>
    <div class="fgroup"><label>`+i18n.T(lang, "register.confirm_password")+`</label><input type="password" id="pwd-confirm" class="finput" placeholder="••••••••" autocomplete="new-password"></div>
    <div id="pwd-msg" style="font-size:0.85rem;min-height:1em;margin-bottom:6px;"></div>
    <button class="btn-main" onclick="changePassword()">`+i18n.T(lang, "profile.save_password")+`</button>
  </div>

  <div class="profile-section">
    <div class="profile-section-title">`+i18n.T(lang, "profile.dm_settings_section")+`</div>
    <p style="color:var(--text2);font-size:0.85rem;margin-bottom:0.8rem;">`+i18n.T(lang, "dm.settings_desc")+`</p>
    <label class="dm-setting-option"><input type="radio" name="profile-dm-policy" value="everyone"><div><span>`+i18n.T(lang, "dm.policy_everyone")+`</span><small>`+i18n.T(lang, "dm.policy_everyone_desc")+`</small></div></label>
    <label class="dm-setting-option"><input type="radio" name="profile-dm-policy" value="initiated"><div><span>`+i18n.T(lang, "dm.policy_initiated")+`</span><small>`+i18n.T(lang, "dm.policy_initiated_desc")+`</small></div></label>
    <label class="dm-setting-option"><input type="radio" name="profile-dm-policy" value="nobody"><div><span>`+i18n.T(lang, "dm.policy_nobody")+`</span><small>`+i18n.T(lang, "dm.policy_nobody_desc")+`</small></div></label>
    <div id="dm-settings-msg" style="font-size:0.85rem;min-height:1em;margin:6px 0;"></div>
    <button class="btn-main" onclick="saveProfileDMSettings()" style="margin-top:0.4rem;">`+i18n.T(lang, "dm.save_settings")+`</button>
  </div>

  <div class="profile-section">
    <div class="profile-section-title">`+i18n.T(lang, "profile.blocked_users_section")+`</div>
    <div id="blocked-users-list"><div class="txlist-load">`+i18n.T(lang, "common.loading")+`</div></div>
  </div>

</div>

<!-- MY WALLET TAB -->
<div class="profile-panel" id="profile-panel-wallet" style="display:none;">

  <div class="wallet-stats">
    <div class="wallet-stat-card">
      <div class="wallet-stat-label">`+i18n.T(lang, "profile.wallet_balance")+`</div>
      <div class="wallet-stat-value" id="ws-balance">%d <span style="font-size:0.9rem;color:var(--text2)">SCO</span></div>
    </div>
    <div class="wallet-stat-card">
      <div class="wallet-stat-label">`+i18n.T(lang, "profile.wallet_blocks_mined")+`</div>
      <div class="wallet-stat-value green" id="ws-blocks">—</div>
    </div>
  </div>

  <div class="wallet-address-box">
    <div class="wallet-address-label">`+i18n.T(lang, "receive.your_address")+`</div>
    <div class="wallet-address-value mono" id="wallet-addr-display">%s</div>
    <button class="btn-main" onclick="copyWalletAddr()" style="margin-top:0.8rem;width:auto;padding:8px 24px;">`+i18n.T(lang, "receive.copy_address")+`</button>
    <div id="wallet-copy-ok" class="copy-confirm hidden">
      <svg width="14" height="14" viewBox="0 0 14 14"><path d="M2 7l3 3 7-7" stroke="#00e85a" stroke-width="2" fill="none" stroke-linecap="round"/></svg>
      `+i18n.T(lang, "receive.address_copied")+`
    </div>
  </div>

  <div class="wallet-sr-grid">
    <div class="wallet-sr-card">
      <div class="wallet-sr-title">`+i18n.T(lang, "wallet.send")+` SCO</div>
      <div class="send-bal" style="margin-bottom:0.8rem;">`+i18n.T(lang, "send.available_balance")+`&#160;: <strong class="green" id="ws-send-balance">%d SCO</strong></div>
      <div class="fgroup"><label>`+i18n.T(lang, "send.recipient")+`</label><input type="text" id="ws-to" class="finput" placeholder="SCO..." autocomplete="off"></div>
      <div class="fgroup"><label>`+i18n.T(lang, "send.amount")+`</label><input type="number" id="ws-amount" class="finput" placeholder="0" min="1" oninput="updateWsFee()"></div>
      <div class="fee-box">`+i18n.T(lang, "send.network_fee")+`&#160;: <strong id="ws-fee-val">1 SCO</strong> — `+i18n.T(lang, "send.treasury")+`&#160;: <strong id="ws-treasury-val">0 SCO</strong> — `+i18n.T(lang, "send.total")+`&#160;: <strong id="ws-total-val">-- SCO</strong></div>
      <div id="ws-error" class="ferror hidden"></div>
      <button class="btn-main" onclick="openWsSendModal()">`+i18n.T(lang, "send.check_send")+`</button>
      <div id="ws-send-result" class="hidden" style="margin-top:1rem;"></div>
    </div>
    <div class="wallet-sr-card">
      <div class="wallet-sr-title">`+i18n.T(lang, "wallet.receive")+` SCO</div>
      <div class="qr-wrap" style="margin:0 auto 1rem;display:table;">
        <div id="wallet-qr"></div>
      </div>
      <div class="recv-addr mono green" style="font-size:0.72rem;word-break:break-all;text-align:center;">%s</div>
    </div>
  </div>

  <div class="profile-section">
    <div class="profile-section-title">`+i18n.T(lang, "history.title")+`</div>
    <div class="txlist" id="ws-hist-list"><div class="txlist-load">`+i18n.T(lang, "common.loading")+`</div></div>
  </div>

  <div id="ws-send-modal" class="modal-overlay hidden" onclick="if(event.target===this)closeWsSendModal()">
    <div class="modal-box">
      <div class="modal-title">`+i18n.T(lang, "send.confirm_title")+`</div>
      <div class="modal-body">
        <div class="cb-row"><span>`+i18n.T(lang, "send.recipient_label")+`</span><span class="mono sm" id="wsm-to"></span></div>
        <div class="cb-row"><span>`+i18n.T(lang, "send.amount_label")+`</span><span class="green fw6" id="wsm-amount"></span></div>
        <div class="cb-row"><span>`+i18n.T(lang, "send.fee_label")+`</span><span id="wsm-fee"></span></div>
        <div class="cb-row"><span>`+i18n.T(lang, "send.treasury_label")+`</span><span id="wsm-treasury"></span></div>
        <div class="cb-row total"><span>`+i18n.T(lang, "send.total_deducted")+`</span><span id="wsm-total"></span></div>
      </div>
      <div class="modal-actions">
        <button class="btn-cancel" onclick="closeWsSendModal()">`+i18n.T(lang, "send.cancel")+`</button>
        <button class="btn-confirm" id="wsm-btn-confirm" onclick="doWsSend()">`+i18n.T(lang, "send.confirm")+`</button>
      </div>
    </div>
  </div>

</div>

`, picHTML, deleteBtnStyle, balance, user.Address, balance, user.Address) +
		widgetPanel +
		buyPanel +
		fmt.Sprintf(`</div>
<script>
// ─── TAB SWITCH ───
function switchProfileTab(tab){
  document.querySelectorAll('.profile-tab').forEach(b=>b.classList.remove('active'));
  document.querySelectorAll('.profile-panel').forEach(c=>c.style.display='none');
  const tabBtn=document.getElementById('profile-tab-'+tab);
  if(tabBtn)tabBtn.classList.add('active');
  const panel=document.getElementById('profile-panel-'+tab);
  if(panel)panel.style.display='';
  history.replaceState(null,'','#'+tab);
  if(tab==='wallet'&&!window._wsLoaded){window._wsLoaded=true;loadWalletTab();}
  if(tab==='profile'&&!window._profileLoaded){window._profileLoaded=true;loadProfileTab();}
  if(tab==='widget'&&!window._widgetLoaded){window._widgetLoaded=true;loadWidgetTab();}
  if(tab==='buy'&&!window._buyLoaded){window._buyLoaded=true;initBuyTab();}
}
window.switchProfileTab=switchProfileTab;
window.addEventListener('DOMContentLoaded',function(){
  const hash=window.location.hash.replace('#','')||'profile';
  switchProfileTab(hash);
});
window.addEventListener('hashchange',function(){
  const hash=window.location.hash.replace('#','')||'profile';
  switchProfileTab(hash);
});

// ─── PROFILE TAB ───
async function loadProfileTab(){
  loadBlockedUsers();
  loadProfileDMPolicy();
}

async function loadProfileDMPolicy(){
  const res=await fetch('/api/dm/settings');
  const d=await res.json();
  const policy=d.policy||'everyone';
  const radios=document.querySelectorAll('input[name="profile-dm-policy"]');
  radios.forEach(r=>{if(r.value===policy)r.checked=true;});
}

async function saveProfileDMSettings(){
  const policy=document.querySelector('input[name="profile-dm-policy"]:checked')?.value;
  if(!policy)return;
  const msg=document.getElementById('dm-settings-msg');
  const res=await fetch('/api/dm/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({policy})});
  const d=await res.json();
  if(d.success){msg.textContent=I18N['dm.save_settings']+'!';msg.style.color='var(--green)';}
  else{msg.textContent=I18N['common.error'];msg.style.color='var(--red,#ff4444)';}
  setTimeout(()=>msg.textContent='',3000);
}

async function loadBlockedUsers(){
  const el=document.getElementById('blocked-users-list');
  try{
    const res=await fetch('/api/profile/blocked-users');
    const users=await res.json();
    if(!users||!users.length){el.innerHTML='<div class="txlist-load">'+I18N['profile.no_blocked_users']+'</div>';return;}
    el.innerHTML=users.map(u=>'<div class="blocked-user-row">'+
      '<span class="blocked-user-avatar">'+u.avatar_svg+'</span>'+
      '<span class="blocked-user-pseudo">@'+u.pseudo+'</span>'+
      '<button class="btn-sm" onclick="unblockUser(\''+u.id+'\',this)">'+I18N['profile.unblock_btn']+'</button>'+
    '</div>').join('');
  }catch(e){el.innerHTML='<div class="txlist-load">'+I18N['common.error']+'</div>';}
}

async function unblockUser(userId,btn){
  btn.disabled=true;
  const res=await fetch('/api/chat/unblock',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user_id:userId})});
  const d=await res.json();
  if(d.success){btn.closest('.blocked-user-row').remove();const el=document.getElementById('blocked-users-list');if(!el.children.length)el.innerHTML='<div class="txlist-load">'+I18N['profile.no_blocked_users']+'</div>';}
  else btn.disabled=false;
}

// ─── PHOTO ───
let _pendingFile=null;
function previewProfilePic(input){
  const file=input.files[0];if(!file)return;
  if(file.size>5*1024*1024){document.getElementById('pic-msg').textContent=I18N['profile.pic_too_large'];document.getElementById('pic-msg').style.color='var(--red,#ff4444)';input.value='';return;}
  _pendingFile=file;
  const reader=new FileReader();
  reader.onload=function(e){
    document.getElementById('profile-pic-new-img').src=e.target.result;
    document.getElementById('profile-pic-new-preview').style.display='flex';
  };
  reader.readAsDataURL(file);
}
function cancelPreview(){
  _pendingFile=null;
  document.getElementById('profile-pic-new-preview').style.display='none';
  document.getElementById('pic-file-input').value='';
}
async function uploadProfilePic(){
  if(!_pendingFile)return;
  const fd=new FormData();fd.append('pic',_pendingFile);
  document.getElementById('pic-msg').textContent=I18N['common.loading'];document.getElementById('pic-msg').style.color='var(--muted)';
  try{
    const res=await fetch('/api/profile/upload-pic',{method:'POST',body:fd});
    const d=await res.json();
    if(d.url){
      document.getElementById('profile-pic-preview').innerHTML='<img src="'+d.url+'?t='+Date.now()+'" width="120" height="120" style="border-radius:50%%;object-fit:cover;display:block;">';
      document.getElementById('pic-delete-btn').style.display='';
      document.getElementById('pic-msg').textContent=I18N['profile.pic_saved'];document.getElementById('pic-msg').style.color='var(--green)';
      cancelPreview();
    } else {
      document.getElementById('pic-msg').textContent=d.error||I18N['common.error'];document.getElementById('pic-msg').style.color='var(--red,#ff4444)';
    }
  }catch(e){document.getElementById('pic-msg').textContent=I18N['common.error'];document.getElementById('pic-msg').style.color='var(--red,#ff4444)';}
  _pendingFile=null;document.getElementById('pic-file-input').value='';
}
async function removeProfilePic(){
  if(!confirm(I18N['profile.confirm_delete_pic']))return;
  const res=await fetch('/api/profile/delete-pic',{method:'DELETE'});
  const d=await res.json();
  if(d.success){
    const pseudo=I18N['_pseudo']||'?';
    const initials=pseudo.substring(0,2).toUpperCase();
    const s=%d;
    document.getElementById('profile-pic-preview').innerHTML='<div style="width:'+s+'px;height:'+s+'px;border-radius:50%%;background:#0d2010;display:inline-flex;align-items:center;justify-content:center;color:#00e85a;font-weight:700;font-size:'+(s/3)+'px;">'+initials+'</div>';
    document.getElementById('pic-delete-btn').style.display='none';
    document.getElementById('pic-msg').textContent=I18N['profile.pic_deleted'];document.getElementById('pic-msg').style.color='var(--muted)';
  }
}

// ─── PASSWORD ───
async function changePassword(){
  const cur=document.getElementById('pwd-current').value;
  const nw=document.getElementById('pwd-new').value;
  const cf=document.getElementById('pwd-confirm').value;
  const msg=document.getElementById('pwd-msg');
  if(!cur||!nw||!cf){msg.textContent=I18N['register.fill_fields'];msg.style.color='var(--red,#ff4444)';return;}
  if(nw!==cf){msg.textContent=I18N['register.passwords_mismatch'];msg.style.color='var(--red,#ff4444)';return;}
  if(nw.length<8){msg.textContent=I18N['register.password_too_short'];msg.style.color='var(--red,#ff4444)';return;}
  msg.textContent=I18N['common.loading'];msg.style.color='var(--muted)';
  const res=await fetch('/api/profile/change-password',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({current:cur,new_password:nw})});
  const d=await res.json();
  if(d.success){msg.textContent=I18N['profile.password_changed'];msg.style.color='var(--green)';document.getElementById('pwd-current').value='';document.getElementById('pwd-new').value='';document.getElementById('pwd-confirm').value='';}
  else{msg.textContent=d.error||I18N['common.error'];msg.style.color='var(--red,#ff4444)';}
}

// ─── WALLET TAB ───
async function loadWalletTab(){
  try{
    const res=await fetch('/api/profile/%s');
    const p=await res.json();
    if(p.blocks_mined!==undefined)document.getElementById('ws-blocks').textContent=p.blocks_mined;
  }catch(e){}
  loadWalletHistory();
  try{
    new QRCode(document.getElementById('wallet-qr'),{text:'%s',width:140,height:140,colorDark:'#00e85a',colorLight:'#ffffff',correctLevel:QRCode.CorrectLevel.M});
  }catch(e){}
}

function copyWalletAddr(){
  navigator.clipboard.writeText('%s').then(()=>{
    const ok=document.getElementById('wallet-copy-ok');
    ok.classList.remove('hidden');
    setTimeout(()=>ok.classList.add('hidden'),2500);
  });
}

async function loadWalletHistory(){
  const el=document.getElementById('ws-hist-list');
  try{
    const res=await fetch('/api/history/%s');
    const txs=await res.json()||[];
    const mined=txs.filter(t=>t.type==='minage');
    if(!mined.length){el.innerHTML='<div class="txlist-load">'+I18N['history.no_tx']+'</div>';return;}
    el.innerHTML=mined.map(t=>{
      const d=new Date(t.timestamp*1000).toLocaleString(I18N['common.locale'],{day:'2-digit',month:'2-digit',year:'numeric',hour:'2-digit',minute:'2-digit'});
      const icon='<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/></svg>';
      return '<div class="tx-row" onclick="window.location=\'\/block\/'+t.block+'\'">'+
        '<div class="tx-icon mine">'+icon+'</div>'+
        '<div class="tx-info"><div class="tx-label">'+I18N['history.mined_block']+t.block+'</div><div class="tx-meta">'+d+'</div></div>'+
        '<div class="tx-right"><div class="tx-amt pos">+'+t.amount+' SCO</div><div class="tx-date">Bloc #'+t.block+'</div></div>'+
      '</div>';
    }).join('');
  }catch(e){el.innerHTML='<div class="txlist-load">'+I18N['common.error']+'</div>';}
}

// ─── WALLET SEND ───
let wsFee=1;
function updateWsFee(){
  const amt=parseInt(document.getElementById('ws-amount').value)||0;
  wsFee=amt>100?2:1;
  const treasury=Math.floor(amt*0.001);
  document.getElementById('ws-fee-val').textContent=wsFee+' SCO';
  document.getElementById('ws-treasury-val').textContent=treasury+' SCO';
  document.getElementById('ws-total-val').textContent=amt>0?(amt+wsFee+treasury)+' SCO':'-- SCO';
}
function openWsSendModal(){
  const to=document.getElementById('ws-to').value.trim();
  const amt=parseInt(document.getElementById('ws-amount').value)||0;
  const err=document.getElementById('ws-error');
  err.classList.add('hidden');
  if(!to.startsWith('SCO')){err.textContent=I18N['send.invalid_address'];err.classList.remove('hidden');return;}
  if(amt<=0){err.textContent=I18N['send.invalid_amount'];err.classList.remove('hidden');return;}
  const treasury=Math.floor(amt*0.001);
  if(amt+wsFee+treasury>%d){err.textContent=I18N['send.insufficient_balance'];err.classList.remove('hidden');return;}
  document.getElementById('wsm-to').textContent=to.length>20?to.substring(0,16)+'...':to;
  document.getElementById('wsm-amount').textContent=amt+' SCO';
  document.getElementById('wsm-fee').textContent=wsFee+' SCO';
  document.getElementById('wsm-treasury').textContent=treasury+' SCO';
  document.getElementById('wsm-total').textContent=(amt+wsFee+treasury)+' SCO';
  document.getElementById('ws-send-modal').classList.remove('hidden');
}
function closeWsSendModal(){document.getElementById('ws-send-modal').classList.add('hidden');}

// ─── WIDGET TAB ───
let wgAllTx=[],wgFiltered=[],wgPeriod='all',wgTypeFilter='all',wgPage=0,wgChart=null;
const WG_PAGE_SIZE=20;
async function loadWidgetTab(){
  try{
    const res=await fetch('/api/history/%s');
    wgAllTx=await res.json()||[];
    wgAllTx.sort((a,b)=>a.timestamp-b.timestamp);
    let received=0,sent=0,minedCount=0,minedSCO=0;
    wgAllTx.forEach(t=>{
      if(t.type==='minage'){minedCount++;minedSCO+=t.amount;}
      else if(t.type==='envoi')sent+=t.amount;
      else received+=t.amount;
    });
    document.getElementById('wg-balance').textContent='%d SCO';
    document.getElementById('wg-received').textContent=received.toLocaleString()+' SCO';
    document.getElementById('wg-sent').textContent=sent.toLocaleString()+' SCO';
    document.getElementById('wg-mined').textContent=minedCount+' `+wgtl("blocs", "blocks")+` · '+minedSCO.toLocaleString()+' SCO';
    wgRenderChart();
    wgApplyFilters();
  }catch(e){console.error(e);}
}
function setWgPeriod(p,btn){
  wgPeriod=p;
  document.querySelectorAll('.wg-period-btn').forEach(b=>b.classList.remove('active'));
  btn.classList.add('active');
  wgRenderChart();
}
function setWgFilter(f,btn){
  wgTypeFilter=f;wgPage=0;
  document.querySelectorAll('.wg-type-btn').forEach(b=>b.classList.remove('active'));
  btn.classList.add('active');
  wgApplyFilters();
}
function wgApplyFilters(){
  let filtered=wgTypeFilter==='all'?[...wgAllTx]:wgAllTx.filter(t=>{
    if(wgTypeFilter==='reception') return t.type==='réception'||t.type==='reception'||t.type==='premine';
    return t.type===wgTypeFilter;
  });
  wgFiltered=filtered.slice().reverse();
  wgRenderTable();
}
function wgRenderTable(){
  const start=wgPage*WG_PAGE_SIZE;
  const page=wgFiltered.slice(start,start+WG_PAGE_SIZE);
  const tbody=document.getElementById('wg-tbody');
  if(!tbody)return;
  if(!wgFiltered.length){
    tbody.innerHTML='<tr><td colspan="5" style="text-align:center;color:#555;padding:2rem;">`+wgtl("Aucune transaction", "No transactions")+`</td></tr>';
    document.getElementById('wg-page-info').textContent='';
    document.getElementById('wg-prev').disabled=true;
    document.getElementById('wg-next').disabled=true;
    return;
  }
  const badgeLabel={minage:'`+wgtl("Minage", "Mining")+`',réception:'`+wgtl("Reçu", "Received")+`',reception:'`+wgtl("Reçu", "Received")+`',envoi:'`+wgtl("Envoi", "Sent")+`',premine:'Premine'};
  const badgeCls={minage:'minage',réception:'reception',reception:'reception',envoi:'envoi',premine:'reception'};
  tbody.innerHTML=page.map(t=>{
    const d=new Date(t.timestamp*1000).toLocaleString([],{day:'2-digit',month:'2-digit',year:'2-digit',hour:'2-digit',minute:'2-digit'});
    const issend=t.type==='envoi';
    const cls=badgeCls[t.type]||'minage';
    const badge='<span class="wg-badge '+cls+'">'+(badgeLabel[t.type]||t.type)+'</span>';
    const amt='<span class="'+(issend?'wg-amt-neg':'wg-amt-pos')+'">'+(issend?'-':'+')+t.amount.toLocaleString()+' SCO</span>';
    const hashShort=t.hash?t.hash.substring(0,14)+'…':'—';
    const hashLink=t.hash?'<a class="wg-hash" href="/block/'+t.block+'" onclick="event.stopPropagation()">'+hashShort+'</a>':'—';
    return '<tr onclick="window.location=\'/block/'+t.block+'\'">'+'<td>'+d+'</td><td>'+badge+'</td><td>'+amt+'</td><td style="color:#555">#'+t.block+'</td><td>'+hashLink+'</td></tr>';
  }).join('');
  const totalPages=Math.max(1,Math.ceil(wgFiltered.length/WG_PAGE_SIZE));
  document.getElementById('wg-page-info').textContent=(wgPage+1)+' / '+totalPages;
  document.getElementById('wg-prev').disabled=wgPage===0;
  document.getElementById('wg-next').disabled=wgPage>=totalPages-1;
}
function wgChangePage(dir){
  const totalPages=Math.max(1,Math.ceil(wgFiltered.length/WG_PAGE_SIZE));
  wgPage=Math.max(0,Math.min(wgPage+dir,totalPages-1));
  wgRenderTable();
}
function wgRenderChart(){
  const periods={' 1d':86400,'1w':604800,'1m':2592000,'1y':31536000,'all':Infinity};
  const now=Date.now()/1000;
  const since=wgPeriod==='all'?0:now-(periods[wgPeriod]||Infinity);
  let runBal=0;
  const allPoints=[];
  if(wgAllTx.length) allPoints.push({ts:wgAllTx[0].timestamp,bal:0});
  wgAllTx.forEach(t=>{
    runBal+=t.type==='envoi'?-t.amount:t.amount;
    allPoints.push({ts:t.timestamp,bal:runBal});
  });
  const pts=allPoints.filter(p=>p.ts>=since);
  if(!pts.length&&allPoints.length) pts.push(...allPoints.slice(-1));
  const labels=pts.map(p=>{
    const d=new Date(p.ts*1000);
    return wgPeriod==='1d'?d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'}):d.toLocaleDateString([],{day:'2-digit',month:'2-digit'});
  });
  const data=pts.map(p=>p.bal);
  const ctx=document.getElementById('wg-chart');
  if(!ctx)return;
  if(wgChart)wgChart.destroy();
  wgChart=new Chart(ctx,{type:'line',data:{labels,datasets:[{data,borderColor:'#00e85a',borderWidth:2,pointRadius:pts.length>60?0:3,pointHoverRadius:5,fill:true,backgroundColor:function(c){const g=c.chart.ctx.createLinearGradient(0,0,0,220);g.addColorStop(0,'rgba(0,232,90,0.13)');g.addColorStop(1,'rgba(0,232,90,0)');return g;},tension:0.35}]},options:{responsive:true,maintainAspectRatio:false,plugins:{legend:{display:false},tooltip:{callbacks:{label:function(c){return c.parsed.y.toLocaleString()+' SCO';}}}},scales:{x:{grid:{color:'#1a1a1a'},ticks:{color:'#555',maxTicksLimit:8,font:{size:11}}},y:{grid:{color:'#1a1a1a'},ticks:{color:'#555',font:{size:11},callback:function(v){return v.toLocaleString()+' SCO';}}}}}});
}

async function doWsSend(){
  const btn=document.getElementById('wsm-btn-confirm');
  btn.disabled=true;btn.textContent=I18N['send.sending'];
  const to=document.getElementById('ws-to').value.trim();
  const amt=parseInt(document.getElementById('ws-amount').value)||0;
  const res=await fetch('/api/wallet/send',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({to,amount:amt})});
  const data=await res.json();
  closeWsSendModal();
  if(data.success){
    document.getElementById('ws-send-result').innerHTML='<div class="success-card"><div class="success-title">'+I18N['send.success_title']+'</div><div class="success-txid">TX&#160;: '+data.tx_id+'</div><div class="success-info">'+I18N['send.confirmed_next_block']+'</div></div>';
    document.getElementById('ws-send-result').classList.remove('hidden');
    document.getElementById('ws-to').value='';document.getElementById('ws-amount').value='';
  }else{
    const err=document.getElementById('ws-error');
    err.textContent=data.error||I18N['send.error'];
    err.classList.remove('hidden');
  }
  btn.disabled=false;btn.textContent=I18N['send.confirm'];
}

// ─── BUY SCO TAB ───
var buyCrypto='btc';
var buyRates={btc:0,doge:0,xrp:0};
var buyOrderId=null;
var buyTimerInterval=null;
var buyStatusInterval=null;

function initBuyTab(){
  fetch('/api/sco/price').then(r=>r.json()).then(d=>{
    buyRates=d;
    updateBuyCalc();
  }).catch(()=>{});
}
window.initBuyTab=initBuyTab;

function selectCrypto(c){
  buyCrypto=c;
  document.querySelectorAll('.buy-crypto-btn').forEach(b=>b.classList.remove('selected'));
  document.getElementById('buy-btn-'+c).classList.add('selected');
  const minEur=c==='btc'?10:2;
  document.getElementById('buy-minimum-label').textContent='1 SCO = 0.07 EUR — Minimum '+minEur+' EUR';
  const inp=document.getElementById('buy-eur');
  inp.min=minEur;
  inp.placeholder=c==='btc'?'50':'5';
  updateBuyCalc();
}
window.selectCrypto=selectCrypto;

function updateBuyCalc(){
  const eur=parseFloat(document.getElementById('buy-eur').value)||0;
  const rate=buyRates[buyCrypto]||0;
  const sco=eur/0.07;
  const calc=document.getElementById('buy-calc');
  if(rate>0&&eur>0){
    const cryptoAmt=(eur*rate).toFixed(8);
    calc.textContent='Vous recevrez '+sco.toFixed(0)+' SCO — Montant à payer : '+cryptoAmt+' '+buyCrypto.toUpperCase();
  } else {
    calc.textContent='Entrez un montant pour voir le calcul.';
  }
}
window.updateBuyCalc=updateBuyCalc;

async function submitBuyOrder(){
  const eur=parseFloat(document.getElementById('buy-eur').value)||0;
  const scoAddr=document.getElementById('buy-sco-addr').value.trim();
  const emailVal=document.getElementById('buy-email').value.trim();
  const err=document.getElementById('buy-step1-error');
  err.classList.add('hidden');
  const minEur=buyCrypto==='btc'?10:2;
  if(eur<minEur){err.textContent='Minimum '+minEur+' EUR pour '+buyCrypto.toUpperCase()+'.';err.classList.remove('hidden');return;}
  if(!scoAddr.startsWith('SCO')){err.textContent='Adresse SCO invalide.';err.classList.remove('hidden');return;}
  if(!emailVal.includes('@')){err.textContent='Email invalide.';err.classList.remove('hidden');return;}
  const res=await fetch('/api/sco/order',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({crypto:buyCrypto,amount_eur:eur,email:emailVal,sco_address:scoAddr})});
  const order=await res.json();
  if(order.error){err.textContent=order.error;err.classList.remove('hidden');return;}
  buyOrderId=order.id;
  const cryptoAmt=order.amount_crypto.toFixed(8);
  document.getElementById('buy-pay-instruction').textContent='Envoyez exactement '+cryptoAmt+' '+buyCrypto.toUpperCase()+' à cette adresse. Le paiement est détecté automatiquement.';
  document.getElementById('buy-pay-address').textContent=order.address_to_pay;
  document.getElementById('buy-pay-amount').textContent=cryptoAmt+' '+buyCrypto.toUpperCase();
  document.getElementById('buy-qr-img').src='https://api.qrserver.com/v1/create-qr-code/?size=180x180&data='+encodeURIComponent(order.address_to_pay);
  const memoWrap=document.getElementById('buy-xrp-memo-wrap');
  const memoEl=document.getElementById('buy-pay-memo');
  if(order.memo&&memoWrap&&memoEl){memoEl.textContent=order.memo;memoWrap.style.display='block';}
  else if(memoWrap){memoWrap.style.display='none';}
  showBuyStep(2);
  startBuyTimer(order.expires_at);
  startBuyStatusPolling(order.id,order.sco_amount);
}
window.submitBuyOrder=submitBuyOrder;

function showBuyStep(n){
  document.querySelectorAll('.buy-step').forEach(s=>s.classList.remove('active'));
  document.getElementById('buy-step-'+n).classList.add('active');
}

function startBuyTimer(expiresAt){
  if(buyTimerInterval)clearInterval(buyTimerInterval);
  const expMs=new Date(expiresAt).getTime();
  buyTimerInterval=setInterval(()=>{
    const left=Math.max(0,Math.floor((expMs-Date.now())/1000));
    const m=Math.floor(left/60);const s=left%%60;
    const el=document.getElementById('buy-timer');
    el.textContent=(m<10?'0':'')+m+':'+(s<10?'0':'')+s;
    el.className='buy-timer'+(left<300?' urgent':'');
    if(left===0)clearInterval(buyTimerInterval);
  },1000);
}

function startBuyStatusPolling(orderId,scoAmount){
  if(buyStatusInterval)clearInterval(buyStatusInterval);
  buyStatusInterval=setInterval(async()=>{
    try{
      const r=await fetch('/api/sco/order/'+orderId);
      const o=await r.json();
      const el=document.getElementById('buy-pay-status');
      if(o.status==='pending'){el.textContent='En attente du paiement...';el.className='buy-status';}
      else if(o.status==='confirmed'){el.textContent='Paiement détecté ! Envoi des SCO...';el.className='buy-status confirmed';}
      else if(o.status==='sent'){
        clearInterval(buyStatusInterval);
        clearInterval(buyTimerInterval);
        document.getElementById('buy-confirm-msg').textContent=''+Math.floor(scoAmount)+' SCO ont été envoyés à votre adresse SCO.';
        showBuyStep(3);
      } else if(o.status==='expired'){
        clearInterval(buyStatusInterval);
        el.textContent='Commande expirée. Veuillez recommencer.';el.className='buy-status';
      }
    }catch(e){}
  },30000);
}

function copyBuyAddr(){
  const addr=document.getElementById('buy-pay-address').textContent;
  navigator.clipboard.writeText(addr).then(()=>{}).catch(()=>{});
}
window.copyBuyAddr=copyBuyAddr;
function copyBuyMemo(){
  const memo=document.getElementById('buy-pay-memo').textContent;
  navigator.clipboard.writeText(memo).then(()=>{}).catch(()=>{});
}
window.copyBuyMemo=copyBuyMemo;

function copyBuyAmount(){
  const amt=document.getElementById('buy-pay-amount').textContent;
  navigator.clipboard.writeText(amt.split(' ')[0]).then(()=>{}).catch(()=>{});
}
window.copyBuyAmount=copyBuyAmount;

async function cancelBuyOrder(){
  if(buyStatusInterval)clearInterval(buyStatusInterval);
  if(buyTimerInterval)clearInterval(buyTimerInterval);
  if(buyOrderId){
    try{await fetch('/api/sco/cancel',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({order_id:buyOrderId})});}catch(e){}
    buyOrderId=null;
  }
  document.getElementById('buy-timer').textContent='30:00';
  document.getElementById('buy-timer').className='buy-timer';
  document.getElementById('buy-pay-status').textContent='En attente du paiement...';
  document.getElementById('buy-pay-status').className='buy-status';
  showBuyStep(1);
}
window.cancelBuyOrder=cancelBuyOrder;
</script>`, 120, user.Pseudo, user.Address, user.Address, user.Address, balance, user.Address, balance)
}

func mineHTML(user *db.User, lang string) string {
	fr := lang == "fr"
	tl := func(frStr, enStr string) string {
		if fr {
			return frStr
		}
		return enStr
	}
	walletHint := tl(
		`Créez un <a href="/wallet/register" style="color:#f5a623;text-decoration:none;border-bottom:1px solid rgba(245,166,35,0.3);">wallet SCO gratuit</a> pour recevoir vos récompenses de minage.`,
		`<a href="/wallet/register" style="color:#f5a623;text-decoration:none;border-bottom:1px solid rgba(245,166,35,0.3);">Create a free SCO wallet</a> to receive your mining rewards.`,
	)
	if user != nil {
		walletHint = tl("Minage vers : ", "Mining to: ") + `<span style="font-family:'Courier Prime',monospace;color:#f5a623;">` + user.Address + `</span>`
	}
	mineContent := `<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Crimson+Pro:wght@400;600;700&family=Courier+Prime:wght@400;700&display=swap" rel="stylesheet">
<style>
.mp-wrap{max-width:1100px;margin:0 auto;padding:60px 24px 80px;}
.mp-header{text-align:center;margin-bottom:56px;}
.mp-title{font-family:'Crimson Pro',serif;font-size:3.2rem;font-weight:700;color:#f5f0e8;letter-spacing:-0.01em;margin-bottom:10px;}
.mp-subtitle{color:rgba(245,240,232,0.5);font-size:1.05rem;margin-bottom:32px;}
.mp-stats{display:inline-flex;gap:32px;background:rgba(245,166,35,0.06);border:1px solid rgba(245,166,35,0.15);border-radius:8px;padding:14px 28px;}
.mp-stat{text-align:center;}
.mp-stat-label{font-size:10px;text-transform:uppercase;letter-spacing:0.1em;color:rgba(245,240,232,0.35);margin-bottom:4px;}
.mp-stat-val{font-family:'Courier Prime',monospace;font-size:1.05rem;font-weight:700;color:#f5a623;}
.mp-cards{display:grid;grid-template-columns:repeat(3,1fr);gap:20px;margin-bottom:64px;}
@media(max-width:768px){.mp-cards{grid-template-columns:1fr;}.mp-stats{flex-direction:column;gap:12px;}.mp-title{font-size:2.2rem;}}
.mp-card{background:rgba(255,255,255,0.025);border:1px solid rgba(245,166,35,0.15);border-radius:12px;padding:32px 24px;display:flex;flex-direction:column;align-items:center;gap:16px;transition:border-color 0.2s,box-shadow 0.2s;animation:fadeInUp 0.5s ease both;}
.mp-card:nth-child(2){animation-delay:0.08s;}
.mp-card:nth-child(3){animation-delay:0.16s;}
.mp-card:hover{border-color:rgba(245,166,35,0.5);box-shadow:0 0 24px rgba(245,166,35,0.06);}
@keyframes fadeInUp{from{opacity:0;transform:translateY(18px);}to{opacity:1;transform:translateY(0);}}
.mp-os-icon{width:52px;height:52px;opacity:0.85;}
.mp-os-name{font-family:'Crimson Pro',serif;font-size:1.45rem;font-weight:700;color:#f5f0e8;}
.mp-os-arch{font-size:11px;font-family:'Courier Prime',monospace;color:rgba(245,240,232,0.35);letter-spacing:0.05em;margin-top:-10px;}
.mp-dl-btn{display:inline-flex;align-items:center;gap:8px;padding:11px 22px;background:#f5a623;color:#0a0a0f;font-weight:700;font-size:0.92rem;border-radius:4px;text-decoration:none;transition:filter 0.15s,transform 0.15s;margin-top:4px;width:100%;justify-content:center;}
.mp-dl-btn:hover{filter:brightness(1.12);transform:translateY(-1px);}
.mp-steps{margin-top:10px;width:100%;}
.mp-step{display:flex;align-items:flex-start;gap:10px;margin-bottom:8px;}
.mp-step-num{width:20px;height:20px;border-radius:50%;background:rgba(245,166,35,0.15);color:#f5a623;font-size:11px;font-weight:700;display:flex;align-items:center;justify-content:center;flex-shrink:0;margin-top:1px;}
.mp-step-text{font-size:0.82rem;color:rgba(245,240,232,0.55);line-height:1.45;}
.mp-step-text code{font-family:'Courier Prime',monospace;background:rgba(245,166,35,0.1);color:#f5a623;padding:1px 5px;border-radius:3px;font-size:0.78rem;}
.mp-how{margin-bottom:64px;}
.mp-how-title{font-family:'Crimson Pro',serif;font-size:1.8rem;font-weight:700;color:#f5f0e8;text-align:center;margin-bottom:32px;}
.mp-how-steps{display:grid;grid-template-columns:repeat(3,1fr);gap:20px;}
@media(max-width:768px){.mp-how-steps{grid-template-columns:1fr;}}
.mp-how-step{text-align:center;padding:24px 16px;background:rgba(255,255,255,0.02);border:1px solid rgba(255,255,255,0.06);border-radius:10px;}
.mp-how-icon{margin:0 auto 14px;width:40px;height:40px;color:rgba(245,166,35,0.7);}
.mp-how-label{font-family:'Crimson Pro',serif;font-size:1.1rem;font-weight:600;color:#f5f0e8;margin-bottom:6px;}
.mp-how-desc{font-size:0.82rem;color:rgba(245,240,232,0.4);line-height:1.5;}
.mp-wallet{text-align:center;padding:24px;background:rgba(245,166,35,0.04);border:1px solid rgba(245,166,35,0.1);border-radius:8px;margin-bottom:48px;font-size:0.9rem;color:rgba(245,240,232,0.5);line-height:1.6;}
.mp-footer{text-align:center;font-size:0.78rem;color:rgba(245,240,232,0.2);letter-spacing:0.06em;text-transform:uppercase;}
</style>
<div class="mp-wrap">
  <div class="mp-header">
    <h1 class="mp-title">Mine SCO</h1>
    <p class="mp-subtitle">Proof of Work &mdash; SHA-256 &mdash; ` + tl("Récompense : 11 SCO par bloc", "Reward: 11 SCO per block") + `</p>
    <div class="mp-stats" id="mp-stats">
      <div class="mp-stat"><div class="mp-stat-label">` + tl("Difficulté", "Difficulty") + `</div><div class="mp-stat-val" id="st-diff">—</div></div>
      <div class="mp-stat"><div class="mp-stat-label">` + tl("Dernier bloc", "Last Block") + `</div><div class="mp-stat-val" id="st-block">—</div></div>
      <div class="mp-stat"><div class="mp-stat-label">Supply</div><div class="mp-stat-val" id="st-supply">—</div></div>
    </div>
  </div>

  <div class="mp-cards">
    <div class="mp-card">
      <svg class="mp-os-icon" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
        <path d="M3 5.5L11 4v7.5H3V5.5zM12 3.8L21 2.5V11.5H12V3.8zM3 12.5H11V20L3 18.5V12.5zM12 12.5H21V21.5L12 20V12.5z" fill="rgba(245,166,35,0.85)"/>
      </svg>
      <div class="mp-os-name">Windows</div>
      <div class="mp-os-arch">amd64 &mdash; x86_64</div>
      <a href="/static/miners/scorbits-miner-windows.zip" download class="mp-dl-btn">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 3v13M5 16l7 7 7-7"/><path d="M3 21h18"/></svg>
        ` + tl("Télécharger pour Windows", "Download for Windows") + `
      </a>
      <div class="mp-steps">
        <div class="mp-step"><div class="mp-step-num">1</div><div class="mp-step-text">` + tl("Extrayez le ZIP et double-cliquez sur", "Extract the ZIP and double-click") + ` <code>start-mining.bat</code></div></div>
        <div class="mp-step"><div class="mp-step-num">2</div><div class="mp-step-text">` + tl("Entrez votre adresse SCO quand demandé", "Enter your SCO address when prompted") + `</div></div>
        <div class="mp-step"><div class="mp-step-num">3</div><div class="mp-step-text">` + tl("Le mineur se connecte et commence à gagner des SCO", "The miner connects and starts earning SCO") + `</div></div>
      </div>
    </div>

    <div class="mp-card">
      <svg class="mp-os-icon" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
        <ellipse cx="12" cy="12" rx="9" ry="9" stroke="rgba(245,166,35,0.85)" stroke-width="1.5"/>
        <circle cx="12" cy="12" r="3" fill="rgba(245,166,35,0.85)"/>
        <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1" stroke="rgba(245,166,35,0.85)" stroke-width="1.5" stroke-linecap="round"/>
      </svg>
      <div class="mp-os-name">Linux</div>
      <div class="mp-os-arch">amd64 &mdash; x86_64</div>
      <a href="/static/miners/scorbits-miner-linux.zip" download class="mp-dl-btn">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 3v13M5 16l7 7 7-7"/><path d="M3 21h18"/></svg>
        ` + tl("Télécharger pour Linux", "Download for Linux") + `
      </a>
      <div class="mp-steps">
        <div class="mp-step"><div class="mp-step-num">1</div><div class="mp-step-text">` + tl("Extrayez le ZIP, puis lancez", "Extract the ZIP, then run") + ` <code>bash start-mining.sh</code></div></div>
        <div class="mp-step"><div class="mp-step-num">2</div><div class="mp-step-text">` + tl("Entrez votre adresse SCO quand demandé", "Enter your SCO address when prompted") + `</div></div>
        <div class="mp-step"><div class="mp-step-num">3</div><div class="mp-step-text">` + tl("Le mineur se connecte et commence à gagner des SCO", "The miner connects and starts earning SCO") + `</div></div>
      </div>
    </div>

    <div class="mp-card">
      <svg class="mp-os-icon" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
        <path d="M12 3C9.5 3 7.5 4.5 6.5 6.5C5 6.2 3 7 2.5 9C2 11 3 13 4.5 13.5C4.2 15 5 17 6.5 18C7.5 19.5 9.5 21 12 21C14.5 21 16.5 19.5 17.5 18C19 17 19.8 15 19.5 13.5C21 13 22 11 21.5 9C21 7 19 6.2 17.5 6.5C16.5 4.5 14.5 3 12 3Z" stroke="rgba(245,166,35,0.85)" stroke-width="1.5" fill="rgba(245,166,35,0.12)"/>
        <path d="M12 7v10M8 9.5C9 8.5 10.5 8 12 8s3 .5 4 1.5" stroke="rgba(245,166,35,0.85)" stroke-width="1.5" stroke-linecap="round"/>
      </svg>
      <div class="mp-os-name">macOS</div>
      <div class="mp-os-arch">amd64 &mdash; Intel &amp; Rosetta 2</div>
      <a href="/static/miners/scorbits-miner-macos.zip" download class="mp-dl-btn">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 3v13M5 16l7 7 7-7"/><path d="M3 21h18"/></svg>
        ` + tl("Télécharger pour macOS", "Download for macOS") + `
      </a>
      <div class="mp-steps">
        <div class="mp-step"><div class="mp-step-num">1</div><div class="mp-step-text">` + tl("Extrayez le ZIP, puis lancez", "Extract the ZIP, then run") + ` <code>bash start-mining.sh</code></div></div>
        <div class="mp-step"><div class="mp-step-num">2</div><div class="mp-step-text">` + tl("Entrez votre adresse SCO quand demandé", "Enter your SCO address when prompted") + `</div></div>
        <div class="mp-step"><div class="mp-step-num">3</div><div class="mp-step-text">` + tl("Si bloqué par Gatekeeper, autorisez-le dans les Réglages Système", "If blocked by Gatekeeper, allow it in System Settings") + `</div></div>
      </div>
    </div>
  </div>

  <div class="mp-how">
    <h2 class="mp-how-title">` + tl("Comment ça marche", "How it works") + `</h2>
    <div class="mp-how-steps">
      <div class="mp-how-step">
        <svg class="mp-how-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M12 3v13M5 16l7 7 7-7"/><path d="M3 21h18"/></svg>
        <div class="mp-how-label">` + tl("Télécharger", "Download") + `</div>
        <div class="mp-how-desc">` + tl("Téléchargez le mineur pour votre OS. Aucune installation — un seul fichier.", "Get the miner for your OS. No installation required — a single binary.") + `</div>
      </div>
      <div class="mp-how-step">
        <svg class="mp-how-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M7 8h10M7 12h6M7 16h8"/></svg>
        <div class="mp-how-label">` + tl("Configurer", "Configure") + `</div>
        <div class="mp-how-desc">` + tl("Lancez le script et entrez votre adresse SCO. C'est tout.", "Run the launcher script and enter your SCO address. That's all.") + `</div>
      </div>
      <div class="mp-how-step">
        <svg class="mp-how-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="9"/><path d="M12 6v6l4 2"/></svg>
        <div class="mp-how-label">` + tl("Miner", "Mine") + `</div>
        <div class="mp-how-desc">` + tl("Le mineur tourne en continu. Gagnez 11 SCO pour chaque bloc trouvé.", "The miner works continuously. Earn 11 SCO for every block you find.") + `</div>
      </div>
    </div>
  </div>

  <div class="mp-wallet">` + walletHint + `</div>

  <div class="mp-footer">Scorbits (SCO) &mdash; Proof of Work &mdash; 2026</div>
</div>
<script>
(async function(){
  try{
    var s=await fetch('/api/stats').then(r=>r.json());
    document.getElementById('st-diff').textContent=s.difficulty||'—';
    document.getElementById('st-block').textContent='#'+(s.last_block||0);
    var sup=s.total_supply||0;
    document.getElementById('st-supply').textContent=(sup/1).toLocaleString('en-US')+' SCO';
  }catch(e){}
})();
</script>`
	return `<style>
.tab-bar{display:flex;gap:0;border-bottom:1px solid var(--border);margin-bottom:2rem;}
.tab-btn{padding:.75rem 2rem;background:transparent;border:none;color:#888;font-size:1rem;font-weight:600;cursor:pointer;border-bottom:2px solid transparent;transition:all .2s;}
.tab-btn.active{color:var(--green);border-bottom-color:var(--green);}
.tab-btn:hover{color:#eee;}
.tab-content{display:none;}
.tab-content.active{display:block;}
</style>
<div class="tab-bar">
  <button class="tab-btn" data-tab="mine" onclick="switchTab('mine')">&#x26CF; Mine</button>
  <button class="tab-btn" data-tab="node" onclick="switchTab('node')">&#x1F5A7; Node</button>
</div>
<div id="tab-mine" class="tab-content">` + mineContent + `</div>
<div id="tab-node" class="tab-content">` + nodeHTML(lang) + `</div>
<script>
function switchTab(tab) {
  document.querySelectorAll('.tab-btn').forEach(b => b.classList.toggle('active', b.dataset.tab === tab));
  document.querySelectorAll('.tab-content').forEach(c => c.classList.toggle('active', c.id === 'tab-' + tab));
  location.hash = tab;
}
const hash = location.hash.replace('#','') || 'mine';
switchTab(hash);
</script>`
}

// ─── HTML PAGES ───────────────────────────────────────────────────────────────

func loginHTML(lang string) string {
	return `
<div class="auth-wrap">
  <div class="auth-box">
    <div style="text-align:center;margin-bottom:24px;">
      <img src="/static/scorbits_logo.png" alt="Scorbits" style="height:72px;width:auto;">
    </div>
    <div class="auth-title">` + i18n.T(lang, "login.title") + `</div>
    <div class="auth-sub">` + i18n.T(lang, "login.subtitle") + `</div>
    <div class="fgroup"><label>` + i18n.T(lang, "login.email") + `</label><input type="email" id="l-email" class="finput" placeholder="` + i18n.T(lang, "login.placeholder_email") + `" autocomplete="email"></div>
    <div class="fgroup"><label>` + i18n.T(lang, "login.password") + `</label><input type="password" id="l-pass" class="finput" placeholder="` + i18n.T(lang, "login.placeholder_pass") + `" autocomplete="current-password"></div>
    <div id="l-error" class="ferror hidden"></div>
    <button class="btn-main" onclick="doLogin()">` + i18n.T(lang, "login.btn") + `</button>
    <div class="auth-switch">` + i18n.T(lang, "login.no_account") + ` <a href="/wallet/register">` + i18n.T(lang, "login.create") + `</a></div>
  </div>
</div>
<script>
async function doLogin() {
  const email = document.getElementById('l-email').value.trim();
  const pass = document.getElementById('l-pass').value;
  if (!email || !pass) { showErr('l-error',I18N['login.fill_fields']); return; }
  const res = await fetch('/api/auth/login', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({email,password:pass})});
  const data = await res.json();
  if (data.success) { window.location = '/wallet/dashboard'; }
  else { showErr('l-error', data.error || I18N['login.error']); }
}
function showErr(id, msg) { const el = document.getElementById(id); el.textContent = msg; el.classList.remove('hidden'); }
document.addEventListener('keydown', e => { if(e.key==='Enter') doLogin(); });
</script>`
}

func registerHTML(lang string) string {
	return `
<div class="auth-wrap" style="align-items:flex-start;padding-top:2rem;">
<div class="wizard-wrap">

  <!-- Barre de progression -->
  <div class="wizard-progress">
    <div class="wp-step active" id="wp1">
      <div class="wp-dot">1</div><div class="wp-label">` + i18n.T(lang, "register.step1_label") + `</div>
    </div>
    <div class="wp-line"></div>
    <div class="wp-step" id="wp2">
      <div class="wp-dot">2</div><div class="wp-label">` + i18n.T(lang, "register.step2_label") + `</div>
    </div>
    <div class="wp-line"></div>
    <div class="wp-step" id="wp3">
      <div class="wp-dot">3</div><div class="wp-label">` + i18n.T(lang, "register.step3_label") + `</div>
    </div>
  </div>

  <!-- ÉTAPE 1 : Infos compte -->
  <div class="wizard-step" id="step1">
    <div style="text-align:center;margin-bottom:24px;">
      <img src="/static/scorbits_logo.png" alt="Scorbits" style="height:72px;width:auto;">
    </div>
    <div class="auth-title">` + i18n.T(lang, "register.title") + `</div>
    <div class="auth-sub">` + i18n.T(lang, "register.step1_sub") + `</div>
    <div class="reg-grid">
      <div class="fgroup"><label>` + i18n.T(lang, "register.email") + `</label><input type="email" id="r-email" class="finput" placeholder="` + i18n.T(lang, "register.placeholder_email") + `" autocomplete="email" oninput="checkEmail(this.value)"><div id="email-status" class="field-status"></div></div>
      <div class="fgroup">
        <label>` + i18n.T(lang, "register.pseudo") + ` <span class="hint">` + i18n.T(lang, "register.pseudo_hint") + `</span></label>
        <input type="text" id="r-pseudo" class="finput" placeholder="` + i18n.T(lang, "register.placeholder_pseudo") + `" oninput="checkPseudo(this.value)" autocomplete="username">
        <div id="pseudo-status" class="field-status"></div>
      </div>
      <div class="fgroup"><label>` + i18n.T(lang, "register.password") + `</label><input type="password" id="r-pass" class="finput" placeholder="` + i18n.T(lang, "register.placeholder_pass") + `" autocomplete="new-password"></div>
      <div class="fgroup"><label>` + i18n.T(lang, "register.confirm_password") + `</label><input type="password" id="r-pass2" class="finput" placeholder="` + i18n.T(lang, "register.placeholder_pass2") + `" autocomplete="new-password"></div>
    </div>
    <div id="s1-error" class="ferror hidden"></div>
    <button class="btn-main" onclick="goStep2()">` + i18n.T(lang, "register.continue") + `</button>
    <div class="auth-switch">` + i18n.T(lang, "register.already_account") + ` <a href="/wallet">` + i18n.T(lang, "register.login") + `</a></div>
  </div>

  <!-- ÉTAPE 2 : Génération wallet -->
  <div class="wizard-step hidden" id="step2">
    <div class="auth-title">` + i18n.T(lang, "register.wallet_title") + `</div>
    <div class="auth-sub">` + i18n.T(lang, "register.step2_sub") + `</div>
    <div class="wallet-explain">
      <div class="we-row">
        <div class="we-icon">
          <svg width="32" height="32" viewBox="0 0 32 32"><rect x="4" y="8" width="24" height="18" rx="3" fill="none" stroke="#00e85a" stroke-width="1.5"/><rect x="20" y="14" width="8" height="6" rx="1.5" fill="#00e85a" opacity="0.7"/><line x1="4" y1="13" x2="28" y2="13" stroke="#00e85a" stroke-width="1.5"/></svg>
        </div>
        <div class="we-text">
          <div class="we-title">` + i18n.T(lang, "register.sco_addr_title") + `</div>
          <div class="we-desc">` + i18n.T(lang, "register.sco_addr_desc") + `</div>
        </div>
      </div>
      <div class="we-row">
        <div class="we-icon">
          <svg width="32" height="32" viewBox="0 0 32 32"><rect x="8" y="12" width="16" height="14" rx="2" fill="none" stroke="#ffd93d" stroke-width="1.5"/><path d="M11 12V9a5 5 0 0 1 10 0v3" stroke="#ffd93d" stroke-width="1.5" fill="none" stroke-linecap="round"/><circle cx="16" cy="18" r="2" fill="#ffd93d"/><line x1="16" y1="20" x2="16" y2="23" stroke="#ffd93d" stroke-width="1.5" stroke-linecap="round"/></svg>
        </div>
        <div class="we-text">
          <div class="we-title" style="color:#ffd93d">` + i18n.T(lang, "register.privkey_title") + `</div>
          <div class="we-desc">` + i18n.T(lang, "register.privkey_desc") + `</div>
        </div>
      </div>
    </div>
    <div id="wallet-gen-status" class="wallet-gen">
      <div class="wg-spinner"></div>
      <div class="wg-text">` + i18n.T(lang, "register.generating") + `</div>
    </div>
    <div id="wallet-ready" class="hidden">
      <div class="wallet-addr-box">
        <div class="wab-label">` + i18n.T(lang, "register.address_label") + `</div>
        <div class="wab-addr mono green" id="gen-address"></div>
      </div>
      <div style="display:flex;gap:1rem;margin-top:1.5rem;">
        <button class="btn-back" onclick="goStep(1)">` + i18n.T(lang, "register.back") + `</button>
        <button class="btn-main" onclick="goStep3()" style="flex:2">` + i18n.T(lang, "register.continue") + `</button>
      </div>
    </div>
  </div>

  <!-- ÉTAPE 3 : Sécurité et téléchargement -->
  <div class="wizard-step hidden" id="step3">
    <div class="auth-title">` + i18n.T(lang, "register.step3_label") + `</div>
    <div class="auth-sub">` + i18n.T(lang, "register.step3_sub") + `</div>

    <!-- Triangle danger custom -->
    <div class="danger-banner">
      <svg class="danger-triangle" width="52" height="46" viewBox="0 0 52 46" fill="none" xmlns="http://www.w3.org/2000/svg">
        <path d="M26 3 L49 43 H3 Z" fill="#2a1500" stroke="#ff8800" stroke-width="2.5" stroke-linejoin="round"/>
        <line x1="26" y1="16" x2="26" y2="30" stroke="#ff8800" stroke-width="3" stroke-linecap="round"/>
        <circle cx="26" cy="36.5" r="2.2" fill="#ff8800"/>
      </svg>
      <div class="danger-text">
        <div class="danger-title">` + i18n.T(lang, "register.danger_title") + `</div>
        <div class="danger-desc">` + i18n.T(lang, "register.danger_desc") + `</div>
      </div>
    </div>

    <div class="security-checklist">
      <div class="sc-item">
        <svg width="18" height="18" viewBox="0 0 18 18"><path d="M3 9l4 4 8-8" stroke="#00e85a" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>
        ` + i18n.T(lang, "register.check1") + `
      </div>
      <div class="sc-item">
        <svg width="18" height="18" viewBox="0 0 18 18"><path d="M3 9l4 4 8-8" stroke="#00e85a" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>
        ` + i18n.T(lang, "register.check2") + `
      </div>
      <div class="sc-item">
        <svg width="18" height="18" viewBox="0 0 18 18"><path d="M3 9l4 4 8-8" stroke="#00e85a" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>
        ` + i18n.T(lang, "register.check3") + `
      </div>
    </div>

    <div class="keys-display" id="keys-display" style="display:none">
      <div class="keys-note">` + i18n.T(lang, "register.wallet_note") + `</div>
      <div class="key-row"><div class="key-label">` + i18n.T(lang, "register.address_field") + `</div><div class="key-val mono" id="kd-address"></div><button class="key-copy-btn" onclick="copyKey('kd-address',this)">` + i18n.T(lang, "register.copy_btn") + `</button></div>
      <div class="key-row"><div class="key-label">` + i18n.T(lang, "register.pubkey_field") + `</div><div class="key-val mono" id="kd-pubkey"></div><button class="key-copy-btn" onclick="copyKey('kd-pubkey',this)">` + i18n.T(lang, "register.copy_btn") + `</button></div>
      <div class="key-row key-row-priv"><div class="key-label" style="color:#ff8800">` + i18n.T(lang, "register.privkey_field") + `</div><div class="key-val mono" id="kd-privkey"></div><button class="key-copy-btn key-copy-btn-warn" onclick="copyKey('kd-privkey',this)">` + i18n.T(lang, "register.copy_btn") + `</button></div>
    </div>
    <button class="btn-download" id="btn-download" onclick="downloadWallet()">
      <svg width="18" height="18" viewBox="0 0 18 18"><path d="M9 3v8m0 0l-3-3m3 3l3-3" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/><rect x="2" y="13" width="14" height="3" rx="1.5" fill="currentColor" opacity="0.4"/></svg>
      ` + i18n.T(lang, "register.download_btn") + `
    </button>
    <div id="download-confirm" class="hidden" style="color:var(--green);font-size:0.88rem;margin:0.5rem 0;text-align:center;">
      <svg width="14" height="14" viewBox="0 0 14 14"><path d="M2 7l3 3 7-7" stroke="#00e85a" stroke-width="2" fill="none" stroke-linecap="round"/></svg>
      ` + i18n.T(lang, "register.file_downloaded") + `
    </div>

    <div class="confirm-check" id="confirm-check-wrap">
      <label class="check-label">
        <input type="checkbox" id="r-confirm" disabled onchange="updateStep3Btn()">
        <span class="checkmark"></span>
        ` + i18n.T(lang, "register.confirm_check") + `
      </label>
    </div>
    <div class="blocked-hint" id="blocked-hint">
      ` + i18n.T(lang, "register.blocked_hint") + `
    </div>

    <div id="s3-error" class="ferror hidden"></div>
    <button class="btn-main" id="btn-create" onclick="doRegister()" disabled style="opacity:0.4;cursor:not-allowed;margin-top:1rem">
      ` + i18n.T(lang, "register.create_btn") + `
    </button>
    <div class="auth-switch"><a href="/wallet/register">` + i18n.T(lang, "register.restart") + `</a></div>
  </div>

</div>
</div>

<style>
.wizard-wrap{width:100%;max-width:600px;margin:0 auto;}
.wizard-progress{display:flex;align-items:center;margin-bottom:2rem;}
.wp-step{display:flex;flex-direction:column;align-items:center;gap:0.3rem;flex:1;}
.wp-dot{width:32px;height:32px;border-radius:50%;background:var(--bg3);border:2px solid var(--green3);color:var(--muted);font-weight:700;font-size:0.9rem;display:flex;align-items:center;justify-content:center;transition:all .3s;}
.wp-step.active .wp-dot{background:var(--green4);border-color:var(--green);color:var(--green);}
.wp-step.done .wp-dot{background:var(--green);border-color:var(--green);color:#000;}
.wp-label{font-size:0.72rem;color:var(--muted);font-weight:600;}
.wp-step.active .wp-label{color:var(--green2);}
.wp-line{flex:1;height:2px;background:var(--green4);margin-bottom:1rem;}
.wizard-step{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:2.5rem;box-shadow:0 0 60px rgba(0,232,90,0.08);}
.reg-grid{display:grid;grid-template-columns:1fr 1fr;gap:1rem;}
@media(max-width:600px){.reg-grid{grid-template-columns:1fr;}}
.hint{font-size:0.72rem;color:var(--muted);font-weight:400;margin-left:0.3rem;}
.field-status{font-size:0.78rem;margin-top:0.3rem;min-height:1.2em;}
.field-status.ok{color:var(--green);}
.field-status.err{color:#ff6464;}
.btn-back{background:transparent;border:1px solid var(--border);color:var(--text2);padding:12px 20px;border-radius:7px;cursor:pointer;font-family:'Inter',sans-serif;font-size:0.95rem;transition:all .2s;flex:1;}
.btn-back:hover{border-color:var(--green3);color:var(--text);}
.wallet-explain{display:flex;flex-direction:column;gap:1rem;margin-bottom:1.5rem;}
.we-row{display:flex;gap:1rem;align-items:flex-start;background:var(--bg3);border-radius:8px;padding:1rem;}
.we-icon{flex-shrink:0;}
.we-title{font-weight:700;font-size:0.95rem;color:var(--green);margin-bottom:0.3rem;}
.we-desc{font-size:0.85rem;color:var(--text2);line-height:1.6;}
.wallet-gen{display:flex;align-items:center;gap:1rem;background:var(--bg3);border-radius:8px;padding:1.2rem;}
.wg-spinner{width:24px;height:24px;border:3px solid var(--green4);border-top-color:var(--green);border-radius:50%;animation:spin .8s linear infinite;flex-shrink:0;}
@keyframes spin{to{transform:rotate(360deg);}}
.wg-text{color:var(--text2);font-size:0.9rem;}
.wallet-addr-box{background:var(--bg3);border:1px solid var(--green3);border-radius:8px;padding:1.2rem;}
.wab-label{font-size:0.75rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.5rem;}
.wab-addr{font-size:0.85rem;word-break:break-all;line-height:1.6;}
.danger-banner{display:flex;gap:1rem;align-items:flex-start;background:rgba(255,136,0,0.07);border:1px solid rgba(255,136,0,0.3);border-radius:10px;padding:1.2rem 1.4rem;margin-bottom:1.5rem;}
.danger-title{font-weight:700;font-size:1rem;color:#ff8800;margin-bottom:0.4rem;}
.danger-desc{font-size:0.88rem;color:var(--text2);line-height:1.6;}
.security-checklist{display:flex;flex-direction:column;gap:0.6rem;margin-bottom:1.5rem;}
.sc-item{display:flex;align-items:center;gap:0.6rem;font-size:0.9rem;color:var(--text2);}
.btn-download{width:100%;background:var(--bg3);border:2px solid var(--green3);color:var(--green);padding:14px;border-radius:8px;font-size:1rem;font-weight:600;cursor:pointer;transition:all .3s;font-family:'Inter',sans-serif;display:flex;align-items:center;justify-content:center;gap:0.6rem;margin-bottom:0.8rem;}
.btn-download:hover{background:var(--green4);border-color:var(--green2);box-shadow:0 0 20px rgba(0,232,90,0.2);}
.btn-download.downloaded{border-color:var(--green);background:var(--green4);}
.confirm-check{margin:1rem 0 0.5rem;}
.check-label{display:flex;gap:0.8rem;align-items:flex-start;cursor:pointer;font-size:0.88rem;color:var(--text2);line-height:1.6;}
.check-label input{margin-top:3px;accent-color:var(--green);width:16px;height:16px;}
.blocked-hint{font-size:0.8rem;color:var(--muted);text-align:center;margin-bottom:0.5rem;}
.auth-logo-wrap{display:flex;justify-content:center;margin-bottom:1rem;}
.keys-display{background:var(--bg3);border:1px solid var(--border);border-radius:8px;padding:1rem;margin-bottom:1rem;display:flex;flex-direction:column;gap:0.6rem;}
.keys-note{font-size:0.8rem;color:var(--muted);margin-bottom:0.4rem;line-height:1.5;}
.key-row{display:flex;align-items:center;gap:0.5rem;flex-wrap:wrap;}
.key-row-priv{background:rgba(255,136,0,0.06);border-radius:6px;padding:0.4rem;}
.key-label{font-size:0.72rem;font-weight:700;color:var(--muted);text-transform:uppercase;letter-spacing:0.5px;min-width:80px;flex-shrink:0;}
.key-val{font-size:0.72rem;color:var(--text2);word-break:break-all;flex:1;line-height:1.5;}
.key-copy-btn{flex-shrink:0;background:var(--bg2);border:1px solid var(--border);color:var(--text2);padding:3px 10px;border-radius:5px;cursor:pointer;font-size:0.75rem;font-family:'Inter',sans-serif;transition:all .2s;}
.key-copy-btn:hover{border-color:var(--green3);color:var(--green);}
.key-copy-btn-warn{border-color:rgba(255,136,0,0.4);color:#ff8800;}
.key-copy-btn-warn:hover{border-color:#ff8800;background:rgba(255,136,0,0.1);}
</style>
<script>
let wizardData = { email:'', pseudo:'', password:'', address:'', pubHex:'', privHex:'' };
let walletDownloaded = false;

function goStep(n) {
  document.querySelectorAll('.wizard-step').forEach(s => s.classList.add('hidden'));
  document.getElementById('step'+n).classList.remove('hidden');
  document.querySelectorAll('.wp-step').forEach((s,i) => {
    s.classList.remove('active','done');
    if(i+1 < n) s.classList.add('done');
    if(i+1 === n) s.classList.add('active');
  });
  window.scrollTo({top:0,behavior:'smooth'});
}

function goStep2() {
  const email = document.getElementById('r-email').value.trim();
  const pseudo = document.getElementById('r-pseudo').value.trim();
  const pass = document.getElementById('r-pass').value;
  const pass2 = document.getElementById('r-pass2').value;
  if (!email||!pseudo||!pass||!pass2) { showErr('s1-error',I18N['register.fill_fields']); return; }
  if (pass !== pass2) { showErr('s1-error',I18N['register.passwords_mismatch']); return; }
  if (pass.length < 8) { showErr('s1-error',I18N['register.password_too_short']); return; }
  if (pseudo.length < 3) { showErr('s1-error',I18N['register.pseudo_too_short']); return; }
  const es = document.getElementById('email-status');
  if (es.classList.contains('err')) { showErr('s1-error',I18N['register.email_taken_err']); return; }
  wizardData.email = email; wizardData.pseudo = pseudo; wizardData.password = pass;
  document.getElementById('s1-error').classList.add('hidden');
  goStep(2);
  generateWallet();
}
let emailTimer;
async function checkEmail(val) {
  clearTimeout(emailTimer);
  const el = document.getElementById('email-status');
  if (!val || !val.includes('@')) { el.textContent=''; return; }
  emailTimer = setTimeout(async () => {
    const r = await fetch('/api/auth/check-email?email=' + encodeURIComponent(val));
    const d = await r.json();
    if (d.available) { el.textContent=I18N['register.email_available']; el.className='field-status ok'; }
    else { el.textContent=I18N['register.email_taken_status']; el.className='field-status err'; }
  }, 400);
}

function goStep3() {
  goStep(3);
  document.getElementById('kd-address').textContent = wizardData.address;
  document.getElementById('kd-pubkey').textContent = wizardData.pubHex;
  document.getElementById('kd-privkey').textContent = wizardData.privHex;
  document.getElementById('keys-display').style.display = 'flex';
}
function copyKey(id, btn) {
  const val = document.getElementById(id).textContent;
  navigator.clipboard.writeText(val).then(() => {
    const orig = btn.textContent; btn.textContent = I18N['register.copied'];
    setTimeout(() => { btn.textContent = orig; }, 1500);
  });
}

async function generateWallet() {
  const kp = await crypto.subtle.generateKey({name:'ECDSA',namedCurve:'P-256'}, true, ['sign','verify']);
  const pub = await crypto.subtle.exportKey('raw', kp.publicKey);
  const priv = await crypto.subtle.exportKey('pkcs8', kp.privateKey);
  wizardData.pubHex = bufToHex(pub);
  wizardData.privHex = bufToHex(priv);
  wizardData.address = 'SCO' + (await sha256hex(wizardData.pubHex)).substring(0,32);
  document.getElementById('wallet-gen-status').classList.add('hidden');
  document.getElementById('gen-address').textContent = wizardData.address;
  document.getElementById('wallet-ready').classList.remove('hidden');
}

function downloadWallet() {
  const walletData = JSON.stringify({
    "// ATTENTION": I18N['register.warning_text'],
    "pseudo": wizardData.pseudo,
    "address": wizardData.address,
    "public_key": wizardData.pubHex,
    "private_key": wizardData.privHex,
    "created_at": new Date().toISOString(),
    "network": I18N['register.network']
  }, null, 2);
  const a = document.createElement('a');
  a.href = 'data:application/json,' + encodeURIComponent(walletData);
  a.download = I18N['register.wallet_file_prefix'] + wizardData.pseudo + '.json';
  a.click();
  walletDownloaded = true;
  document.getElementById('download-confirm').classList.remove('hidden');
  document.getElementById('btn-download').classList.add('downloaded');
  document.getElementById('r-confirm').disabled = false;
  document.getElementById('blocked-hint').style.display = 'none';
  updateStep3Btn();
}

function updateStep3Btn() {
  const confirmed = document.getElementById('r-confirm').checked;
  const btn = document.getElementById('btn-create');
  if (walletDownloaded && confirmed) {
    btn.disabled = false;
    btn.style.opacity = '1';
    btn.style.cursor = 'pointer';
  } else {
    btn.disabled = true;
    btn.style.opacity = '0.4';
    btn.style.cursor = 'not-allowed';
  }
}

async function doRegister() {
  if (!walletDownloaded || !document.getElementById('r-confirm').checked) return;
  const bodyColors = ['#00e85a','#4f9fff','#ff6b6b','#ffd93d','#c77dff','#ff9f43','#ffffff','#00d4ff','#ff4dc4','#39ff14','#ff8800','#a8ff3e','#ff3e3e','#b8b8b8','#7fff00','#00ffcc','#ff6600','#cc00ff','#ffcc00','#00ccff','#ff0066','#66ff00','#0066ff','#ff6666','#aaffaa'];
  const bgColors = ['#1a2a1e','#0d1b2a','#1a0a2e','#2a0a0a','#0a2a1a','#2a2a2a','#001a2a','#0a0a0a','#1a1a2a','#002a2a','#2a1a00','#1a001a','#2a2a00','#1a2a2a','#2a1a2a','#0a1a0a','#1a0a0a','#0a0a1a','#0a1a1a','#1a1a0a','#000a00','#00000a','#0a0000','#050505','#101010'];
  const eyes = ['normal','gros','en_amande','fermes','etoile'];
  const mouths = ['neutre','sourire','triste','fou','mechant'];
  const accessories = ['aucun','casquette','chapeau','couronne','bandana'];
  const rnd = arr => arr[Math.floor(Math.random()*arr.length)];
  const avatar = { shape:'circle', body_color:rnd(bodyColors), bg_color:rnd(bgColors), expression:'neutre', eyes:rnd(eyes), mouth:rnd(mouths), accessory:rnd(accessories) };
  const res = await fetch('/api/auth/register', {
    method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({
      email: wizardData.email,
      password: wizardData.password,
      pseudo: wizardData.pseudo,
      address: wizardData.address,
      pub_key: wizardData.pubHex,
      avatar: avatar
    })
  });
  const data = await res.json();
  if (data.success) {
    document.querySelector('.wizard-wrap').innerHTML =
      '<div class="wizard-step" style="text-align:center;padding:3rem 2rem">' +
      '<svg width="60" height="60" viewBox="0 0 60 60" style="margin-bottom:1.5rem"><circle cx="30" cy="30" r="28" fill="none" stroke="#00e85a" stroke-width="2"/><path d="M18 30l8 8 16-16" stroke="#00e85a" stroke-width="3" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>' +
      '<div style="font-size:1.5rem;font-weight:700;margin-bottom:0.8rem;">'+I18N['register.account_created']+'</div>' +
      '<p style="color:var(--text2);line-height:1.7;margin-bottom:1rem;">' + data.message + '</p>' +
      '<a href="/wallet" style="display:inline-block;margin-top:1.5rem;color:var(--green2);font-weight:600;">'+I18N['register.back_to_login']+'</a></div>';
  } else { showErr('s3-error', data.error || I18N['register.error']); }
}

function showErr(id, msg) { const el = document.getElementById(id); el.textContent = msg; el.classList.remove('hidden'); }
async function sha256hex(str) { const buf = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(str)); return bufToHex(buf); }
function bufToHex(buf) { return Array.from(new Uint8Array(buf)).map(b=>b.toString(16).padStart(2,'0')).join(''); }
let pseudoTimer;
async function checkPseudo(val) {
  clearTimeout(pseudoTimer);
  const el = document.getElementById('pseudo-status');
  if (!val) { el.textContent=''; return; }
  if (val.length < 3) { el.textContent=I18N['register.min_3_chars']; el.className='field-status err'; return; }
  pseudoTimer = setTimeout(()=>{ el.textContent='@'+val+I18N['register.pseudo_suffix']; el.className='field-status ok'; }, 500);
}
</script>`
}

func verifySuccessHTML(pseudo, lang string) string {
	return `<div class="auth-wrap"><div class="auth-box" style="text-align:center;">
    <svg width="64" height="64" viewBox="0 0 64 64" style="margin-bottom:1rem"><circle cx="32" cy="32" r="30" fill="none" stroke="#00e85a" stroke-width="2"/><path d="M20 32l8 8 16-16" stroke="#00e85a" stroke-width="3" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>
    <div class="auth-title">` + i18n.T(lang, "verify.success_title") + `</div>
    <p style="color:var(--text2);margin:1rem 0 1.5rem;line-height:1.7;">` + i18n.T(lang, "verify.success_msg") + pseudo + i18n.T(lang, "verify.success_msg2") + `</p>
    <a href="/wallet/dashboard" class="btn-main" style="display:block;text-decoration:none;text-align:center;">` + i18n.T(lang, "verify.go_wallet") + `</a>
  </div></div>`
}

func verifyErrorHTML(msg, lang string) string {
	return `<div class="auth-wrap"><div class="auth-box" style="text-align:center;">
    <svg width="64" height="64" viewBox="0 0 64 64" style="margin-bottom:1rem"><circle cx="32" cy="32" r="30" fill="none" stroke="#ff6464" stroke-width="2"/><line x1="20" y1="20" x2="44" y2="44" stroke="#ff6464" stroke-width="3" stroke-linecap="round"/><line x1="44" y1="20" x2="20" y2="44" stroke="#ff6464" stroke-width="3" stroke-linecap="round"/></svg>
    <div class="auth-title">` + i18n.T(lang, "verify.error_title") + `</div>
    <p style="color:var(--text2);margin:1rem 0 1.5rem;line-height:1.7;">` + msg + `</p>
    <a href="/wallet/register" class="btn-main" style="display:block;text-decoration:none;text-align:center;">` + i18n.T(lang, "verify.create_account") + `</a>
  </div></div>`
}

func activeClass(nav, active string) string {
	if nav == active {
		return " active"
	}
	return ""
}

func logoSVG(size int) string {
	return logoSCO(size)
}

func logoSCO(size int) string {
	return fmt.Sprintf(`<svg width="%d" height="%d" viewBox="0 0 60 60" xmlns="http://www.w3.org/2000/svg">
  <defs>
    <linearGradient id="sco-grad" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0%%" stop-color="#00e85a"/>
      <stop offset="100%%" stop-color="#ffe600"/>
    </linearGradient>
  </defs>
  <circle cx="30" cy="30" r="28" fill="#040d05" stroke="url(#sco-grad)" stroke-width="2"/>
  <path d="M22 20 Q22 13 30 13 Q38 13 38 20 Q38 27 30 27 Q22 27 22 34 Q22 41 30 41 Q38 41 38 34"
        stroke="url(#sco-grad)" stroke-width="3.2" fill="none" stroke-linecap="round"/>
  <line x1="19" y1="20" x2="41" y2="20" stroke="url(#sco-grad)" stroke-width="2.2" stroke-linecap="round"/>
  <line x1="19" y1="34" x2="41" y2="34" stroke="url(#sco-grad)" stroke-width="2.2" stroke-linecap="round"/>
  <circle cx="30" cy="8" r="2.2" fill="#ffe600"/>
  <circle cx="30" cy="46" r="2.2" fill="#00e85a"/>
</svg>`, size, size)
}

func computeBadgesHTML(blocksMined, daysSince int) string {
	out := ""
	if blocksMined >= 1 {
		out += `<span class="badge-item">Premier bloc</span>`
	}
	if blocksMined >= 10 {
		out += `<span class="badge-item">Mineur actif</span>`
	}
	if blocksMined >= 100 {
		out += `<span class="badge-item gold">Centurion</span>`
	}
	if blocksMined >= 1000 {
		out += `<span class="badge-item diamond">Maître mineur</span>`
	}
	if daysSince >= 30 {
		out += `<span class="badge-item blue">Vétéran 30j</span>`
	}
	if daysSince >= 365 {
		out += `<span class="badge-item purple">Vétéran 1 an</span>`
	}
	return out
}

func page(title, content, active, lang string, user *db.User) string {
	year := fmt.Sprintf("%d", time.Now().Year())
	// Escape </script> sequences to prevent premature tag close in inline scripts
	i18nJSON := strings.ReplaceAll(i18n.TranslationsJSON(lang), "</script>", `<\/script>`)
	langEnActive := ""
	langFrActive := ""
	if lang == "en" {
		langEnActive = " active"
	} else {
		langFrActive = " active"
	}
	navUserHTML := fmt.Sprintf(`<div class="nav-auth-buttons"><a href="/wallet/register" class="btn-register">%s</a><a href="/wallet" class="btn-login">%s</a></div>`,
		i18n.T(lang, "nav.create_account"), i18n.T(lang, "nav.login"))
	isLoggedIn := "false"
	isAdmin := "false"
	userPseudo := ""
	userIDHex := ""
	if user != nil {
		pic34 := renderProfilePic(user, 34)
		pic48 := renderProfilePic(user, 48)
		addrShortNav := user.Address
		if len(addrShortNav) > 20 {
			addrShortNav = addrShortNav[:10] + "..." + addrShortNav[len(addrShortNav)-6:]
		}
		navBadge := ""
		if user.Pseudo == "Yousse" {
			navBadge = `<span class="scorbits-badge" title="Scorbits Official"><span>S</span></span>`
		}
		navUserHTML = fmt.Sprintf(`<div class="nav-user-corner" id="nav-user-corner" onclick="toggleNavMenu()">
  <div class="nav-user-btn">%s<span class="nav-pseudo">@%s</span>%s<span class="nav-chevron">▾</span></div>
  <div class="nav-user-menu hidden" id="nav-user-menu">
    <div class="nav-menu-header">%s<div><div class="nav-menu-pseudo">@%s</div><div class="nav-menu-addr">%s</div></div></div>
    <a href="/" class="nav-menu-item" onclick="document.getElementById('nav-user-menu').classList.add('hidden')">🏠 Home</a>
    <div class="nav-menu-divider"></div>
    <a href="/profile#profile" class="nav-menu-item" onclick="navigateToProfileTab('profile')">%s</a>
    <a href="/profile#wallet" class="nav-menu-item" onclick="navigateToProfileTab('wallet')">%s</a>
    <a href="/mine" class="nav-menu-item">%s</a>
    <a href="/pool" class="nav-menu-item">Mining Pool</a>
    <div class="nav-menu-divider"></div>
    <a href="/wallet/logout" class="nav-menu-item nav-menu-logout">%s</a>
  </div>
</div>`,
			pic34, user.Pseudo, navBadge,
			pic48, user.Pseudo, addrShortNav,
			i18n.T(lang, "nav.my_profile"),
			i18n.T(lang, "nav.my_wallet"),
			i18n.T(lang, "nav.mine_sco"),
			i18n.T(lang, "wallet.logout"))
		isLoggedIn = "true"
		userPseudo = user.Pseudo
		userIDHex = user.ID.Hex()
		if adminUsernames[user.Pseudo] {
			isAdmin = "true"
		}
	}
	publishFAB := ""
	if user != nil {
		publishFAB = `<div id="publish-fab" class="publish-fab" onclick="openPublishModal()" title="` + i18n.T(lang, "common.new_post") + `">
  <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4L16.5 3.5z"/></svg>
</div>
<div class="fab-group">
  <div class="notif-wrapper">
    <button class="notif-btn" onclick="toggleNotifPanel()" id="notif-btn" title="Notifications">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg>
      <span class="notif-badge" id="notif-badge">0</span>
    </button>
    <div id="notif-panel" class="notif-panel hidden">
      <div class="notif-header">
        <span>🔔 Notifications</span>
        <button onclick="markAllNotifsRead()" id="notif-mark-all-btn" style="display:none;font-size:0.75rem;color:var(--green2)">Tout lire</button>
        <button onclick="toggleNotifPanel()">✕</button>
      </div>
      <div id="notif-list" class="notif-list"></div>
    </div>
  </div>
  <button class="dm-open-btn" onclick="toggleDMWindow()" id="dm-open-btn">
    💬 ` + i18n.T(lang, "dm.messages") + `
    <span class="dm-badge" id="dm-badge">0</span>
  </button>
</div>
<div id="notif-toast-container"></div>
<div id="dm-window" class="dm-window hidden">
  <div id="dm-view-convs" class="dm-view">
    <div class="dm-header">
      <span>💬 ` + i18n.T(lang, "dm.messages") + `</span>
      <button onclick="openCreateGroup()" class="dm-new-group-btn" title="` + i18n.T(lang, "dm.new_group") + `">👥+</button>
      <button onclick="openDMSettings()" title="` + i18n.T(lang, "dm.settings") + `">⚙️</button>
      <button onclick="toggleDMWindow()">✕</button>
    </div>
    <input type="text" id="dm-search" class="dm-search" placeholder="` + i18n.T(lang, "dm.search_placeholder") + `" oninput="searchDMUser(this.value)" autocomplete="off">
    <div id="dm-search-results" class="dm-list hidden"></div>
    <div id="dm-conv-list" class="dm-list"></div>
  </div>
  <div id="dm-view-conv" class="dm-view hidden">
    <div class="dm-header">
      <button onclick="closeDMConversation()">←</button>
      <span id="dm-conv-header-avatar" style="display:inline-flex;width:28px;height:28px;border-radius:50%;overflow:hidden;flex-shrink:0;background:var(--green4)"></span>
      <span id="dm-conv-header-pseudo"></span>
      <button onclick="toggleDMWindow()">✕</button>
    </div>
    <div id="dm-messages" class="dm-messages"></div>
    <div class="dm-input-area">
      <div class="dm-toolbar">
        <button class="chat-emoji-btn" onclick="toggleDMEmoji(event)" title="Emoji">😊</button>
        <button class="chat-gif-btn" onclick="toggleDMGif(event)" title="GIF">GIF</button>
        <input type="text" id="dm-input" class="chat-input" placeholder="` + i18n.T(lang, "dm.input_placeholder") + `" maxlength="1000" autocomplete="off">
        <button class="chat-send-btn" onclick="sendDM()">➤</button>
      </div>
      <div class="emoji-picker hidden" id="dm-emoji-picker"></div>
      <div class="gif-picker hidden" id="dm-gif-picker" onclick="event.stopPropagation()">
        <input type="text" id="dm-gif-search" class="gif-search-input" placeholder="` + i18n.T(lang, "chat.search_gif") + `" oninput="searchDMGifs(this.value)">
        <div class="gif-grid" id="dm-gif-grid"></div>
      </div>
    </div>
  </div>
  <div id="dm-view-settings" class="dm-view hidden">
    <div class="dm-header">
      <button onclick="closeDMSettings()">←</button>
      <span>` + i18n.T(lang, "dm.settings") + `</span>
      <button onclick="toggleDMWindow()">✕</button>
    </div>
    <div class="dm-settings-panel">
      <p style="color:#4a6a4a;font-size:12px">` + i18n.T(lang, "dm.settings_desc") + `</p>
      <label class="dm-setting-option"><input type="radio" name="dm-policy" value="everyone"><div><span>` + i18n.T(lang, "dm.policy_everyone") + `</span><small>` + i18n.T(lang, "dm.policy_everyone_desc") + `</small></div></label>
      <label class="dm-setting-option"><input type="radio" name="dm-policy" value="initiated"><div><span>` + i18n.T(lang, "dm.policy_initiated") + `</span><small>` + i18n.T(lang, "dm.policy_initiated_desc") + `</small></div></label>
      <label class="dm-setting-option"><input type="radio" name="dm-policy" value="nobody"><div><span>` + i18n.T(lang, "dm.policy_nobody") + `</span><small>` + i18n.T(lang, "dm.policy_nobody_desc") + `</small></div></label>
      <button onclick="saveDMSettings()" class="dm-save-btn">` + i18n.T(lang, "dm.save_settings") + `</button>
    </div>
  </div>
  <div id="dm-view-create-group" class="dm-view hidden">
    <div class="dm-header">
      <button onclick="showDMView('convs')">←</button>
      <span>` + i18n.T(lang, "dm.create_group_title") + `</span>
      <button onclick="toggleDMWindow()">✕</button>
    </div>
    <div class="dm-create-group">
      <input type="text" id="group-name-input" placeholder="` + i18n.T(lang, "dm.group_name_placeholder") + `" class="dm-search" maxlength="50" autocomplete="off">
      <p style="color:#4a6a4a;font-size:12px;margin:4px 0 2px">` + i18n.T(lang, "dm.group_search_members") + `</p>
      <input type="text" id="group-member-search" placeholder="` + i18n.T(lang, "dm.search_placeholder") + `" oninput="searchGroupMember(this.value)" class="dm-search" autocomplete="off">
      <div id="group-member-results"></div>
      <div id="group-selected-members" style="display:flex;flex-wrap:wrap;gap:6px;margin:4px 0"></div>
      <button onclick="createGroup()" class="dm-save-btn">` + i18n.T(lang, "dm.create_group_btn") + `</button>
    </div>
  </div>
  <div id="dm-view-group" class="dm-view hidden">
    <div class="dm-header">
      <button onclick="closeGroupConversation()">←</button>
      <span id="dm-group-header-avatar" style="display:inline-flex;width:28px;height:28px;border-radius:50%;overflow:hidden;flex-shrink:0;background:#0d2010;align-items:center;justify-content:center;color:#00e85a;font-weight:700;font-size:0.7rem"></span>
      <div style="flex:1;min-width:0">
        <div id="dm-group-header-name" style="font-weight:700;font-size:0.85rem;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;color:#d4f5e0"></div>
        <div id="dm-group-member-count" onclick="openGroupInfo()" class="dm-group-members-count"></div>
      </div>
      <button id="dm-group-settings-btn" onclick="openGroupInfo()" class="dm-group-settings-btn">⚙️</button>
      <button onclick="toggleDMWindow()">✕</button>
    </div>
    <div id="dm-group-messages" class="dm-messages"></div>
    <div class="dm-input-area">
      <div class="dm-toolbar">
        <input type="text" id="dm-group-input" class="chat-input" placeholder="` + i18n.T(lang, "dm.input_placeholder") + `" maxlength="1000" autocomplete="off">
        <button class="chat-send-btn" onclick="sendGroupMessage()">➤</button>
      </div>
    </div>
  </div>
  <div id="dm-view-group-info" class="dm-view hidden">
    <div class="dm-header">
      <button onclick="showDMView('group')">←</button>
      <span>` + i18n.T(lang, "dm.group_info_title") + `</span>
      <button onclick="toggleDMWindow()">✕</button>
    </div>
    <div class="dm-group-info-panel">
      <div id="dm-group-rename-section">
        <input type="text" id="dm-group-rename-input" class="dm-search" placeholder="` + i18n.T(lang, "dm.group_name_placeholder") + `" maxlength="50" style="margin-bottom:4px">
        <button id="dm-group-rename-btn" onclick="renameGroup()" class="dm-save-btn" style="margin-bottom:8px">` + i18n.T(lang, "dm.group_save_name") + `</button>
      </div>
      <p id="dm-group-members-title" style="color:#4a6a4a;font-size:11px;margin-bottom:6px">` + i18n.T(lang, "dm.group_members_title") + `</p>
      <div id="dm-group-members-list"></div>
      <div id="dm-group-add-section" style="margin-top:8px">
        <p style="color:#4a6a4a;font-size:11px;margin-bottom:4px">` + i18n.T(lang, "dm.group_add_member") + `</p>
        <input type="text" id="dm-group-add-search" placeholder="` + i18n.T(lang, "dm.search_placeholder") + `" oninput="searchGroupAddMember(this.value)" class="dm-search-add-input" autocomplete="off">
        <div id="dm-group-add-results" class="dm-search-add-results"></div>
      </div>
      <button onclick="leaveGroup()" class="dm-danger-btn" style="margin-top:12px">` + i18n.T(lang, "dm.group_leave") + `</button>
    </div>
  </div>
</div>
<div id="dm-toast-container" class="dm-toast-container"></div>
<div id="publish-modal-overlay" class="publish-modal-overlay hidden" onclick="if(event.target===this)closePublishModal()">
  <div class="publish-modal">
    <div class="publish-modal-title">` + i18n.T(lang, "common.new_post") + `</div>
    <div style="position:relative;">
      <textarea id="publish-content" class="publish-textarea" placeholder="` + i18n.T(lang, "common.what_new") + `" maxlength="500" oninput="updatePublishCount()"></textarea>
      <div class="chat-mentions-list hidden" id="publish-mentions" style="bottom:auto;top:100%;z-index:100"></div>
    </div>
    <div class="publish-charcount"><span id="publish-count">0</span>/500</div>
    <div id="publish-img-preview" class="publish-img-preview hidden"></div>
    <div class="publish-toolbar">
      <label class="publish-tool-btn" title="` + i18n.T(lang, "post.add_image") + `">
        <input type="file" id="publish-img-input" accept="image/*" style="display:none" onchange="onPublishImgChange(this)">
        &#128247;
      </label>
      <button class="publish-tool-btn" title="` + i18n.T(lang, "post.add_emoji") + `" onclick="togglePublishEmoji()">&#128512;</button>
    </div>
    <div id="publish-emoji-picker" class="publish-emoji-picker hidden"></div>
    <div class="publish-actions">
      <button class="publish-cancel-btn" onclick="closePublishModal()">` + i18n.T(lang, "common.cancel") + `</button>
      <button class="publish-submit-btn" onclick="submitPost()">` + i18n.T(lang, "common.publish") + `</button>
    </div>
  </div>
</div>`
	}

	return `<!DOCTYPE html>
<html lang="` + lang + `">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>` + title + ` — Scorbits</title>
<link rel="icon" type="image/x-icon" href="/static/favicon.ico">
<link rel="icon" type="image/png" sizes="32x32" href="/static/favicon.ico">
<link rel="icon" type="image/png" sizes="16x16" href="/static/favicon-16.png">
<link rel="apple-touch-icon" sizes="192x192" href="/static/favicon-192.png">
<meta property="og:image" content="https://scorbits.com/static/scorbits_logo.png">
<meta property="og:title" content="Scorbits (SCO) — Blockchain Explorer">
<meta property="og:description" content="Explore the Scorbits blockchain. Mine SCO, send transactions, and track the network in real time.">
<meta name="twitter:card" content="summary">
<meta name="twitter:image" content="https://scorbits.com/static/scorbits_logo.png">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>
:root{
  --bg:#040d05;--bg2:#071009;--bg3:#0c180d;
  --green:#00e85a;--green2:#00b847;--green3:#005f24;--green4:#002d10;
  --muted:#2a6038;--text:#d4f5e0;--text2:#7ab98a;--border:#0c2614;
  --orange:#ff9f43;
}
*{box-sizing:border-box;margin:0;padding:0;}
body{background:var(--bg);color:var(--text);font-family:'Inter',sans-serif;font-size:16px;line-height:1.6;min-height:100vh;}
canvas#rain{position:fixed;top:0;left:0;width:100%;height:100%;z-index:0;pointer-events:none;}
header,main,footer{position:relative;z-index:1;}

/* HEADER */
header{background:rgba(4,13,5,0.97);border-bottom:1px solid var(--green3);padding:0 2rem;display:flex;align-items:center;justify-content:space-between;height:64px;position:sticky;top:0;z-index:200;backdrop-filter:blur(12px);}
.logo{display:flex;align-items:center;gap:0.6rem;text-decoration:none;position:absolute;left:50%;transform:translateX(-50%);z-index:2;}
.logo-text{font-size:1.2rem;font-weight:800;color:var(--green);}
.logo-sub{color:var(--text2);font-size:0.85rem;font-weight:400;}
nav{display:flex;gap:0.3rem;align-items:center;}
.nav-link{color:var(--text2);text-decoration:none;font-size:0.88rem;font-weight:500;padding:6px 14px;border:1px solid transparent;border-radius:6px;transition:all .2s;}
.nav-link:hover,.nav-link.active{color:var(--green);border-color:var(--green3);background:var(--green4);}
.nav-user-corner{position:relative;cursor:pointer;user-select:none;flex-shrink:0;}
.nav-user-btn{display:flex;align-items:center;gap:8px;padding:6px 12px;border-radius:10px;border:1px solid var(--green3);background:var(--green4);transition:all .2s;}
.nav-user-btn:hover{background:#0a2510;border-color:var(--green2);}
.nav-pseudo{font-size:0.85rem;font-weight:600;color:var(--green);}
.nav-chevron{font-size:0.7rem;color:var(--muted);transition:transform .2s;line-height:1;}
.nav-user-corner.open .nav-chevron{transform:rotate(180deg);}
.nav-user-menu{position:absolute;top:calc(100%+8px);left:0;min-width:220px;background:#0a1a0a;border:1px solid var(--green3);border-radius:12px;box-shadow:0 8px 24px rgba(0,0,0,0.6);z-index:1000;overflow:hidden;}
.nav-user-menu.hidden{display:none;}
.nav-menu-header{display:flex;align-items:center;gap:10px;padding:14px;border-bottom:1px solid var(--green3);background:#071209;}
.nav-menu-pseudo{font-size:0.9rem;font-weight:700;color:var(--green);}
.nav-menu-addr{font-size:0.68rem;color:var(--muted);font-family:'JetBrains Mono',monospace;margin-top:2px;word-break:break-all;}
.nav-menu-item{display:block;padding:10px 16px;color:var(--text2);text-decoration:none;font-size:0.85rem;transition:all .15s;}
.nav-menu-item:hover{background:var(--green4);color:var(--green);}
.nav-menu-divider{height:1px;background:var(--green3);margin:4px 0;}
.nav-menu-logout{color:#ff6060;}
.nav-menu-logout:hover{background:rgba(255,60,60,0.08);color:#ff4444;}
.nav-auth-buttons{display:flex;align-items:center;gap:8px;flex-shrink:0;}
.btn-register{padding:7px 16px;border-radius:8px;border:1px solid var(--green3);color:var(--text2);background:transparent;text-decoration:none;font-size:0.85rem;font-weight:500;transition:all .2s;}
.btn-register:hover{border-color:var(--green2);color:var(--green);}
.btn-login{padding:7px 16px;border-radius:8px;border:none;background:var(--green);color:#000;text-decoration:none;font-size:0.85rem;font-weight:700;transition:all .2s;}
.btn-login:hover{background:var(--green2);}
.btn-sm{padding:5px 12px;font-size:0.8rem;border:1px solid var(--green3);background:var(--green4);color:var(--green);border-radius:6px;cursor:pointer;transition:all .2s;text-decoration:none;display:inline-block;}
.btn-sm:hover{background:#0a2510;border-color:var(--green2);}
.btn-danger{border-color:rgba(255,60,60,0.3);background:rgba(255,60,60,0.05);color:#ff6060;}
.btn-danger:hover{background:rgba(255,60,60,0.1);border-color:#ff4444;}
.header-right{display:flex;gap:1rem;align-items:center;}
.search-form{display:flex;}
.search-form input{background:rgba(4,13,5,0.9);border:1px solid var(--green3);border-right:none;color:var(--text);padding:8px 14px;border-radius:6px 0 0 6px;width:200px;font-family:'Inter',sans-serif;font-size:0.85rem;outline:none;transition:all .2s;}
.search-form input:focus{border-color:var(--green2);}
.search-form input::placeholder{color:var(--muted);}
.search-form button{background:var(--green3);border:1px solid var(--green3);color:var(--green);padding:8px 14px;border-radius:0 6px 6px 0;cursor:pointer;font-size:0.85rem;font-weight:600;transition:all .2s;font-family:'Inter',sans-serif;}
.search-form button:hover{background:var(--green2);color:#000;}

/* HEADER — MOBILE */
@media(max-width:768px){
  header{padding:0 0.75rem;height:auto;min-height:52px;flex-wrap:wrap;row-gap:0;}
  .logo{position:static;transform:none;order:1;flex:1;min-width:0;padding:8px 0;}
  .logo img{height:32px!important;max-height:40px!important;}
  .logo-text{font-size:1rem;}
  .nav-user-corner{order:2;flex-shrink:0;padding:8px 0;}
  .nav-auth-buttons{order:2;flex-shrink:0;padding:8px 0;}
  .header-right{order:3;width:100%;padding:0.3rem 0 0.4rem;gap:0;}
  .header-right>div{width:100%;flex-wrap:nowrap;gap:0.3rem;}
  .lang-selector{display:none!important;}
  .search-form{width:100%;flex:1;display:flex;}
  .search-form input{width:auto!important;flex:1;min-width:0;}
  .nav-pseudo{display:none!important;}
}
@media(max-width:768px){
  .ticker-wrap{top:98px;}
}

/* HALVING BAR */
.ticker-wrap{background:linear-gradient(90deg,#b34700 0%,#e67e00 15%,#f5a623 35%,#f5d050 55%,#ffe566 75%,#C8A01E 100%);border-bottom:1px solid rgba(0,0,0,0.2);height:32px;position:sticky;top:64px;z-index:199;display:flex;align-items:center;justify-content:center;padding:0 1.5rem;}
.halving-bar-container{display:flex;align-items:center;height:100%;width:100%;max-width:760px;}
.halving-bar-left{display:flex;flex-direction:column;align-items:flex-start;flex-shrink:0;line-height:1.1;margin-right:16px;}
.halving-bar-track{flex:1;height:16px;background:linear-gradient(90deg,#ffe566 0%,#f5d050 40%,#C8A01E 100%);border-radius:8px;position:relative;overflow:hidden;border:1px solid rgba(0,0,0,0.15);box-shadow:inset 0 2px 4px rgba(0,0,0,0.2);}
#halving-bar-pct{position:absolute;top:50%;left:50%;transform:translate(-50%,-50%);font-family:Arial,sans-serif;font-size:13px;font-weight:700;color:#000;z-index:2;pointer-events:none;white-space:nowrap;text-shadow:0 1px 2px rgba(255,255,255,0.5);}
.halving-bar-fill{height:100%;border-radius:8px;background:repeating-linear-gradient(45deg,#00ff66 0px,#00ff66 12px,#00cc44 12px,#00cc44 24px);background-size:34px 34px;animation:hatchMove 0.8s linear infinite;box-shadow:2px 0 10px rgba(0,255,100,0.5);transition:width 1.5s ease;}
@keyframes hatchMove{0%{background-position:0 0}100%{background-position:34px 0}}

/* MAIN */
main{max-width:1400px;margin:0 auto;padding:1.5rem;}

/* HOME 2 COLONNES */
.home-layout{display:grid;grid-template-columns:1fr 1.2fr;gap:1.2rem;align-items:start;}
@media(max-width:900px){.home-layout{grid-template-columns:1fr;}}
.col-left,.col-mid{min-width:0;}

.home-col{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.1rem;}
.panel-title{font-size:0.72rem;font-weight:700;color:var(--muted);text-transform:uppercase;letter-spacing:1.5px;margin-bottom:0.8rem;display:flex;align-items:center;gap:0.4rem;}
.panel-title::before{content:'';display:inline-block;width:3px;height:0.85em;background:var(--green);border-radius:2px;}
.panel-sub{font-size:0.68rem;color:var(--muted);font-weight:400;letter-spacing:0;margin-left:auto;}

/* STAT MINI */
.stat-mini-grid{display:grid;grid-template-columns:1fr 1fr;gap:0.5rem;margin-bottom:0.8rem;}
.stat-mini{background:var(--bg3);border:1px solid var(--border);border-radius:7px;padding:0.6rem 0.8rem;}
.sm-label{font-size:0.67rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.15rem;}
.sm-val{font-size:1.15rem;font-weight:800;color:var(--green);}

/* ACTIVITY FEED */
.activity-feed{max-height:400px;overflow-y:auto;margin-bottom:0.4rem;scrollbar-width:thin;scrollbar-color:#1e1e1e transparent;}
.activity-feed::-webkit-scrollbar{width:3px;}.activity-feed::-webkit-scrollbar-track{background:transparent;}.activity-feed::-webkit-scrollbar-thumb{background:var(--green3);border-radius:2px;}
.af-loading{color:var(--muted);font-size:0.78rem;padding:0.4rem 0;}
.af-item{display:flex;align-items:center;gap:0.5rem;padding:0.35rem 0;border-bottom:1px solid var(--border);font-size:0.78rem;color:var(--text2);}
.af-item:last-child{border-bottom:none;}
.af-dot{width:6px;height:6px;background:var(--green);border-radius:50%;flex-shrink:0;box-shadow:0 0 4px var(--green);}
.af-text{flex:1;min-width:0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;}
.af-time{color:var(--muted);font-size:0.66rem;margin-left:auto;flex-shrink:0;white-space:nowrap;}

/* MINI TABLE */
.mini-table-wrap{overflow-x:auto;}
.mini-table{width:100%;border-collapse:collapse;font-size:0.78rem;}
.mini-table th{padding:5px 7px;text-align:left;color:var(--muted);font-size:0.67rem;font-weight:600;text-transform:uppercase;border-bottom:1px solid var(--green4);}
.mini-table td{padding:6px 7px;border-bottom:1px solid var(--border);color:var(--text);}
.mini-table tr:last-child td{border-bottom:none;}
.mini-table tr:hover td{background:rgba(0,232,90,0.03);}

/* LEADERBOARD */
.leaderboard-widget{font-size:0.8rem;}
.lb-item{display:flex;align-items:center;gap:0.6rem;padding:0.45rem 0;border-bottom:1px solid var(--border);}
.lb-item:last-child{border-bottom:none;}
.lb-rank{width:20px;text-align:center;font-weight:700;color:var(--muted);font-size:0.75rem;flex-shrink:0;}
.lb-rank.r1{color:#ffd700;}.lb-rank.r2{color:#c0c0c0;}.lb-rank.r3{color:#cd7f32;}
.lb-name{flex:1;color:var(--text);font-weight:600;cursor:pointer;font-size:0.8rem;}
.lb-name:hover{color:var(--green);}
.lb-blocks{color:var(--green);font-weight:700;font-size:0.75rem;}

/* PROFILE MODAL */
.profile-modal-overlay{position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,0.78);z-index:500;display:flex;align-items:center;justify-content:center;}
.profile-modal-box{background:var(--bg2);border:1px solid var(--green3);border-radius:14px;padding:2rem;max-width:360px;width:90%;position:relative;box-shadow:0 0 60px rgba(0,0,0,0.8);}
.pmo-close{position:absolute;top:0.9rem;right:0.9rem;background:none;border:none;color:var(--text2);cursor:pointer;padding:4px;border-radius:4px;display:flex;align-items:center;}
.pmo-close:hover{color:var(--text);}
.pmo-avatar{display:flex;justify-content:center;margin-bottom:0.8rem;}
.pmo-pseudo{text-align:center;font-size:1.2rem;font-weight:700;color:var(--green);margin-bottom:0.2rem;}
.pmo-since{text-align:center;font-size:0.75rem;color:var(--muted);margin-bottom:1rem;}
.pmo-stats{display:flex;gap:1.2rem;justify-content:center;margin-bottom:1rem;}
.pmo-stat{text-align:center;}
.pmo-stat-val{font-size:1.1rem;font-weight:700;color:var(--green);}
.pmo-stat-label{font-size:0.68rem;color:var(--muted);text-transform:uppercase;letter-spacing:0.8px;}
.pmo-badges{display:flex;flex-wrap:wrap;gap:0.35rem;justify-content:center;margin-bottom:1.2rem;}
.pmo-btn{width:100%;background:var(--green4);border:1px solid var(--green3);color:var(--green2);padding:10px;border-radius:7px;font-size:0.88rem;font-weight:600;cursor:pointer;font-family:'Inter',sans-serif;transition:all .2s;display:flex;align-items:center;justify-content:center;gap:0.5rem;}
.pmo-btn:hover{background:var(--green3);color:var(--green);}

/* BADGES */
.badge-item{background:var(--green4);border:1px solid var(--green3);color:var(--green2);padding:3px 9px;border-radius:11px;font-size:0.7rem;font-weight:600;}
.badge-item.gold{background:rgba(255,215,0,0.1);border-color:rgba(255,215,0,0.3);color:#ffd700;}
.badge-item.diamond{background:rgba(0,212,255,0.1);border-color:rgba(0,212,255,0.3);color:#00d4ff;}
.badge-item.blue{background:rgba(79,159,255,0.1);border-color:rgba(79,159,255,0.3);color:#4f9fff;}
.badge-item.purple{background:rgba(199,125,255,0.1);border-color:rgba(199,125,255,0.3);color:#c77dff;}
.pub-badges{display:flex;flex-wrap:wrap;gap:0.35rem;margin-top:0.7rem;}
.pub-profile{max-width:580px;margin:0 auto;}
.pub-profile-header{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.8rem;display:flex;gap:1.5rem;align-items:flex-start;margin-bottom:1.5rem;}
.pub-pseudo{font-size:1.3rem;font-weight:700;color:var(--green);}
.pub-since{margin-bottom:0.5rem;}
.pub-stats{display:flex;gap:1.5rem;margin-top:0.8rem;}
.pub-stat{text-align:center;}
.ps-val{font-size:1.1rem;font-weight:700;}
.ps-label{font-size:0.68rem;color:var(--muted);text-transform:uppercase;letter-spacing:0.8px;}

/* GENERAL */
.stitle{font-size:1.05rem;font-weight:700;color:var(--text);margin:1.6rem 0 0.8rem;padding-bottom:0.4rem;border-bottom:2px solid var(--green3);display:flex;align-items:center;gap:0.5rem;}
.stitle::before{content:'';display:inline-block;width:3px;height:0.95em;background:var(--green);border-radius:2px;box-shadow:0 0 5px var(--green);}
.tbox{border:1px solid var(--green3);border-radius:10px;overflow:hidden;}
/* BLOCK LIST */
.block-list{max-height:400px;overflow-y:auto;border:1px solid #1a2a1a;border-radius:12px;background:#071020;}
.block-list::-webkit-scrollbar{width:3px;}.block-list::-webkit-scrollbar-track{background:transparent;}.block-list::-webkit-scrollbar-thumb{background:#00e85a;border-radius:2px;}
.block-row{display:flex;align-items:center;gap:0.8rem;padding:10px 14px;border-bottom:1px solid rgba(26,42,26,0.8);transition:background .15s;cursor:pointer;animation:bfadein .4s ease;}
.block-row:last-child{border-bottom:none;}
.block-row:hover{background:rgba(0,232,90,0.04);}
@keyframes bfadein{from{opacity:0;transform:translateY(-8px)}to{opacity:1;transform:translateY(0)}}
.block-num{font-size:0.82rem;font-weight:700;color:#00e85a;flex-shrink:0;min-width:48px;}
.block-hash{font-family:'JetBrains Mono',monospace;font-size:0.68rem;color:#4a6a4a;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.block-miner{font-size:0.75rem;color:var(--text2);flex-shrink:0;}
.block-miner a{color:var(--text2);text-decoration:none;}.block-miner a:hover{color:#00e85a;}
.block-ts{font-size:0.68rem;color:#4a6a4a;flex-shrink:0;}
.block-reward{font-size:0.78rem;font-weight:700;color:#00e85a;flex-shrink:0;white-space:nowrap;}
table{width:100%;border-collapse:collapse;font-size:0.92rem;}
thead tr{background:var(--bg3);}
th{padding:10px 15px;text-align:left;color:var(--muted);font-size:0.7rem;font-weight:600;text-transform:uppercase;letter-spacing:0.8px;border-bottom:1px solid var(--green4);}
td{padding:11px 15px;border-bottom:1px solid var(--border);color:var(--text);vertical-align:middle;}
tr{cursor:pointer;transition:background .15s;}tr:last-child td{border-bottom:none;}tr:hover td{background:rgba(0,232,90,0.025);}
.badge{display:inline-block;background:var(--green4);border:1px solid var(--green3);color:var(--green);padding:2px 9px;border-radius:5px;font-size:0.8rem;font-weight:600;}
.mono{font-family:'JetBrains Mono',monospace;}
.sm{font-size:0.8rem!important;}
.muted{color:var(--text2)!important;}
.green{color:var(--green)!important;}
.orange{color:var(--orange)!important;}
.fw6{font-weight:600;}
.wrap{word-break:break-all;line-height:1.6;}
.hidden{display:none!important;}
.dbox{border:1px solid var(--green3);border-radius:10px;overflow:hidden;background:var(--bg2);margin-bottom:1rem;}
.drow{display:flex;padding:12px 18px;border-bottom:1px solid var(--border);gap:1.5rem;align-items:flex-start;transition:background .15s;}
.drow:last-child{border-bottom:none;}.drow:hover{background:rgba(0,232,90,0.02);}
.dlabel{min-width:145px;color:var(--text2);font-size:0.78rem;font-weight:500;flex-shrink:0;padding-top:2px;}
.bnav{display:flex;gap:0.8rem;margin-bottom:1rem;}
.navbtn{background:var(--bg3);border:1px solid var(--green3);color:var(--text2);padding:7px 15px;border-radius:6px;font-size:0.85rem;text-decoration:none;transition:all .2s;}
.navbtn:hover{border-color:var(--green);color:var(--green);}
.conf-tip{background:rgba(255,159,67,0.06);border:1px solid rgba(255,159,67,0.25);border-radius:7px;padding:0.65rem 1rem;font-size:0.82rem;color:var(--orange);margin-bottom:1rem;}
.big-reward{font-size:1.2rem;font-weight:700;color:var(--green);}
.bigbal{font-size:1.7rem;font-weight:800;color:var(--green);}
a{color:var(--green2);text-decoration:none;transition:color .15s;}a:hover{color:var(--green);}
.profile-link{color:var(--green2);font-weight:600;}
.modal-overlay{position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,0.76);z-index:400;display:flex;align-items:center;justify-content:center;}

/* AUTH */
.auth-wrap{min-height:calc(100vh - 140px);display:flex;align-items:center;justify-content:center;padding:2rem;}
.auth-box{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;box-shadow:0 0 50px rgba(0,232,90,0.07);padding:2.2rem;width:100%;max-width:440px;}
.auth-box.wide{max-width:680px;}
.auth-title{text-align:center;font-size:1.45rem;font-weight:700;margin-bottom:0.3rem;}
.auth-sub{text-align:center;color:var(--text2);font-size:0.87rem;margin-bottom:1.6rem;}
.auth-switch{text-align:center;margin-top:1.1rem;color:var(--text2);font-size:0.83rem;}
.auth-logo-svg{display:flex;justify-content:center;margin-bottom:0.6rem;}
.fgroup{margin-bottom:0.95rem;}
.fgroup label{display:block;font-size:0.78rem;font-weight:600;color:var(--text2);margin-bottom:0.4rem;}
.finput{width:100%;background:var(--bg3);border:1px solid var(--green3);color:var(--text);padding:9px 13px;border-radius:6px;font-family:'Inter',sans-serif;font-size:0.9rem;outline:none;transition:all .2s;}
.finput:focus{border-color:var(--green2);box-shadow:0 0 8px rgba(0,232,90,0.07);}
.finput::placeholder{color:var(--muted);}
.btn-main{width:100%;background:var(--green);color:#000;border:none;padding:11px;border-radius:7px;font-size:0.92rem;font-weight:700;cursor:pointer;transition:all .2s;font-family:'Inter',sans-serif;}
.btn-main:hover{background:var(--green2);box-shadow:0 0 16px rgba(0,232,90,0.22);}
.btn-main:disabled{opacity:0.4;cursor:not-allowed;}
.ferror{color:#ff6464;font-size:0.82rem;margin:0.5rem 0;padding:8px 12px;background:rgba(255,100,100,0.07);border:1px solid rgba(255,100,100,0.2);border-radius:6px;}

/* WALLET DASHBOARD */
.wdash{display:grid;grid-template-columns:215px 1fr;gap:1.2rem;}
@media(max-width:768px){.wdash{grid-template-columns:1fr;}}
.wside{background:var(--bg2);border:1px solid var(--green3);border-radius:10px;padding:1.1rem;height:fit-content;position:sticky;top:108px;}
.wside-user{text-align:center;padding-bottom:0.9rem;border-bottom:1px solid var(--border);margin-bottom:0.9rem;}
.wside-avatar{display:flex;justify-content:center;margin-bottom:0.5rem;}
.wside-avatar svg{border-radius:50%;border:2px solid var(--green3);}
.wside-pseudo{font-weight:700;font-size:0.92rem;color:var(--green);}
.wside-addr{margin-top:0.2rem;}
.wside-nav{display:flex;flex-direction:column;gap:0.2rem;}
.wnav-item{color:var(--text2);text-decoration:none;padding:7px 11px;border-radius:6px;font-size:0.85rem;font-weight:500;transition:all .2s;border:1px solid transparent;}
.wnav-item:hover{background:var(--bg3);color:var(--text);border-color:var(--border);}
.wnav-item.active{background:var(--green4);color:var(--green);border-color:var(--green3);}
.wnav-item.logout{color:#ff6464;margin-top:0.4rem;}
.wnav-item.logout:hover{background:rgba(255,100,100,0.07);border-color:rgba(255,100,100,0.2);}
.wmain{min-width:0;}
.wdash-balance{background:linear-gradient(135deg,var(--bg2),var(--bg3));border:1px solid var(--green3);border-radius:12px;padding:1.6rem;text-align:center;margin-bottom:1.1rem;position:relative;}
.wdb-label{font-size:0.7rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:1.5px;margin-bottom:0.5rem;}
.wdb-amount{font-size:2.4rem;font-weight:800;color:var(--green);text-shadow:0 0 22px rgba(0,232,90,0.28);}
.wdb-eur{font-size:0.9rem;color:var(--text2);margin-top:0.3rem;}
.balance-notif{position:absolute;top:0.7rem;right:0.7rem;background:var(--green4);border:1px solid var(--green3);color:var(--green);padding:4px 11px;border-radius:18px;font-size:0.75rem;font-weight:600;display:flex;align-items:center;gap:0.35rem;animation:fadeIn .3s ease;}
@keyframes fadeIn{from{opacity:0;transform:translateY(-4px)}to{opacity:1;transform:translateY(0)}}
.wdash-actions{display:grid;grid-template-columns:repeat(3,1fr);gap:0.7rem;margin-bottom:1.1rem;}
.wact{background:var(--bg2);border:1px solid var(--green3);border-radius:9px;padding:0.9rem;text-align:center;text-decoration:none;color:var(--text);transition:all .2s;display:flex;flex-direction:column;align-items:center;gap:0.35rem;}
.wact:hover{border-color:var(--green);background:var(--bg3);color:var(--green);}
.wact.primary{background:var(--green4);border-color:var(--green2);color:var(--green);}
.wact-label{font-size:0.8rem;font-weight:600;}
.chart-wrap{background:var(--bg2);border:1px solid var(--green3);border-radius:10px;padding:0.9rem;margin-bottom:0.9rem;}
.txlist{background:var(--bg2);border:1px solid var(--green3);border-radius:10px;overflow:hidden;}
.txlist-load{padding:1.4rem;text-align:center;color:var(--muted);}
.tx-row{display:flex;align-items:center;gap:0.7rem;padding:0.8rem 0.9rem;border-bottom:1px solid var(--border);transition:background .15s;cursor:pointer;}
.tx-row:last-child{border-bottom:none;}.tx-row:hover{background:rgba(0,232,90,0.025);}
.tx-icon{width:34px;height:34px;border-radius:50%;display:flex;align-items:center;justify-content:center;flex-shrink:0;}
.tx-icon.mine{background:rgba(0,232,90,0.1);color:var(--green);}
.tx-icon.send{background:rgba(255,100,100,0.1);color:#ff6464;}
.tx-icon.recv{background:rgba(0,232,90,0.1);color:var(--green);}
.tx-info{flex:1;}.tx-label{font-weight:600;font-size:0.85rem;}.tx-meta{font-size:0.72rem;color:var(--muted);margin-top:1px;}
.tx-right{text-align:right;}.tx-amt{font-weight:700;font-size:0.87rem;}.tx-amt.pos{color:var(--green);}.tx-amt.neg{color:#ff6464;}
.tx-date{font-size:0.7rem;color:var(--muted);margin-top:1px;}
.send-card{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.6rem;max-width:490px;}
.send-bal{background:var(--bg3);border-radius:7px;padding:0.65rem 0.9rem;margin-bottom:1.1rem;font-size:0.85rem;color:var(--text2);}
.fee-box{background:var(--bg3);border-left:3px solid var(--green3);padding:0.65rem 0.9rem;border-radius:0 6px 6px 0;font-size:0.82rem;color:var(--text2);margin-bottom:0.9rem;}
.modal-box{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.6rem;max-width:360px;width:90%;box-shadow:0 0 50px rgba(0,0,0,0.8);}
.modal-title{font-size:1.05rem;font-weight:700;margin-bottom:1.1rem;}
.modal-body{margin-bottom:1.3rem;}
.cb-row{display:flex;justify-content:space-between;padding:0.45rem 0;font-size:0.85rem;color:var(--text2);border-bottom:1px solid var(--border);}
.cb-row:last-child{border-bottom:none;}.cb-row.total{font-weight:700;color:var(--text)!important;font-size:0.92rem;}
.modal-actions{display:flex;gap:0.7rem;}
.btn-cancel{flex:1;background:transparent;border:1px solid var(--border);color:var(--text2);padding:9px;border-radius:6px;cursor:pointer;font-family:'Inter',sans-serif;font-size:0.88rem;transition:all .2s;}
.btn-cancel:hover{border-color:var(--green3);color:var(--text);}
.btn-confirm{flex:2;background:var(--green);color:#000;border:none;padding:9px;border-radius:6px;cursor:pointer;font-family:'Inter',sans-serif;font-size:0.88rem;font-weight:700;transition:all .2s;}
.btn-confirm:hover{background:var(--green2);}
.success-card{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:2.2rem 1.4rem;text-align:center;}
.success-icon{display:flex;justify-content:center;margin-bottom:0.9rem;}
.success-title{font-size:1.2rem;font-weight:700;margin-bottom:0.45rem;}
.success-txid{font-family:'JetBrains Mono',monospace;font-size:0.75rem;color:var(--muted);margin-bottom:0.6rem;word-break:break-all;}
.success-info{color:var(--text2);font-size:0.85rem;}
.recv-card{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.8rem;max-width:400px;text-align:center;}
.qr-wrap{background:#fff;border-radius:9px;padding:11px;display:inline-block;margin-bottom:1.1rem;}
.recv-label{font-size:0.7rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.5rem;}
.recv-addr{font-size:0.78rem;word-break:break-all;background:var(--bg3);border:1px solid var(--green3);border-radius:6px;padding:0.9rem;margin-bottom:1.1rem;line-height:1.6;}
.recv-info{color:var(--text2);font-size:0.82rem;margin-top:0.9rem;line-height:1.7;}
.copy-confirm{display:flex;align-items:center;justify-content:center;gap:0.4rem;color:var(--green);font-size:0.82rem;font-weight:600;margin-top:0.6rem;}

/* MINE */
.mine-page{max-width:880px;margin:0 auto;}
.mine-hero{text-align:center;padding:2rem 1rem 1.5rem;}
.mine-hero-icon{display:flex;justify-content:center;margin-bottom:0.8rem;}
.mine-hero-title{font-size:1.8rem;font-weight:800;margin-bottom:0.4rem;}
.mine-hero-sub{color:var(--text2);font-size:0.95rem;margin-bottom:1rem;line-height:1.7;}
.mine-reward-badge{display:inline-block;background:var(--green4);border:1px solid var(--green3);color:var(--green);padding:7px 18px;border-radius:18px;font-size:0.9rem;}
.mine-tabs{display:grid;grid-template-columns:repeat(3,1fr);gap:0.8rem;margin-bottom:1.5rem;}
.mine-tab{background:var(--bg2);border:1px solid var(--green3);border-radius:9px;padding:1rem;cursor:pointer;transition:all .2s;display:flex;flex-direction:column;align-items:center;gap:0.25rem;font-family:'Inter',sans-serif;}
.mine-tab:hover{border-color:var(--green2);background:var(--bg3);}
.mine-tab.active{background:var(--green4);border-color:var(--green2);}
.mine-tab-label{font-size:0.9rem;font-weight:700;color:var(--text);}
.mine-tab-sub{font-size:0.75rem;color:var(--text2);}
.mine-panel{display:none;}.mine-panel.active{display:block;}
.mine-card{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.8rem;}
.mine-card-title{font-size:1.1rem;font-weight:700;margin-bottom:0.4rem;}
.mine-card-desc{color:var(--text2);font-size:0.9rem;line-height:1.7;margin-bottom:1.5rem;}
.mine-login-prompt{background:var(--bg3);border:1px solid var(--border);border-radius:8px;padding:1.2rem;margin-bottom:1.2rem;}
.mlp-text{color:var(--text2);font-size:0.88rem;margin-bottom:0.9rem;line-height:1.6;}
.mlp-actions{display:flex;align-items:center;gap:0.9rem;flex-wrap:wrap;}
.mlp-or{color:var(--muted);font-size:0.82rem;}
.mine-wallet-info{background:var(--bg3);border:1px solid var(--green3);border-radius:7px;padding:0.9rem 1.1rem;margin-bottom:1.2rem;}
.mwi-label{font-size:0.7rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.35rem;}
.mwi-addr{font-size:0.82rem;word-break:break-all;line-height:1.6;}
.mine-controls{display:flex;gap:0.9rem;margin-bottom:1.2rem;}
.btn-mine-start{background:var(--green);color:#000;border:none;padding:12px 28px;border-radius:7px;font-size:0.95rem;font-weight:700;cursor:pointer;transition:all .2s;font-family:'Inter',sans-serif;}
.btn-mine-start:hover{background:var(--green2);}
.btn-mine-start.mining{background:var(--green3);color:var(--green);animation:pulse 2s infinite;}
@keyframes pulse{0%,100%{box-shadow:0 0 0 0 rgba(0,232,90,0.3)}50%{box-shadow:0 0 0 7px rgba(0,232,90,0)}}
.btn-mine-stop{background:transparent;border:1px solid #ff6464;color:#ff6464;padding:12px 20px;border-radius:7px;font-size:0.9rem;font-weight:600;cursor:pointer;transition:all .2s;font-family:'Inter',sans-serif;}
.btn-mine-stop:hover{background:rgba(255,100,100,0.07);}
.mine-status{background:var(--bg3);border:1px solid var(--border);border-radius:9px;padding:1.3rem;margin-bottom:1.3rem;}
.mine-stat-row{display:grid;grid-template-columns:repeat(4,1fr);gap:0.8rem;margin-bottom:1rem;}
@media(max-width:580px){.mine-stat-row{grid-template-columns:repeat(2,1fr);}}
.mine-stat{text-align:center;}
.mine-stat-label{font-size:0.68rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.25rem;}
.mine-stat-val{font-size:1.3rem;font-weight:700;color:var(--text);}
.mine-stat-val.green{color:var(--green);}
.mine-progress{margin-bottom:0.9rem;}
.mine-progress-label{font-size:0.78rem;color:var(--text2);margin-bottom:0.4rem;}
.mine-progress-bar{background:var(--bg2);border-radius:3px;height:5px;overflow:hidden;}
.mine-progress-fill{background:var(--green);height:100%;width:0%;transition:width .3s;box-shadow:0 0 6px rgba(0,232,90,0.4);}
.mine-log{max-height:110px;overflow-y:auto;font-family:'JetBrains Mono',monospace;font-size:0.72rem;color:var(--muted);line-height:1.8;}
.mine-log .log-success{color:var(--green);}
.dl-grid{display:grid;grid-template-columns:repeat(4,1fr);gap:0.8rem;margin-bottom:1.5rem;}
@media(max-width:650px){.dl-grid{grid-template-columns:repeat(2,1fr);}}
.dl-card{background:var(--bg3);border:1px solid var(--border);border-radius:9px;padding:1rem;text-align:center;transition:border-color .2s;}
.dl-card:hover{border-color:var(--green3);}
.dl-os{font-weight:700;font-size:0.9rem;color:var(--text);}
.dl-arch{font-size:0.72rem;color:var(--muted);margin-bottom:0.7rem;}
.btn-dl{display:block;background:var(--green3);border:1px solid var(--green2);color:var(--green);padding:7px 10px;border-radius:6px;font-size:0.78rem;font-weight:600;text-decoration:none;transition:all .2s;}
.btn-dl:hover{background:var(--green2);color:#000;}
.guide-steps{display:flex;flex-direction:column;gap:1rem;}
.guide-title{font-size:0.95rem;font-weight:700;margin-bottom:0.7rem;}
.step{display:flex;gap:0.9rem;align-items:flex-start;}
.step-num{width:30px;height:30px;background:var(--green4);border:1px solid var(--green3);border-radius:50%;display:flex;align-items:center;justify-content:center;font-weight:700;color:var(--green);font-size:0.85rem;flex-shrink:0;}
.step-title{font-weight:600;font-size:0.9rem;margin-bottom:0.25rem;}
.step-desc{font-size:0.84rem;color:var(--text2);line-height:1.6;}
.code-block{background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:0.7rem 0.9rem;margin-top:0.5rem;font-family:'JetBrains Mono',monospace;font-size:0.78rem;color:var(--green2);line-height:1.8;overflow-x:auto;}
.code-os{font-size:0.68rem;color:var(--muted);margin-bottom:0.3rem;font-family:'Inter',sans-serif;}
.pro-section{margin-bottom:1.3rem;}
.pro-section-title{font-size:0.9rem;font-weight:700;margin-bottom:0.5rem;}

footer{text-align:center;padding:1.8rem;color:var(--muted);font-size:0.8rem;border-top:1px solid var(--border);margin-top:3rem;position:relative;z-index:1;}
.footer-wp{margin-top:0.6rem;display:flex;justify-content:center;gap:1rem;}
.footer-wp-link{color:var(--green2);text-decoration:none;font-size:0.75rem;border:1px solid var(--green3);padding:3px 10px;border-radius:12px;transition:color .2s,border-color .2s;}
.footer-wp-link:hover{color:var(--green);border-color:var(--green);}
.wp-banner{text-align:center;padding:0.4rem 1rem 0.8rem;margin-bottom:0.5rem;}
.wp-btn{color:var(--green2);text-decoration:none;font-size:0.82rem;border:1px solid var(--green3);background:var(--bg2);padding:5px 16px;border-radius:14px;transition:color .2s,border-color .2s,background .2s;display:inline-block;}
.wp-btn:hover{color:var(--green);border-color:var(--green);background:var(--bg3);}
.mine-wp-link{margin:0.4rem 0 0.7rem;}

/* BLINK DOT */
.blink-dot{display:inline-block;width:7px;height:7px;background:var(--green);border-radius:50%;margin-left:0.3rem;vertical-align:middle;animation:blink-pulse 1.4s ease-in-out infinite;box-shadow:0 0 5px var(--green);}
@keyframes blink-pulse{0%,100%{opacity:1;transform:scale(1)}50%{opacity:0.3;transform:scale(0.7)}}

/* NET STATS GRID */
.ns-grid{display:grid;grid-template-columns:1fr 1fr;gap:0.55rem;margin-bottom:0.8rem;}
.ns-card{background:#071020;border:1px solid #1a2a1a;border-radius:12px;padding:16px;transition:border-color .2s,box-shadow .2s;cursor:default;}
.ns-card:hover{border-color:#00e85a;box-shadow:0 0 12px rgba(0,232,90,0.08);}
.ns-card-label{color:#4a6a4a;font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:0.5px;margin-bottom:0.35rem;}
.ns-card-val{color:#ffffff;font-size:22px;font-weight:700;line-height:1.2;}

/* MARKET SEARCH */
.market-search-input{width:100%;background:var(--bg3);border:1px solid var(--green3);color:var(--text);padding:7px 11px;border-radius:6px;font-family:'Inter',sans-serif;font-size:0.83rem;outline:none;margin-bottom:0.7rem;transition:border-color .2s;}
.market-search-input:focus{border-color:var(--green2);}
.market-search-input::placeholder{color:var(--muted);}

/* MARKET COMPARE v2 */
.mc2-compare{display:grid;grid-template-columns:1fr auto 1fr;gap:0.5rem;align-items:start;margin-bottom:0.8rem;}
.mc2-slot{background:var(--bg3);border:1px solid var(--border);border-radius:8px;padding:0.7rem;}
.mc2-label{font-size:0.65rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:0.8px;margin-bottom:0.35rem;}
.mc2-select{width:100%;background:var(--bg2);border:1px solid var(--green3);color:var(--text);padding:5px 7px;border-radius:5px;font-family:'Inter',sans-serif;font-size:0.75rem;outline:none;margin-bottom:0.4rem;}
.mc2-chart-wrap{height:90px;margin-bottom:0.4rem;}
.cw-main{background:var(--bg3);border:1px solid var(--border);border-radius:9px;padding:0.9rem;margin-bottom:0.8rem;}
.cw-main-header{display:flex;align-items:center;gap:0.6rem;margin-bottom:0.6rem;}
.cw-main-logo{width:32px;height:32px;border-radius:50%;object-fit:cover;flex-shrink:0;}
.cw-main-info{flex:1;min-width:0;}
.cw-main-name{font-size:0.92rem;font-weight:700;color:var(--text);}
.cw-main-pair{font-size:0.68rem;color:var(--muted);font-weight:600;letter-spacing:0.5px;}
.cw-main-rank{font-size:0.7rem;font-weight:700;color:var(--muted);background:var(--bg2);border:1px solid var(--border);border-radius:4px;padding:2px 6px;flex-shrink:0;}
.cw-main-price-row{display:flex;align-items:baseline;gap:0.7rem;margin-bottom:0.6rem;}
.cw-main-price{font-size:1.5rem;font-weight:800;color:var(--green);}
.cw-main-chg{font-size:0.82rem;font-weight:700;}
.cw-main-chg.pos{color:#00e85a;}.cw-main-chg.neg{color:#ff6464;}
.cw-main-stats{display:grid;grid-template-columns:1fr 1fr;gap:0.4rem;margin-bottom:0.7rem;}
.cw-stat2{background:var(--bg2);border:1px solid var(--border);border-radius:6px;padding:0.4rem 0.6rem;}
.cw-stat2-label{font-size:0.62rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:0.8px;margin-bottom:0.1rem;}
.cw-stat2-val{font-size:0.82rem;font-weight:700;color:var(--text2);}
.cw-main-search-row{display:flex;gap:0.4rem;margin-bottom:0.4rem;}
.mc2-vs{font-weight:800;color:var(--muted);font-size:0.78rem;align-self:center;text-align:center;padding-top:40px;}
.mc2-intervals{display:flex;flex-wrap:wrap;gap:0.2rem;}
.mc2-ibtn{background:var(--bg2);border:1px solid var(--border);color:var(--muted);padding:2px 5px;border-radius:3px;font-size:0.62rem;font-weight:600;cursor:pointer;font-family:'Inter',sans-serif;transition:all .15s;}
.mc2-ibtn:hover{border-color:var(--green3);color:var(--text2);}
.mc2-ibtn.active{background:var(--green4);border-color:var(--green3);color:var(--green);}

/* MARKET LIST v2 */
.market-list2{max-height:200px;overflow-y:auto;}
.ml-row2{display:flex;align-items:center;gap:0.5rem;padding:0.4rem 0;border-bottom:1px solid var(--border);font-size:0.78rem;}
.ml-row2:last-child{border-bottom:none;}
.ml-logo2{border-radius:50%;object-fit:cover;flex-shrink:0;}
.ml-name2{flex:1;font-weight:600;color:var(--text);}
.ml-sym2{color:var(--muted);font-size:0.68rem;min-width:30px;}
.ml-price2{font-weight:700;color:var(--text);min-width:60px;text-align:right;}
.ml-chg2{font-size:0.7rem;font-weight:700;min-width:48px;text-align:right;}
.ml-chg2.pos{color:#00e85a;}.ml-chg2.neg{color:#ff6464;}
.market-all-title{font-size:0.68rem;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.4rem;}


/* MINE STAT NOTE */
.mine-stat-note{font-size:0.62rem;color:var(--muted);margin-top:0.2rem;line-height:1.4;}

/* MINE POWER SLIDER */
.mine-power-wrap{margin-bottom:0.9rem;}
.mine-power-label{font-size:0.78rem;color:var(--text2);display:block;margin-bottom:0.35rem;}
.power-slider{width:100%;accent-color:var(--green);cursor:pointer;}

/* SELECT FILTER */
.mc2-filter{width:100%;background:var(--bg2);border:1px solid var(--green3);color:var(--text);padding:4px 7px;border-radius:5px;font-family:'Inter',sans-serif;font-size:0.73rem;outline:none;margin-bottom:0.3rem;transition:border-color .2s;}
.mc2-filter:focus{border-color:var(--green2);}
.mc2-filter::placeholder{color:var(--muted);}

/* CHART ERROR */
.chart-err{font-size:0.72rem;color:#ff9f43;background:rgba(255,159,67,0.07);border:1px solid rgba(255,159,67,0.2);border-radius:5px;padding:5px 8px;margin-top:0.3rem;line-height:1.4;}

/* NEWS FEED (kept for compat) */
.news-feed{display:flex;flex-direction:column;gap:0.5rem;}
.news-empty{color:var(--muted);font-size:0.78rem;padding:0.4rem 0;}

/* POSTS FEED */
.posts-feed{height:500px;overflow-y:auto;border:1px solid rgba(0,232,90,0.15);border-radius:12px;background:#0d1b2a;padding:1rem;}
.ann-card{background:#0a1628;border:2px solid #1e90ff;border-radius:12px;padding:1.2rem 1.4rem;margin-bottom:1.2rem;box-shadow:0 4px 24px rgba(30,144,255,0.12);}
.ann-card-header{display:flex;align-items:center;justify-content:space-between;margin-bottom:.75rem;gap:.5rem;}
.ann-official-badge{background:rgba(30,144,255,0.15);color:#1e90ff;font-size:.72rem;font-weight:700;padding:3px 10px;border-radius:20px;border:1px solid rgba(30,144,255,0.35);white-space:nowrap;}
.ann-card-date{font-size:.72rem;color:#5a7a9a;white-space:nowrap;}
.ann-title{font-size:1.1rem;font-weight:700;color:#fff;margin-bottom:.6rem;line-height:1.35;}
.ann-badge{background:#1e90ff22;color:#1e90ff;font-size:.7rem;padding:2px 8px;border-radius:4px;margin-left:8px;}
.ann-content{font-size:.88rem;color:#d0e0f0;line-height:1.65;}
.ann-content img{max-width:100%%;border-radius:8px;margin-top:.5rem;}
.ann-content a{color:#5bc8ff;text-decoration:underline;}
.ann-cover-img{width:100%%;border-radius:8px;margin-top:.8rem;display:block;max-height:360px;object-fit:cover;}
.ann-date{font-size:.75rem;color:#5a7a9a;margin-top:.5rem;}
.ann-toolbar{display:flex;flex-wrap:wrap;gap:.4rem;margin-bottom:.5rem;}
.ann-toolbar button,.ann-toolbar-upload{background:#1e1e1e;border:1px solid #333;border-radius:4px;padding:3px 10px;color:#ccc;cursor:pointer;font-size:.8rem;}
.ann-toolbar button:hover,.ann-toolbar-upload:hover{background:#2a2a2a;color:#fff;}
.ann-toolbar-upload{display:inline-flex;align-items:center;gap:4px;}
.ann-editor{min-height:120px;background:#111;border:1px solid #222;border-radius:6px;padding:.6rem;color:#fff;font-size:.9rem;line-height:1.6;margin-bottom:.75rem;outline:none;}
.ann-editor:empty:before{content:attr(placeholder);color:#555;pointer-events:none;}
.ann-preview{min-height:60px;background:#0d0d0d;border:1px solid #1e1e1e;border-radius:6px;padding:.6rem;color:#ccc;font-size:.9rem;line-height:1.6;}
.ann-preview img{max-width:100%%;border-radius:6px;}
.posts-feed::-webkit-scrollbar{width:5px;}
.posts-feed::-webkit-scrollbar-track{background:transparent;}
.posts-feed::-webkit-scrollbar-thumb{background:var(--green3);border-radius:3px;}
.posts-feed::-webkit-scrollbar-thumb:hover{background:var(--green2);}
.posts-loading,.posts-empty{color:var(--muted);font-size:0.78rem;padding:0.4rem 0;}
.post-card{background:#0a1628;border:1px solid rgba(0,232,90,0.1);border-radius:10px;padding:1rem;margin-bottom:0.6rem;box-shadow:0 2px 8px rgba(0,0,0,0.3);transition:border-color .15s;}
.post-card:last-child{margin-bottom:0;}
.post-card:hover{border-color:rgba(0,232,90,0.3);}
.post-header{display:flex;align-items:center;gap:0.5rem;margin-bottom:0.5rem;}
.post-avatar{cursor:pointer;flex-shrink:0;line-height:0;}
.post-meta{display:flex;align-items:center;gap:0.5rem;flex:1;}
.post-pseudo{font-size:0.8rem;font-weight:700;color:#5bc8ff;cursor:pointer;}
.post-pseudo:hover{color:#8dd8ff;}
.post-age{font-size:0.68rem;color:#4a6080;margin-left:auto;}
.post-content{font-size:0.84rem;color:#ffffff;line-height:1.55;word-break:break-word;margin-bottom:0.55rem;}
.post-image{max-width:100%;max-height:280px;object-fit:cover;border-radius:10px;border:1px solid var(--green3);display:block;cursor:pointer;margin:0.5rem 0;transition:opacity .15s;}.post-image:hover{opacity:0.85;}
#img-lightbox{display:none;position:fixed;inset:0;z-index:9999;background:rgba(0,0,0,0.92);align-items:center;justify-content:center;cursor:zoom-out;}
#img-lightbox.open{display:flex;}
#img-lightbox img{max-width:90vw;max-height:90vh;border-radius:8px;object-fit:contain;box-shadow:0 8px 40px rgba(0,0,0,0.8);}
.post-actions{display:flex;gap:0.4rem;flex-wrap:wrap;}
.post-react-btn{background:rgba(0,232,90,0.07);border:1px solid rgba(0,232,90,0.2);color:#00e85a;padding:3px 8px;border-radius:12px;font-size:0.7rem;cursor:pointer;transition:all .15s;font-family:'Inter',sans-serif;}
.post-react-btn:hover{background:rgba(0,232,90,0.15);border-color:rgba(0,232,90,0.4);color:#00e85a;}
.post-comment-btn,.post-view-btn{background:rgba(0,232,90,0.07);border:1px solid rgba(0,232,90,0.2);color:#00b847;padding:3px 8px;border-radius:12px;font-size:0.7rem;cursor:pointer;transition:all .15s;font-family:'Inter',sans-serif;}
.post-comment-btn:hover,.post-view-btn:hover{background:rgba(0,232,90,0.15);border-color:rgba(0,232,90,0.4);color:#00e85a;}
.post-comment-input{flex:1;background:#071020;border:1px solid rgba(0,232,90,0.25);color:#fff;padding:5px 9px;border-radius:6px;font-family:'Inter',sans-serif;font-size:0.8rem;outline:none;transition:border-color .2s;}.post-comment-input:focus{border-color:#00e85a;}.post-comment-input::placeholder{color:#4a6a4a;}
.post-comment-submit{background:rgba(0,232,90,0.07);border:1px solid rgba(0,232,90,0.2);color:#00e85a;padding:5px 10px;border-radius:6px;font-size:0.78rem;font-weight:600;cursor:pointer;font-family:'Inter',sans-serif;transition:all .2s;white-space:nowrap;}
.post-comment-submit:hover{background:rgba(0,232,90,0.18);border-color:#00e85a;color:#00e85a;}
.comment-item{display:flex;gap:0.5rem;align-items:flex-start;padding:0.5rem 0;border-bottom:1px solid rgba(255,255,255,0.06);}
.comment-item:last-child{border-bottom:none;}
.comment-avatar{flex-shrink:0;line-height:0;}
.comment-body{flex:1;}
.comment-pseudo{font-size:0.78rem;font-weight:700;color:#5bc8ff;}
.comment-age{font-size:0.65rem;color:#4a6080;margin-left:0.4rem;}
.comment-text{font-size:0.8rem;color:#e8e8e8;line-height:1.5;margin-top:0.15rem;word-break:break-word;}
.cmt-actions{display:flex;gap:0.6rem;margin-top:0.3rem;align-items:center;}
.cmt-like-btn{background:none;border:none;cursor:pointer;font-size:0.78rem;padding:0;display:inline-flex;align-items:center;gap:2px;}
.cmt-reply-btn{background:none;border:none;cursor:pointer;font-size:0.78rem;color:#666;padding:0;}
.cmt-reply-btn:hover{color:#00e85a;}
.cmt-reply-box{display:flex;gap:0.4rem;margin-top:0.4rem;align-items:center;}
.cmt-reply-box.hidden{display:none;}

/* BLOCK TIMER */
.block-progress-wrap{background:#071020;border:1px solid #1a2a1a;border-radius:12px;padding:14px 16px;margin-bottom:0.6rem;}
.bp-elapsed{font-size:0.78rem;color:var(--text2);}


/* POST DELETE BUTTON & ACTIVE REACTIONS */
.post-delete-btn{background:none;border:none;color:var(--muted);cursor:pointer;padding:4px 6px;border-radius:4px;font-size:0.72rem;transition:all .15s;margin-left:auto;}
.post-delete-btn:hover{color:#ff6464;background:rgba(255,100,100,0.08);}
.admin-del{background:none;border:none;color:#ff4444;cursor:pointer;font-size:14px;opacity:0.6;padding:2px 6px;border-radius:4px;}
.admin-del:hover{opacity:1;background:rgba(255,68,68,0.1);}
.post-react-btn.active{background:var(--green4);border-color:var(--green3);color:var(--green);font-weight:700;}

/* DEVISE TOGGLE */
.devise-toggle{display:flex;gap:0.2rem;}
.devise-btn{background:var(--bg3);border:1px solid var(--border);color:var(--muted);padding:2px 8px;border-radius:4px;font-size:0.68rem;font-weight:700;cursor:pointer;font-family:'Inter',sans-serif;transition:all .15s;}
.devise-btn.active{background:var(--green4);border-color:var(--green3);color:var(--green);}
.devise-btn:hover{border-color:var(--green3);}

/* PUBLISH FAB */
.publish-fab{position:fixed;bottom:1.8rem;right:5.5rem;z-index:300;width:50px;height:50px;background:#4f9fff;border-radius:50%;display:flex;align-items:center;justify-content:center;cursor:pointer;box-shadow:0 4px 18px rgba(79,159,255,0.45);transition:all .2s;color:#fff;}
.publish-fab:hover{transform:scale(1.08);}
.publish-modal-overlay{position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,0.78);z-index:500;display:flex;align-items:center;justify-content:center;}
.publish-modal{background:#0a1628;border:1px solid rgba(0,232,90,0.2);border-radius:14px;padding:1.8rem;max-width:440px;width:90%;box-shadow:0 0 60px rgba(0,0,0,0.8);}
.publish-modal-title{font-size:1rem;font-weight:700;margin-bottom:0.9rem;color:#fff;}
.publish-textarea{width:100%;background:#071020;border:1px solid rgba(0,232,90,0.25);color:#fff;padding:0.8rem;border-radius:7px;font-family:'Inter',sans-serif;font-size:0.88rem;outline:none;resize:vertical;min-height:100px;line-height:1.55;transition:border-color .2s;}
.publish-textarea:focus{border-color:#00e85a;}
.publish-textarea::placeholder{color:#4a6a4a;}
.publish-charcount{font-size:0.72rem;color:var(--muted);text-align:right;margin-top:0.3rem;margin-bottom:0.8rem;}
.publish-actions{display:flex;gap:0.7rem;justify-content:flex-end;}
.publish-cancel-btn{background:transparent;border:1px solid var(--border);color:var(--text2);padding:8px 18px;border-radius:6px;cursor:pointer;font-family:'Inter',sans-serif;font-size:0.88rem;transition:all .2s;}
.publish-cancel-btn:hover{border-color:var(--green3);}
.publish-submit-btn{background:rgba(0,232,90,0.07);border:1px solid rgba(0,232,90,0.3);color:#00e85a;padding:8px 22px;border-radius:6px;cursor:pointer;font-family:'Inter',sans-serif;font-size:0.88rem;font-weight:700;transition:all .2s;}
.publish-submit-btn:hover{background:rgba(0,232,90,0.18);border-color:#00e85a;}
.publish-toolbar{display:flex;gap:0.4rem;margin-bottom:0.6rem;}
.publish-tool-btn{background:var(--bg3);border:1px solid var(--green3);color:var(--text2);padding:5px 10px;border-radius:6px;cursor:pointer;font-size:0.88rem;font-family:'Inter',sans-serif;transition:all .2s;}
.publish-tool-btn:hover{border-color:var(--green2);color:var(--green);}
.publish-img-preview{margin-bottom:0.6rem;}.publish-img-preview img{max-width:100%;max-height:200px;border-radius:8px;border:1px solid var(--green3);}


/* CHAT */
.chat-panel{background:#0a1628;border:1px solid #00e85a;border-radius:12px;display:flex;flex-direction:column;height:480px;margin-bottom:0.8rem;overflow:hidden;}
.chat-header{display:flex;align-items:center;justify-content:space-between;padding:0.6rem 0.9rem;border-bottom:1px solid rgba(0,232,90,0.2);background:rgba(0,232,90,0.04);}
.chat-title{font-size:0.88rem;font-weight:700;color:var(--green);}
.chat-online{font-size:0.72rem;color:#4a6a4a;}
.chat-messages{flex:1;overflow-y:auto;padding:0.5rem 0.7rem;display:flex;flex-direction:column;gap:0.4rem;}
.chat-messages::-webkit-scrollbar{width:4px;}.chat-messages::-webkit-scrollbar-track{background:transparent;}.chat-messages::-webkit-scrollbar-thumb{background:var(--green3);border-radius:2px;}
.chat-msg{padding:0.3rem 0;border-bottom:1px solid rgba(0,232,90,0.05);}
.chat-msg:last-child{border-bottom:none;}
.chat-msg-header{display:flex;align-items:center;gap:0.4rem;margin-bottom:0.15rem;}
.chat-avatar{width:20px;height:20px;border-radius:50%;object-fit:cover;flex-shrink:0;}
.chat-avatar-placeholder{width:20px;height:20px;border-radius:50%;background:var(--green3);flex-shrink:0;display:inline-block;}
.chat-avatar-initials{width:20px;height:20px;border-radius:50%;background:var(--green3);flex-shrink:0;display:inline-flex;align-items:center;justify-content:center;font-size:0.6rem;font-weight:700;color:var(--green);}
.chat-pseudo{color:#00e85a;font-weight:700;font-size:0.8rem;}
.chat-time{color:#4a6a4a;font-size:0.68rem;margin-left:auto;}
.chat-actions{display:flex;gap:0.15rem;opacity:0;transition:opacity .15s;}
.chat-msg:hover .chat-actions{opacity:1;}
.chat-react-btn,.chat-menu-btn{background:none;border:1px solid transparent;color:var(--text2);cursor:pointer;font-size:0.72rem;padding:1px 4px;border-radius:4px;transition:all .15s;}
.chat-react-btn:hover,.chat-menu-btn:hover{border-color:var(--green3);color:var(--green);}
.chat-msg-body{color:#ffffff;font-weight:500;font-size:14px;line-height:1.5;padding-left:24px;word-break:break-word;}
.chat-msg-body a{color:var(--green2);}
.chat-mention{color:#00e85a;font-weight:600;}
.mention-link{color:#5bc8ff;font-weight:600;cursor:pointer;text-decoration:none;}
.mention-link:hover{text-decoration:underline;}
.chat-gif img{max-width:160px;max-height:120px;border-radius:6px;margin-top:0.25rem;display:block;}
.chat-img{max-width:200px;max-height:200px;border-radius:8px;cursor:pointer;margin-top:4px;display:block;}
.chat-media-preview{position:relative;display:inline-block;padding:6px 8px;background:rgba(255,255,255,0.04);border-top:1px solid rgba(0,232,90,0.1);width:100%;box-sizing:border-box;}
.chat-media-preview.hidden{display:none;}
.chat-media-preview-img{max-height:120px;max-width:100%;border-radius:6px;display:block;}
.chat-media-cancel{position:absolute;top:4px;right:6px;background:#c0392b;border:none;color:#fff;border-radius:50%;width:20px;height:20px;font-size:12px;line-height:1;cursor:pointer;display:flex;align-items:center;justify-content:center;padding:0;z-index:1;}
.chat-img-btn{cursor:pointer;background:#0a1628;border:1px solid rgba(0,232,90,0.25);border-radius:6px;padding:4px 8px;font-size:1rem;color:#ccc;display:inline-flex;align-items:center;line-height:1;}
.chat-reactions{display:flex;flex-wrap:wrap;gap:0.2rem;padding-left:24px;margin-top:0.15rem;}
.chat-reaction-chip{background:rgba(0,232,90,0.07);border:1px solid rgba(0,232,90,0.2);color:var(--text2);font-size:0.72rem;padding:1px 6px;border-radius:10px;cursor:pointer;transition:all .15s;}
.chat-reaction-chip:hover{border-color:var(--green);color:var(--green);}
.chat-reaction-mine{border-color:var(--green);color:var(--green);background:rgba(0,232,90,0.15);}
.chat-input-area{border-top:1px solid rgba(0,232,90,0.2);background:#071020;padding:0.5rem;position:relative;}
.chat-toolbar{display:flex;align-items:center;gap:0.35rem;}
.chat-input{flex:1;background:#071020;border:1px solid rgba(0,232,90,0.25);color:#fff;padding:7px 10px;border-radius:6px;font-family:'Inter',sans-serif;font-size:0.85rem;outline:none;transition:border-color .2s;}
.chat-input:focus{border-color:#00e85a;}
.chat-input::placeholder{color:#4a6a4a;}
.chat-emoji-btn,.chat-gif-btn,.chat-send-btn{background:rgba(0,232,90,0.07);border:1px solid rgba(0,232,90,0.2);color:var(--green2);cursor:pointer;padding:6px 9px;border-radius:6px;font-size:0.82rem;transition:all .15s;white-space:nowrap;}
.chat-emoji-btn:hover,.chat-gif-btn:hover,.chat-send-btn:hover{background:rgba(0,232,90,0.15);color:var(--green);}
.chat-mentions-list{position:absolute;bottom:100%;left:0;right:0;background:#071020;border:1px solid rgba(0,232,90,0.3);border-radius:6px;z-index:50;max-height:120px;overflow-y:auto;}
.mention-item{padding:5px 10px;cursor:pointer;font-size:0.82rem;color:var(--text2);}
.mention-item:hover{background:rgba(0,232,90,0.08);color:var(--green);}
.emoji-picker{position:fixed;z-index:200;background:#071020;border:1px solid #00e85a;border-radius:10px;width:340px;max-height:370px;display:flex;flex-direction:column;box-shadow:0 8px 32px rgba(0,0,0,0.6);overflow:hidden;}
.publish-emoji-picker{position:relative;background:var(--bg3);border:1px solid var(--green3);border-radius:8px;overflow:hidden;margin-bottom:0.6rem;display:flex;flex-direction:column;max-height:320px;}
.epk-search{width:100%;background:#0a1628;border:none;border-bottom:1px solid rgba(0,232,90,0.2);color:#fff;padding:7px 12px;font-size:0.82rem;outline:none;box-sizing:border-box;flex-shrink:0;}
.epk-cat-bar{display:flex;gap:2px;padding:3px 6px;border-bottom:1px solid rgba(0,232,90,0.15);background:#071020;flex-shrink:0;overflow-x:auto;}
.epk-cat-bar::-webkit-scrollbar{height:2px;}.epk-cat-bar::-webkit-scrollbar-thumb{background:var(--green3);}
.epk-cat-btn{cursor:pointer;font-size:1rem;padding:2px 5px;border-radius:4px;transition:background .1s;flex-shrink:0;}
.epk-cat-btn:hover{background:rgba(0,232,90,0.15);}
.epk-grid-wrap{flex:1;overflow-y:auto;padding:0.4rem 0.5rem;}
.epk-grid-wrap::-webkit-scrollbar{width:4px;}.epk-grid-wrap::-webkit-scrollbar-thumb{background:var(--green3);}
.epk-section{margin-bottom:0.5rem;}
.epk-cat-label{font-size:0.63rem;font-weight:700;color:var(--muted);margin-bottom:0.2rem;text-transform:uppercase;letter-spacing:0.04em;}
.epk-emojis{display:grid;grid-template-columns:repeat(8,1fr);gap:2px;}
.epk-item{cursor:pointer;font-size:1.1rem;padding:3px;border-radius:4px;text-align:center;transition:background .1s;line-height:1.2;}
.epk-item:hover{background:rgba(0,232,90,0.12);}
.epk-item-active{background:rgba(0,232,90,0.2);outline:1.5px solid var(--green);border-radius:4px;}
.gif-picker{position:absolute;bottom:100%;left:0;right:0;background:#071020;border:1px solid rgba(0,232,90,0.3);border-radius:8px;z-index:100;padding:0.5rem;margin-bottom:4px;}
.gif-search-input{width:100%;background:#0a1628;border:1px solid rgba(0,232,90,0.25);color:#fff;padding:6px 10px;border-radius:5px;font-size:0.82rem;outline:none;margin-bottom:0.4rem;}
.gif-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:4px;min-height:150px;max-height:200px;overflow-y:auto;}
.gif-item{width:100%;border-radius:4px;cursor:pointer;object-fit:cover;aspect-ratio:4/3;}
.gif-item:hover{opacity:0.8;}
.chat-ctx-menu{position:fixed;z-index:300;background:#071020;border:1px solid rgba(0,232,90,0.3);border-radius:8px;padding:0.3rem 0;min-width:160px;box-shadow:0 4px 20px rgba(0,0,0,0.5);}
.ctx-item{padding:7px 14px;font-size:0.82rem;color:var(--text2);cursor:pointer;transition:background .1s;}
.ctx-item:hover{background:rgba(0,232,90,0.08);color:var(--green);}
.ctx-sep{height:1px;background:rgba(0,232,90,0.15);margin:3px 0;}
.ctx-label{padding:4px 14px;font-size:0.68rem;color:var(--muted);font-weight:600;text-transform:uppercase;}
.chat-post-card{background:#071020;border-left:3px solid #00e85a;border-radius:8px;padding:10px;margin-top:6px;cursor:pointer;transition:background .15s;margin-left:24px;}
.chat-post-card:hover{background:#0d1e35;}
.chat-post-card-header{display:flex;align-items:center;gap:6px;margin-bottom:5px;}
.chat-post-card-author{color:#00e85a;font-weight:700;font-size:0.78rem;}
.chat-post-card-date{color:#4a6a4a;font-size:0.68rem;margin-left:auto;}
.chat-post-card-image{width:100%;max-height:150px;object-fit:cover;border-radius:4px;margin-bottom:5px;display:block;}
.chat-post-card-content{color:#ffffff;font-size:13px;overflow:hidden;display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;line-height:1.4;}
.chat-post-card-footer{color:#4a6a4a;font-size:0.68rem;margin-top:5px;}
.chat-share-preview{background:#071020;border:1px solid rgba(0,232,90,0.3);border-radius:6px;padding:6px 10px;margin-bottom:4px;display:flex;align-items:center;justify-content:space-between;font-size:0.78rem;color:var(--green2);}
.chat-share-preview button{background:none;border:none;color:var(--muted);cursor:pointer;font-size:0.9rem;padding:0 4px;}
.chat-share-preview button:hover{color:var(--text2);}
.post-share-btn{background:rgba(0,232,90,0.07);border:1px solid rgba(0,232,90,0.15);color:var(--text2);cursor:pointer;padding:4px 8px;border-radius:5px;font-size:0.72rem;transition:all .15s;}
.post-share-btn:hover{background:rgba(0,232,90,0.15);color:var(--green);}

/* FAB GROUP (cloche + messages) */
.fab-group{position:fixed;bottom:1.8rem;right:10rem;z-index:300;display:flex;align-items:center;gap:0.5rem;}

/* NOTIFICATION BUTTON */
.notif-wrapper{position:relative;}
.notif-btn{width:46px;height:46px;background:#071020;border:1px solid #00e85a;color:#00e85a;border-radius:50%;display:flex;align-items:center;justify-content:center;cursor:pointer;transition:all .2s;padding:0;flex-shrink:0;}
.notif-btn:hover{box-shadow:0 0 12px rgba(0,232,90,0.3);}
.notif-badge{position:absolute;top:-6px;right:-6px;background:#e74c3c;color:#fff;border-radius:50%;min-width:18px;height:18px;font-size:11px;font-weight:700;display:none;align-items:center;justify-content:center;padding:0 3px;pointer-events:none;z-index:1;line-height:1;}

/* NOTIFICATION PANEL */
.notif-panel{position:absolute;bottom:60px;right:0;width:340px;max-height:480px;z-index:10000;background:#0a1628;border:1px solid #00e85a;border-radius:12px;box-shadow:0 8px 32px rgba(0,200,90,0.15);display:flex;flex-direction:column;overflow:hidden;}
.notif-header{display:flex;align-items:center;gap:0.3rem;padding:0.55rem 0.7rem;border-bottom:1px solid rgba(0,232,90,0.2);background:rgba(0,232,90,0.04);flex-shrink:0;}
.notif-header>span:first-child{flex:1;font-size:0.88rem;font-weight:700;color:#00e85a;}
.notif-header button{background:none;border:none;color:var(--text2);cursor:pointer;font-size:0.82rem;padding:3px 7px;border-radius:4px;transition:background .15s;font-family:'Inter',sans-serif;}
.notif-header button:hover{background:rgba(0,232,90,0.1);color:#00e85a;}
.notif-list{overflow-y:auto;flex:1;}
.notif-item{display:flex;gap:0.6rem;padding:0.65rem 0.8rem;cursor:pointer;border-bottom:1px solid rgba(255,255,255,0.04);transition:background .15s;align-items:flex-start;}
.notif-item:hover{background:rgba(0,232,90,0.05);}
.notif-item.notif-unread{background:rgba(0,232,90,0.07);}
.notif-avatar{width:32px;height:32px;border-radius:50%;overflow:hidden;flex-shrink:0;display:flex;align-items:center;justify-content:center;background:var(--bg3);}
.notif-avatar svg{width:32px;height:32px;}
.notif-avatar-fallback{font-size:0.9rem;font-weight:700;color:#00e85a;text-transform:uppercase;}
.notif-body{flex:1;min-width:0;}
.notif-text{font-size:0.8rem;color:var(--text);line-height:1.3;margin-bottom:2px;}
.notif-preview{font-size:0.75rem;color:var(--muted);white-space:nowrap;overflow:hidden;text-overflow:ellipsis;margin-bottom:2px;}
.notif-age{font-size:0.7rem;color:var(--muted);}
.notif-empty{padding:1.5rem;text-align:center;color:var(--muted);font-size:0.85rem;}
@keyframes notif-flash{0%,100%{background:transparent;}50%{background:rgba(0,232,90,0.15);border-radius:8px;}}
.notif-highlight{animation:notif-flash 0.6s ease-in-out 3;}
/* NOTIFICATION TOASTS */
#notif-toast-container{position:fixed;bottom:24px;right:24px;z-index:99999;display:flex;flex-direction:column-reverse;gap:10px;pointer-events:none;}
.notif-toast{pointer-events:all;background:#0d1b2a;color:#fff;border:1px solid rgba(0,232,90,0.25);border-radius:8px;box-shadow:0 4px 24px rgba(0,0,0,0.5);padding:12px 14px;min-width:280px;max-width:340px;display:flex;align-items:flex-start;gap:10px;transform:translateX(120%);opacity:0;transition:transform 0.35s cubic-bezier(0.22,1,0.36,1),opacity 0.25s;}
.notif-toast.toast-in{transform:translateX(0);opacity:1;}
.notif-toast.toast-out{transform:translateX(120%);opacity:0;}
.notif-toast-icon{width:32px;height:32px;border-radius:50%;background:#132035;display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:700;color:#00e85a;flex-shrink:0;}
.notif-toast-body{flex:1;min-width:0;}
.notif-toast-text{font-size:13px;color:#e0e0e0;line-height:1.35;margin-bottom:2px;}
.notif-toast-preview{font-size:11px;color:#7a8fa6;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;}
.notif-toast-close{background:none;border:none;color:#7a8fa6;cursor:pointer;font-size:16px;line-height:1;padding:0 0 0 4px;flex-shrink:0;align-self:flex-start;}

/* DM WINDOW */
.dm-open-btn{background:#071020;border:1px solid #00e85a;color:#00e85a;border-radius:50px;padding:10px 16px;font-size:0.82rem;font-weight:700;cursor:pointer;font-family:'Inter',sans-serif;transition:all .2s;display:flex;align-items:center;gap:0.4rem;position:relative;}
.dm-open-btn:hover{background:#071020;box-shadow:0 0 12px rgba(0,232,90,0.3);}
.dm-badge{position:absolute;top:-8px;right:-8px;background:#e74c3c;color:#fff;border-radius:50%;min-width:18px;height:18px;font-size:11px;font-weight:700;display:none;align-items:center;justify-content:center;padding:0 3px;pointer-events:none;z-index:1;line-height:1;}
.dm-window{position:fixed;bottom:80px;right:20px;width:360px;height:520px;z-index:10000;background:#0a1628;border:1px solid #00e85a;border-radius:12px;box-shadow:0 8px 32px rgba(0,200,90,0.15);display:flex;flex-direction:column;overflow:hidden;}
.dm-header{display:flex;align-items:center;gap:0.3rem;padding:0.55rem 0.7rem;border-bottom:1px solid rgba(0,232,90,0.2);background:rgba(0,232,90,0.04);flex-shrink:0;}
.dm-header>span:first-child{flex:1;font-size:0.88rem;font-weight:700;color:#00e85a;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;}
.dm-header button{background:none;border:none;color:var(--text2);cursor:pointer;font-size:0.88rem;padding:3px 6px;border-radius:4px;transition:background .15s;white-space:nowrap;flex-shrink:0;}
.dm-header button:hover{background:rgba(0,232,90,0.1);color:#00e85a;}
.dm-view{display:flex;flex-direction:column;flex:1;overflow:hidden;}
.dm-search{width:100%;background:#071020;border:none;border-bottom:1px solid rgba(0,232,90,0.12);color:#fff;padding:8px 12px;font-family:'Inter',sans-serif;font-size:0.83rem;outline:none;flex-shrink:0;box-sizing:border-box;}
.dm-search::placeholder{color:#4a6a4a;}
.dm-list{flex:1;overflow-y:auto;}
.dm-list::-webkit-scrollbar{width:3px;}.dm-list::-webkit-scrollbar-thumb{background:var(--green3);}
.dm-conv-item{display:flex;align-items:center;gap:0.5rem;padding:0.5rem 0.8rem;cursor:pointer;border-bottom:1px solid rgba(0,232,90,0.05);border-left:3px solid transparent;transition:background .15s;}
.dm-conv-item:hover{background:rgba(0,232,90,0.06);}
.dm-conv-item.unread{background:#0d2010;border-left:3px solid #00e85a;}
.dm-conv-item.unread:hover{background:#122918;}
.dm-conv-avatar-wrap{width:36px;height:36px;border-radius:50%;overflow:hidden;flex-shrink:0;display:inline-flex;align-items:center;justify-content:center;background:var(--green4);}
.dm-conv-info{flex:1;min-width:0;}
.dm-conv-pseudo{display:block;color:#00e85a;font-weight:700;font-size:0.82rem;}
.dm-conv-item.unread .dm-conv-pseudo{color:#fff;font-weight:800;}
.dm-conv-preview{display:block;color:#7ab98a;font-size:0.72rem;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;}
.dm-conv-item.unread .dm-conv-preview{color:#e0e0e0;}
.dm-conv-item:not(.unread) .dm-conv-preview{font-style:italic;color:#4a6a4a;}
.dm-conv-meta{display:flex;flex-direction:column;align-items:flex-end;gap:2px;flex-shrink:0;}
.dm-conv-time{color:#4a6a4a;font-size:0.65rem;}
.dm-conv-unread-count{background:#e74c3c;color:#fff;border-radius:50%;min-width:20px;height:20px;font-size:11px;font-weight:700;display:flex;align-items:center;justify-content:center;padding:0 3px;}
.dm-unread-dot{width:7px;height:7px;border-radius:50%;background:#00e85a;display:inline-block;flex-shrink:0;margin-left:2px;animation:dm-dot-pulse 1.5s infinite;}
@keyframes dm-dot-pulse{0%,100%{opacity:1;transform:scale(1);}50%{opacity:0.45;transform:scale(0.7);}}
.dm-messages{flex:1;overflow-y:auto;padding:0.5rem 0.7rem;display:flex;flex-direction:column;gap:0.35rem;}
.dm-messages::-webkit-scrollbar{width:3px;}.dm-messages::-webkit-scrollbar-thumb{background:var(--green3);}
.dm-msg{max-width:82%;display:flex;flex-direction:column;position:relative;}
.dm-msg-mine{align-self:flex-end;align-items:flex-end;}
.dm-msg-their{align-self:flex-start;align-items:flex-start;}
.dm-msg-content{padding:7px 11px;border-radius:12px;font-size:0.84rem;line-height:1.45;color:#fff;word-break:break-word;}
.dm-msg-mine .dm-msg-content{background:#1a3a1a;border-bottom-right-radius:3px;}
.dm-msg-their .dm-msg-content{background:#071020;border-bottom-left-radius:3px;border:1px solid rgba(0,232,90,0.1);}
.dm-msg-meta{display:flex;align-items:center;gap:4px;margin-top:2px;padding:0 3px;}
.dm-msg-time{color:#4a6a4a;font-size:0.65rem;}
.dm-msg-status{color:#4a6a4a;font-size:0.65rem;}
.dm-gif{max-width:160px;max-height:120px;border-radius:6px;margin-top:4px;display:block;}
.dm-delete-btn{background:none;border:none;color:#4a6a4a;cursor:pointer;font-size:0.7rem;opacity:0;transition:opacity .15s;padding:2px 4px;border-radius:3px;align-self:center;margin:0 2px;}
.dm-msg:hover .dm-delete-btn{opacity:1;}
.dm-delete-btn:hover{color:#e74c3c;}
.dm-input-area{border-top:1px solid rgba(0,232,90,0.2);background:#071020;padding:0.4rem;position:relative;flex-shrink:0;}
.dm-toolbar{display:flex;align-items:center;gap:0.3rem;}
.dm-settings-panel{padding:0.9rem;display:flex;flex-direction:column;gap:0.5rem;overflow-y:auto;flex:1;}
.dm-settings-panel>p{margin:0 0 0.3rem;}
.dm-setting-option{display:flex;align-items:flex-start;gap:8px;padding:8px 10px;border:1px solid rgba(0,232,90,0.15);border-radius:7px;cursor:pointer;transition:background .15s;}
.dm-setting-option:hover{background:rgba(0,232,90,0.05);}
.dm-setting-option input{margin-top:3px;flex-shrink:0;}
.dm-setting-option div{display:flex;flex-direction:column;gap:1px;}
.dm-setting-option span{color:#d4f5e0;font-size:0.82rem;font-weight:600;}
.dm-setting-option small{color:#4a6a4a;font-size:0.7rem;}
.dm-save-btn{width:100%;background:#00e85a;color:#000;border:none;padding:9px;border-radius:7px;font-size:0.88rem;font-weight:700;cursor:pointer;font-family:'Inter',sans-serif;transition:all .2s;margin-top:0.3rem;}
.dm-save-btn:hover{background:#00b847;}
.dm-empty{text-align:center;color:#4a6a4a;font-size:0.78rem;padding:2rem 1rem;}
.dm-toast-container{position:fixed;bottom:20px;left:20px;z-index:20000;display:flex;flex-direction:column;gap:8px;pointer-events:none;}
.dm-toast{background:#0a1628;border:1px solid #00e85a;border-radius:8px;padding:12px;display:flex;align-items:center;gap:10px;cursor:pointer;animation:dm-slide-in .3s ease;box-shadow:0 4px 20px rgba(0,0,0,0.5);pointer-events:all;max-width:280px;}
@keyframes dm-slide-in{from{transform:translateX(-110%);opacity:0}to{transform:translateX(0);opacity:1}}
.dm-toast-avatar{width:32px;height:32px;border-radius:50%;overflow:hidden;display:inline-flex;align-items:center;justify-content:center;flex-shrink:0;background:var(--green4);}
.dm-toast-body{flex:1;min-width:0;}
.dm-toast-pseudo{color:#00e85a;font-size:0.82rem;font-weight:700;}
.dm-toast-preview{color:#fff;font-size:0.75rem;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;}

/* GROUP DM */
.dm-new-group-btn{background:transparent;color:#00e85a;border:1px solid #00e85a;border-radius:6px;padding:3px 7px;font-size:0.78rem;cursor:pointer;font-family:'Inter',sans-serif;transition:all .15s;flex-shrink:0;}
.dm-new-group-btn:hover{background:rgba(0,232,90,0.1);}
.dm-create-group{padding:0.6rem;display:flex;flex-direction:column;gap:0.4rem;overflow-y:auto;flex:1;}
.dm-member-chip{background:#071020;border:1px solid #00e85a;border-radius:20px;padding:4px 10px;display:flex;align-items:center;gap:6px;font-size:0.78rem;color:#d4f5e0;}
.dm-member-chip button{background:none;border:none;color:#e74c3c;cursor:pointer;padding:0 2px;font-size:0.9rem;line-height:1;}
.group-avatar-placeholder{background:#0d2010;color:#00e85a;border-radius:50%;display:flex;align-items:center;justify-content:center;font-weight:700;font-size:0.75rem;width:36px;height:36px;flex-shrink:0;}
.dm-conv-avatar-outer{position:relative;width:36px;height:36px;flex-shrink:0;display:flex;align-items:center;justify-content:center;}
.dm-group-indicator{position:absolute;bottom:-1px;right:-1px;background:#071020;border-radius:50%;font-size:9px;line-height:1;padding:1px 0;}
.dm-group-members-count{color:#7ab98a;font-size:0.72rem;cursor:pointer;text-decoration:underline;}
.dm-group-members-count:hover{color:#00e85a;}
.dm-group-settings-btn{background:none;border:none;color:#7ab98a;cursor:pointer;font-size:0.88rem;padding:3px 6px;border-radius:4px;transition:background .15s;flex-shrink:0;}
.dm-group-settings-btn:hover{background:rgba(0,232,90,0.1);color:#00e85a;}
.dm-group-info-panel{padding:0.6rem;display:flex;flex-direction:column;gap:0.4rem;overflow-y:auto;flex:1;}
.dm-group-member-row{display:flex;align-items:center;gap:0.5rem;padding:0.4rem 0;border-bottom:1px solid rgba(0,232,90,0.05);}
.dm-group-member-pseudo{flex:1;color:#d4f5e0;font-size:0.82rem;}
.dm-group-member-remove{background:none;border:none;color:#e74c3c;cursor:pointer;font-size:0.8rem;padding:2px 6px;border-radius:4px;}
.dm-group-member-remove:hover{background:rgba(231,76,60,0.15);}
.dm-danger-btn{background:transparent;color:#e74c3c;border:1px solid #e74c3c;border-radius:8px;padding:8px 14px;font-weight:600;font-size:0.82rem;cursor:pointer;font-family:'Inter',sans-serif;width:100%;transition:all .15s;margin-top:auto;}
.dm-danger-btn:hover{background:rgba(231,76,60,0.1);}
.dm-search-add-input{width:100%;background:#071020;border:1px solid rgba(0,232,90,0.2);border-radius:6px;color:#fff;padding:6px 10px;font-family:'Inter',sans-serif;font-size:0.82rem;outline:none;box-sizing:border-box;}
.dm-search-add-results{max-height:150px;overflow-y:auto;}
.dm-group-msg-from{font-size:0.7rem;color:#00e85a;font-weight:600;margin-bottom:2px;}
.dm-group-msg-mine,.dm-group-msg-their{flex-direction:row;align-items:flex-end;max-width:90%;}
.dm-group-msg-mine{align-self:flex-end;}
.dm-group-msg-their{align-self:flex-start;}
.dm-group-msg-mine .dm-msg-content{background:#1a3a1a;border-bottom-right-radius:3px;}
.dm-group-msg-their .dm-msg-content{background:#071020;border-bottom-left-radius:3px;border:1px solid rgba(0,232,90,0.1);}
.dm-group-sender-avatar{width:22px;height:22px;border-radius:50%;overflow:hidden;display:inline-flex;flex-shrink:0;margin:0 4px;align-self:flex-end;}
.dm-group-readby{display:flex;gap:2px;margin-top:2px;flex-wrap:wrap;align-items:center;}
.dm-readby-avatar{width:12px;height:12px;border-radius:50%;overflow:hidden;display:inline-flex;flex-shrink:0;}
.dm-conv-item--added{opacity:0.5;cursor:default;}

/* LANG SELECTOR */
.lang-selector{display:flex;gap:0.2rem;}
.lang-btn{background:var(--bg3);border:1px solid var(--border);color:var(--muted);padding:4px 9px;border-radius:5px;font-size:0.78rem;font-weight:700;cursor:pointer;font-family:'Inter',sans-serif;transition:all .15s;}
.lang-btn:hover{border-color:var(--green3);color:var(--text2);}
.lang-btn.active{background:var(--green4);border-color:var(--green3);color:var(--green);}

/* PROFILE PAGE */
.profile-page{max-width:760px;margin:0 auto;}
.profile-tabs{display:flex;gap:0.5rem;margin-bottom:1.5rem;border-bottom:1px solid var(--green3);padding-bottom:0;}
.profile-tab{background:transparent;border:none;border-bottom:2px solid transparent;color:var(--text2);font-size:0.95rem;font-weight:600;padding:10px 20px;cursor:pointer;font-family:'Inter',sans-serif;transition:all .2s;margin-bottom:-1px;}
.profile-tab:hover{color:var(--green);}
.profile-tab.active{color:var(--green);border-bottom-color:var(--green);}
.profile-tab-content{display:block;}.profile-panel{display:block;}
.profile-section{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.4rem;margin-bottom:1.2rem;}
.profile-section-title{font-size:0.72rem;font-weight:700;color:var(--muted);text-transform:uppercase;letter-spacing:1.5px;margin-bottom:1rem;display:flex;align-items:center;gap:0.4rem;}
.profile-section-title::before{content:'';display:inline-block;width:3px;height:0.85em;background:var(--green);border-radius:2px;}
.profile-photo-area{display:flex;flex-direction:column;align-items:center;gap:0.8rem;padding:0.5rem 0;}
.wallet-stats{display:grid;grid-template-columns:repeat(3,1fr);gap:0.8rem;margin-bottom:1.2rem;}
@media(max-width:600px){.wallet-stats{grid-template-columns:1fr;}}
.wallet-stat-card{background:var(--bg2);border:1px solid var(--green3);border-radius:10px;padding:1rem 1.2rem;}
.wallet-stat-label{font-size:0.68rem;font-weight:700;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.35rem;}
.wallet-stat-value{font-size:1.4rem;font-weight:800;color:var(--text);}
.wallet-stat-value.green{color:var(--green);}
.wallet-address-box{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.4rem;margin-bottom:1.2rem;text-align:center;}
.wallet-address-label{font-size:0.68rem;font-weight:700;color:var(--muted);text-transform:uppercase;letter-spacing:1px;margin-bottom:0.6rem;}
.wallet-address-value{font-size:0.78rem;word-break:break-all;background:var(--bg3);border:1px solid var(--green3);border-radius:6px;padding:0.9rem;color:var(--green);line-height:1.6;}
.wallet-sr-grid{display:grid;grid-template-columns:1fr 1fr;gap:1rem;margin-bottom:1.2rem;}
@media(max-width:640px){.wallet-sr-grid{grid-template-columns:1fr;}}
.wallet-sr-card{background:var(--bg2);border:1px solid var(--green3);border-radius:12px;padding:1.4rem;}
.wallet-sr-title{font-size:0.95rem;font-weight:700;margin-bottom:1rem;color:var(--text);}
.blocked-user-row{display:flex;align-items:center;gap:0.7rem;padding:0.6rem 0;border-bottom:1px solid var(--border);}
.blocked-user-row:last-child{border-bottom:none;}
.blocked-user-avatar{display:inline-flex;flex-shrink:0;}
.blocked-user-pseudo{flex:1;font-size:0.88rem;font-weight:600;color:var(--text2);}
.scorbits-badge{display:inline-flex;align-items:center;justify-content:center;width:18px;height:18px;border-radius:50%;background:#00e85a;border:2px solid #000;margin-left:4px;flex-shrink:0;vertical-align:middle;font-size:9px;font-weight:800;color:#000;box-shadow:0 0 6px rgba(0,232,90,0.5);position:relative;top:-1px;}
.scorbits-badge span{color:#f5a623;font-weight:900;font-size:9px;line-height:1;}
</style>
<script>const I18N=` + i18nJSON + `;const LANG='` + lang + `';</script>
</head>
<body>
<canvas id="rain"></canvas>
<header>
  ` + navUserHTML + `
  <a class="logo" href="/"><img src="/static/scorbits_logo.png" alt="Scorbits" style="height:32px;width:auto;vertical-align:middle;margin-right:6px;"><span class="logo-text">Scorbits</span></a>
  <div class="header-right">
    <div style="display:flex;align-items:center;gap:0.5rem;">
    <div style="position:relative;">
      <form class="search-form" action="/search" onsubmit="return validateNavSearch(event)">
        <div style="position:relative;display:flex;align-items:center;">
          <input type="text" name="q" id="nav-search-q" placeholder="Bloc # ou adresse SCO..." oninput="onNavSearchInput()" autocomplete="off" style="padding-right:22px;">
          <button type="button" id="nav-search-clear" onclick="clearNavSearch()" style="position:absolute;right:6px;background:none;border:none;color:#666;cursor:pointer;font-size:15px;line-height:1;padding:0;display:none;top:50%;transform:translateY(-50%);">&#215;</button>
        </div>
        <button type="submit">` + i18n.T(lang, "nav.search_btn") + `</button>
      </form>
      <div id="nav-search-error" style="display:none;position:absolute;left:0;top:100%;z-index:200;background:#1a0a0a;border:1px solid #ff4444;color:#ff4444;font-size:.8rem;padding:.3rem .7rem;border-radius:4px;margin-top:4px;white-space:nowrap;">Aucun résultat trouvé</div>
    </div>
    <div class="lang-selector">
      <button class="lang-btn` + langEnActive + `" onclick="setLang('en')">` + i18n.T(lang, "nav.lang_en") + `</button>
      <button class="lang-btn` + langFrActive + `" onclick="setLang('fr')">` + i18n.T(lang, "nav.lang_fr") + `</button>
    </div>
    </div>
  </div>
</header>
<div class="ticker-wrap">
  <div class="halving-bar-container">
    <div class="halving-bar-left">
      <span style="font-family:Arial,sans-serif;font-size:11px;font-weight:700;color:#000;text-transform:uppercase;display:block;">Halving</span>
      <span style="font-family:Arial,sans-serif;font-size:15px;font-weight:700;color:#000;display:block;" id="halving-blocks">0 / 840,000</span>
    </div>
    <div class="halving-bar-track">
      <div id="halving-bar-fill" class="halving-bar-fill" style="width:0%"></div>
      <span id="halving-bar-pct">0.00%</span>
    </div>
  </div>
</div>
<main>` + content + `</main>
<footer>
  <div>Scorbits (SCO) &mdash; ` + i18n.T(lang, "footer.tagline") + ` &mdash; ` + year + `</div>
  <div class="footer-wp">
    <a href="/static/Scorbits_Whitepaper_EN.docx" download class="footer-wp-link">&#x1F1EC;&#x1F1E7; Whitepaper EN &#x2193;</a>
    <a href="/static/Scorbits_Whitepaper_FR.docx" download class="footer-wp-link">&#x1F1EB;&#x1F1F7; Whitepaper FR &#x2193;</a>
    <a href="/node" class="footer-wp-link">Run a Node</a>
    <a href="/wallets" class="footer-wp-link">Wallets</a>
    <a href="/transactions" class="footer-wp-link">Transactions</a>
    <a href="/pool" class="footer-wp-link">Mining Pool</a>
  </div>
</footer>
` + publishFAB + `

<!-- Modal profil utilisateur -->
<div id="profile-modal" class="profile-modal-overlay hidden" onclick="if(event.target===this)closeProfileModal()">
  <div class="profile-modal-box">
    <button class="pmo-close" onclick="closeProfileModal()">
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
    </button>
    <div id="profile-modal-content"></div>
  </div>
</div>

<script>
const IS_LOGGED_IN = ` + isLoggedIn + `;
const IS_ADMIN = ` + isAdmin + `;
const CHAT_IS_ADMIN = IS_ADMIN;
const CURRENT_PSEUDO = '` + strings.ReplaceAll(strings.ReplaceAll(userPseudo, `\`, `\\`), `'`, `\'`) + `';
const CURRENT_USER_ID = '` + userIDHex + `';
I18N['_pseudo']=CURRENT_PSEUDO;
function renderAdminBadge(pseudo) {
  if (pseudo === 'Yousse') {
    return '<span class="scorbits-badge" title="Scorbits Official"><span>S</span></span>';
  }
  return '';
}
function setLang(l){fetch('/set-lang',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({lang:l})}).then(()=>window.location.reload());}
function toggleNavMenu(){const corner=document.getElementById('nav-user-corner');const menu=document.getElementById('nav-user-menu');if(!corner||!menu)return;menu.classList.toggle('hidden');corner.classList.toggle('open');}
document.addEventListener('click',function(e){const corner=document.getElementById('nav-user-corner');const menu=document.getElementById('nav-user-menu');if(corner&&menu&&!corner.contains(e.target)){menu.classList.add('hidden');corner.classList.remove('open');}});
function navigateToProfileTab(tab){
  const menu=document.getElementById('nav-user-menu');
  if(menu){menu.classList.add('hidden');}
  const corner=document.getElementById('nav-user-corner');
  if(corner)corner.classList.remove('open');
  if(window.location.pathname==='/profile'){
    if(typeof window.switchProfileTab==='function'){window.switchProfileTab(tab);}
    window.location.hash=tab;
  } else {
    window.location.href='/profile#'+tab;
  }
}
window.navigateToProfileTab=navigateToProfileTab;

// ─── HALVING BAR ───
(function(){
  const HALVING_INTERVAL=840000;
  window.updateHalvingBar = async function(initBlock){
    try{
      let cur=0;
      if(initBlock!==undefined){
        cur=initBlock;
      } else {
        const r=await fetch('/api/stats');
        const d=await r.json();
        cur=d.last_block||0;
      }
      const blocksProgress=cur%HALVING_INTERVAL;
      const pct=blocksProgress/HALVING_INTERVAL*100;
      const fill=document.getElementById('halving-bar-fill');
      const blocks=document.getElementById('halving-blocks');
      const pctEl=document.getElementById('halving-bar-pct');
      if(fill)fill.style.width=pct.toFixed(4)+'%';
      if(blocks)blocks.textContent=blocksProgress.toLocaleString()+' / '+HALVING_INTERVAL.toLocaleString();
      if(pctEl)pctEl.textContent=pct.toFixed(2)+'%';
    }catch(e){}
  };
  window.updateHalvingBar();
  setInterval(window.updateHalvingBar,30000);
})();

// ─── MATRIX RAIN ───
(function(){
  const c = document.getElementById('rain');
  if (!c) return;
  const x = c.getContext('2d');
  function resize(){ c.width = window.innerWidth; c.height = window.innerHeight; }
  resize();
  window.addEventListener('resize', resize);
  let drops = [];
  function initDrops(){
    drops = Array(Math.floor(c.width/18)).fill(0).map(() => ({
      y: Math.random() * -120,
      isSCO: false,
      scoTimer: 0
    }));
  }
  initDrops();
  window.addEventListener('resize', initDrops);
  let scoInterval = 0;
  let mouseX = -999, mouseY = -999;
  document.addEventListener('mousemove', e => { mouseX = e.clientX; mouseY = e.clientY; });
  function draw(){
    x.fillStyle = 'rgba(4,13,5,0.1)';
    x.fillRect(0, 0, c.width, c.height);
    scoInterval++;
    for (let i = 0; i < drops.length; i++){
      const d = drops[i];
      let ch = Math.random() > 0.5 ? '1' : '0';
      let alpha = 0.18 + Math.random() * 0.35;
      let fs = 13;
      if (scoInterval > 1200 && Math.random() < 0.001){
        d.isSCO = true; d.scoTimer = 0; scoInterval = 0;
      }
      if (d.isSCO){
        ch = 'SCO'; alpha = 0.9; fs = 15;
        x.shadowColor = '#00e85a'; x.shadowBlur = 12;
        d.scoTimer++;
        if (d.scoTimer > 40) d.isSCO = false;
      } else {
        x.shadowBlur = 0;
      }
      const colX = i * 18;
      const dist = Math.abs(colX - mouseX);
      let offsetX = 0;
      if (dist < 150){
        const force = (150 - dist) / 150;
        offsetX = colX < mouseX ? -force * 10 : force * 10;
      }
      x.globalAlpha = alpha;
      x.fillStyle = '#00e85a';
      x.font = fs + 'px JetBrains Mono,monospace';
      x.fillText(ch, colX + offsetX, d.y * 18);
      x.globalAlpha = 1;
      x.shadowBlur = 0;
      if (d.y * 18 > c.height && Math.random() > 0.975) d.y = 0;
      d.y += d.isSCO ? 0.55 : 0.3;
    }
  }
  setInterval(draw, 55);
})();


// ─── MARKET WIDGET (page-level, only active on non-home pages) ───
let marketPrices=[];
async function loadMarket(){
  // On home page, homeContent's loadTickers handles it
  const el=document.getElementById('market-list');
  if(!el)return;
  if(typeof tickers!=='undefined'&&tickers.length>0)return;
  try{
    const res=await fetch('/api/tickers');
    marketPrices=await res.json()||[];
    renderPageMarket();
  }catch(e){}
}
function renderPageMarket(){
  const el=document.getElementById('market-list');
  if(!el)return;
  el.innerHTML=marketPrices.map(p=>{
    const chg=p.change||0;
    const cls=chg>=0?'pos':'neg';
    const sign=chg>=0?'+':'';
    const price=p.price>=1?p.price.toLocaleString(I18N['common.locale'],{minimumFractionDigits:2,maximumFractionDigits:2}):p.price.toFixed(6);
    return '<div class="ml-item">'+
      '<img class="ml-icon" src="'+p.logo+'" alt="'+p.symbol+'" onerror="this.style.display=\'none\'">'+
      '<div class="ml-name">'+p.name+' <span class="ml-sym">'+p.symbol+'</span></div>'+
      '<div class="ml-price">€'+price+'</div>'+
      '<div class="ml-chg '+cls+'">'+sign+chg.toFixed(2)+'%</div>'+
      '</div>';
  }).join('');
}

// ─── ACTIVITY FEED ───
async function loadActivity(){
  const el=document.getElementById('activity-feed');
  if(!el)return;
  try{
    const res=await fetch('/api/activity');
    const entries=await res.json()||[];
    if(!entries.length){el.innerHTML='<div class="af-loading">'+I18N['common.no_activity']+'</div>';return;}
    el.innerHTML=entries.map(e=>{
      return '<div class="af-item"><div class="af-dot"></div><span>'+escHtml(e.text||'')+'</span><span class="af-time">'+escHtml(e.time||'')+'</span></div>';
    }).join('');
  }catch(e){}
}
setInterval(loadActivity,30000);

// ─── LEADERBOARD WIDGET ───
async function loadLeaderboard(){
  const el=document.getElementById('leaderboard-widget');
  if(!el)return;
  try{
    const res=await fetch('/api/leaderboard');
    const entries=await res.json()||[];
    if(!entries.length){el.innerHTML='<div class="af-loading">'+I18N['common.no_miners']+'</div>';return;}
    el.innerHTML=entries.slice(0,8).map((e,i)=>{
      const rankClass=i===0?'r1':i===1?'r2':i===2?'r3':'';
      const medal=i===0?'#1':i===1?'#2':i===2?'#3':'#'+(i+1);
      return '<div class="lb-item">'+
        '<div class="lb-rank '+rankClass+'">'+medal+'</div>'+
        '<div class="lb-name" onclick="openProfileModal(\''+( e.pseudo||'')+'\')">@'+(e.pseudo||e.address.substring(0,12)+'...')+'</div>'+
        '<div class="lb-blocks">'+e.blocks_mined+' '+I18N['common.blocks']+'</div>'+
        '</div>';
    }).join('');
  }catch(e){}
}

// ─── PUBLISH POST ───
function openPublishModal(){
  if(!IS_LOGGED_IN){window.location='/wallet';return;}
  const overlay=document.getElementById('publish-modal-overlay');
  if(overlay)overlay.classList.remove('hidden');
  const ta=document.getElementById('publish-content');
  if(ta){ta.value='';ta.focus();}
  updatePublishCount();
}
let publishImgFile=null;
function closePublishModal(){
  const overlay=document.getElementById('publish-modal-overlay');
  if(overlay)overlay.classList.add('hidden');
  publishImgFile=null;
  const imgPrev=document.getElementById('publish-img-preview');
  if(imgPrev){imgPrev.innerHTML='';imgPrev.classList.add('hidden');}
  const emojiPicker=document.getElementById('publish-emoji-picker');
  if(emojiPicker)emojiPicker.classList.add('hidden');
  const imgInput=document.getElementById('publish-img-input');
  if(imgInput)imgInput.value='';
}
function updatePublishCount(){
  const ta=document.getElementById('publish-content');
  const cnt=document.getElementById('publish-count');
  if(ta&&cnt)cnt.textContent=ta.value.length;
}
function onPublishImgChange(input){
  if(!input.files||!input.files[0])return;
  publishImgFile=input.files[0];
  const reader=new FileReader();
  reader.onload=function(e){
    const imgPrev=document.getElementById('publish-img-preview');
    if(imgPrev){imgPrev.innerHTML='<img src="'+e.target.result+'" alt="preview"><button style="position:absolute;top:4px;right:4px;background:#ff4444;border:none;color:#fff;border-radius:50%;width:20px;height:20px;cursor:pointer;font-size:10px" onclick="clearPublishImg()">✕</button>';imgPrev.style.position='relative';imgPrev.classList.remove('hidden');}
  };
  reader.readAsDataURL(publishImgFile);
}
function clearPublishImg(){
  publishImgFile=null;
  const imgPrev=document.getElementById('publish-img-preview');
  if(imgPrev){imgPrev.innerHTML='';imgPrev.classList.add('hidden');}
  const imgInput=document.getElementById('publish-img-input');
  if(imgInput)imgInput.value='';
}
function togglePublishEmoji(){
  const p=document.getElementById('publish-emoji-picker');
  if(!p)return;
  if(p.classList.contains('hidden')){
    if(typeof buildEmojiPicker==='function')buildEmojiPicker('publish-emoji-picker',function(e){ insertPublishEmoji(e); });
    p.classList.remove('hidden');
  } else { p.classList.add('hidden'); }
}
function insertPublishEmoji(e){
  const ta=document.getElementById('publish-content');
  if(!ta)return;
  const s=ta.selectionStart,end=ta.selectionEnd;
  ta.value=ta.value.slice(0,s)+e+ta.value.slice(end);
  ta.selectionStart=ta.selectionEnd=s+e.length;
  ta.focus();
  updatePublishCount();
  document.getElementById('publish-emoji-picker')?.classList.add('hidden');
}
async function submitPost(){
  const ta=document.getElementById('publish-content');
  const text=ta?ta.value.trim():'';
  if(!text&&!publishImgFile)return;
  try{
    const fd=new FormData();
    fd.append('content',text);
    if(publishImgFile)fd.append('image',publishImgFile);
    const res=await fetch('/api/posts',{method:'POST',body:fd});
    if(res.ok){
      closePublishModal();
      if(typeof loadPosts==='function')loadPosts();
    }
  }catch(e){}
}
// ─── IMAGE LIGHTBOX ───
function openImageLightbox(src){
  const lb=document.getElementById('img-lightbox');
  const img=document.getElementById('img-lightbox-img');
  if(!lb||!img)return;
  img.src=src;
  lb.classList.add('open');
  document.addEventListener('keydown',closeLightboxOnEsc,{once:true});
}
function closeImageLightbox(){
  const lb=document.getElementById('img-lightbox');
  if(lb)lb.classList.remove('open');
}
function closeLightboxOnEsc(e){if(e.key==='Escape')closeImageLightbox();}

// ─── PROFILE MODAL ───
async function openProfileModal(pseudo){
  if(!pseudo)return;
  const modal=document.getElementById('profile-modal');
  const content=document.getElementById('profile-modal-content');
  if(!modal||!content)return;
  content.innerHTML='<div style="text-align:center;padding:2rem;color:var(--muted)">'+I18N['common.loading']+'</div>';
  modal.classList.remove('hidden');
  try{
    const res=await fetch('/api/user/profile/'+encodeURIComponent(pseudo));
    const u=await res.json();
    if(u.error){content.innerHTML='<p>'+I18N['common.user_not_found']+'</p>';return;}
    content.innerHTML=
      '<div class="pmo-avatar">'+u.avatar_svg+'</div>'+
      '<div class="pmo-pseudo">@'+u.pseudo+'</div>'+
      '<div class="pmo-since">'+I18N['common.member_since']+' '+u.days_active+' '+I18N['common.days']+'</div>'+
      '<div class="pmo-stats">'+
        '<div class="pmo-stat"><div class="pmo-stat-val">'+u.blocks_mined+'</div><div class="pmo-stat-label">'+I18N['common.blocks_mined']+'</div></div>'+
        '<div class="pmo-stat"><div class="pmo-stat-val">'+u.balance+'</div><div class="pmo-stat-label">'+I18N['common.sco_earned']+'</div></div>'+
      '</div>'+
      '<div class="pmo-badges">'+(u.badges&&u.badges.length?u.badges.map(b=>'<span class="badge-item">'+b+'</span>').join(''):'')+'</div>';
  }catch(e){content.innerHTML='<p>'+I18N['common.error_loading']+'</p>';}
}
function closeProfileModal(){
  const modal=document.getElementById('profile-modal');
  if(modal)modal.classList.add('hidden');
}

function escapeHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}

// ─── INIT ───
document.addEventListener('DOMContentLoaded',()=>{
  loadActivity();
  loadLeaderboard();
  loadMarket();
  if(IS_LOGGED_IN){
    setTimeout(updateDMBadge, 1000);
    setInterval(updateDMBadge, 30000);
    // Hook WS for DM notifications if on home page
    const origHandleChatWS = typeof handleChatWS !== 'undefined' ? handleChatWS : null;
    if(origHandleChatWS){
      window.handleChatWS = function(msg){
        if(msg.type==='dm_notification'){ handleDMNotification(msg); return; }
        if(msg.type==='group_message'){ handleGroupNotification(msg); return; }
        if(msg.type==='group_invite'){ handleGroupInvite(msg); return; }
        origHandleChatWS(msg);
      };
    }
    // Init DM emoji picker if buildEmojiPicker available (home page)
    if(typeof buildEmojiPicker==='function'){
      buildEmojiPicker('dm-emoji-picker', emoji=>{
        const inp=document.getElementById('dm-input');
        if(inp){inp.value+=emoji;inp.focus();}
        document.getElementById('dm-emoji-picker').classList.add('hidden');
      });
    }
    if(typeof loadDMConversations==='function') loadDMConversations();
    // Restore DM state from sessionStorage
    const dmWasOpen = sessionStorage.getItem('dm_open');
    const dmConvStr = sessionStorage.getItem('dm_conv');
    if(dmWasOpen==='true'){
      const w=document.getElementById('dm-window');
      if(w){
        dmWindowOpen=true;
        w.classList.remove('hidden');
        dmConvsInterval=setInterval(loadDMConversations,10000);
        if(dmConvStr){
          try{
            const conv=JSON.parse(dmConvStr);
            if(conv.userId) setTimeout(()=>openDMConversation(conv.userId,conv.pseudo,conv.avatar||''),200);
          }catch(e){}
        } else {
          loadDMConversations();
        }
      }
    }
  }
});
// Publish modal keyboard shortcut
document.addEventListener('keydown',e=>{if(e.key==='Escape'){closePublishModal();if(dmWindowOpen){toggleDMWindow();}}});

// ─── DM SYSTEM ───
let dmWindowOpen=false;
let dmCurrentUserID=null;
let dmCurrentPseudo=null;
let dmCurrentAvatar=null;
let dmCurrentView='convs';
let dmGifUrl=null;
let dmGifSearchTimeout=null;
let dmConvsInterval=null;
let dmMsgsInterval=null;

function toggleDMWindow(){
  const w=document.getElementById('dm-window');
  if(!w) return;
  dmWindowOpen=!dmWindowOpen;
  w.classList.toggle('hidden',!dmWindowOpen);
  if(dmWindowOpen){
    sessionStorage.setItem('dm_open','true');
    if(dmCurrentView==='convs') loadDMConversations();
    const inp=document.getElementById('dm-search');
    if(inp) inp.value='';
    document.getElementById('dm-search-results')?.classList.add('hidden');
    dmConvsInterval=setInterval(loadDMConversations,10000);
  } else {
    sessionStorage.removeItem('dm_open');
    sessionStorage.removeItem('dm_conv');
    clearInterval(dmConvsInterval);
    clearInterval(dmMsgsInterval);
    clearInterval(dmGroupMsgsInterval);
  }
}

function showDMView(view){
  dmCurrentView=view;
  ['convs','conv','settings','create-group','group','group-info'].forEach(v=>{
    const el=document.getElementById('dm-view-'+v);
    if(el) el.classList.toggle('hidden', v!==view);
  });
}

function dmRelTime(iso){
  const sec=Math.floor((Date.now()-new Date(iso))/1000);
  if(sec<60) return I18N['common.instantly']||'now';
  if(sec<3600) return Math.floor(sec/60)+(I18N['common.ago_min']||'m');
  if(sec<86400) return Math.floor(sec/3600)+(I18N['common.ago_h']||'h');
  return Math.floor(sec/86400)+(I18N['common.ago_d']||'d');
}

let dmGroupsCache={};

async function loadDMConversations(){
  if(!IS_LOGGED_IN||dmCurrentView!=='convs') return;
  try{
    const [dmRes,grpRes]=await Promise.all([
      fetch('/api/dm/conversations'),
      fetch('/api/dm/group/list')
    ]);
    const dmConvs=(await dmRes.json())||[];
    const groups=(await grpRes.json())||[];
    groups.forEach(g=>{dmGroupsCache[g.id]=g;});
    const el=document.getElementById('dm-conv-list');
    if(!el) return;
    const dmItems=dmConvs.map(c=>({...c,_type:'dm',_sortTime:c.last_message_at}));
    const grpItems=groups.map(g=>({...g,_type:'group',_sortTime:g.last_message_at||g.created_at}));
    const all=[...dmItems,...grpItems].sort((a,b)=>new Date(b._sortTime)-new Date(a._sortTime));
    if(!all.length){
      el.innerHTML='<div class="dm-empty">'+(I18N['dm.no_conversations']||'No conversations yet')+'</div>';
      return;
    }
    el.innerHTML=all.map(item=>{
      const hasUnread=item.unread_count>0;
      const unreadClass=hasUnread?' unread':'';
      const unreadHtml=hasUnread?'<span class="dm-conv-unread-count">'+item.unread_count+'</span>':'';
      const dotHtml=hasUnread?'<span class="dm-unread-dot"></span>':'';
      if(item._type==='dm'){
        const avatarHtml=item.other_avatar?'<span class="dm-conv-avatar-wrap" style="display:inline-flex;width:36px;height:36px">'+item.other_avatar+'</span>':'<span class="dm-conv-avatar-wrap"></span>';
        return '<div class="dm-conv-item'+unreadClass+'" data-type="dm" data-user-id="'+escHtmlDM(item.other_user_id)+'" data-pseudo="'+escHtmlDM(item.other_pseudo)+'">'+
          avatarHtml+
          '<div class="dm-conv-info"><span class="dm-conv-pseudo">@'+escHtmlDM(item.other_pseudo)+'</span><span class="dm-conv-preview">'+escHtmlDM(item.last_message)+'</span></div>'+
          '<div class="dm-conv-meta"><span class="dm-conv-time">'+dmRelTime(item.last_message_at)+'</span>'+unreadHtml+'</div>'+
          dotHtml+'</div>';
      } else {
        const initials=(item.name||'?').split(' ').map(w=>w[0]||'').join('').toUpperCase().slice(0,2)||'G';
        const membersPreview=(item.member_infos||[]).slice(0,3).map(m=>'@'+escHtmlDM(m.pseudo)).join(', ');
        return '<div class="dm-conv-item'+unreadClass+'" data-type="group" data-group-id="'+escHtmlDM(item.id)+'">'+
          '<div class="dm-conv-avatar-outer"><div class="group-avatar-placeholder">'+initials+'</div><span class="dm-group-indicator">👥</span></div>'+
          '<div class="dm-conv-info"><span class="dm-conv-pseudo" style="color:#d4f5e0">'+escHtmlDM(item.name)+'</span><span class="dm-conv-preview">'+escHtmlDM(item.last_message||membersPreview)+'</span></div>'+
          '<div class="dm-conv-meta"><span class="dm-conv-time">'+dmRelTime(item.last_message_at||item.created_at)+'</span>'+unreadHtml+'</div>'+
          dotHtml+'</div>';
      }
    }).join('');
    el.querySelectorAll('.dm-conv-item[data-type="dm"]').forEach(function(item){
      item.addEventListener('click',function(){
        var uid=item.dataset.userId;
        var ps=item.dataset.pseudo;
        var avWrap=item.querySelector('.dm-conv-avatar-wrap');
        var av=avWrap?avWrap.innerHTML:'';
        openDMConversation(uid,ps,av);
      });
    });
    el.querySelectorAll('.dm-conv-item[data-type="group"]').forEach(function(item){
      item.addEventListener('click',function(){
        var gid=item.dataset.groupId;
        var g=dmGroupsCache[gid];
        if(g) openGroupConversation(g.id,g.name,g.member_infos,g.is_creator,g.member_count);
      });
    });
  }catch(e){}
}

async function openDMConversation(userID, pseudo, avatar){
  dmCurrentUserID=userID;
  dmCurrentPseudo=pseudo;
  sessionStorage.setItem('dm_open','true');
  sessionStorage.setItem('dm_conv',JSON.stringify({userId:userID,pseudo:pseudo,avatar:avatar||''}));
  dmCurrentAvatar=avatar;
  document.getElementById('dm-conv-header-pseudo').textContent='@'+pseudo;
  const avatarEl=document.getElementById('dm-conv-header-avatar');
  if(avatarEl) avatarEl.innerHTML=avatar||'';
  showDMView('conv');
  clearInterval(dmMsgsInterval);
  await loadDMMessages(userID);
  // Mark as read
  try{ await fetch('/api/dm/read',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({with:userID})}); }catch(e){}
  updateDMBadge();
  dmMsgsInterval=setInterval(()=>loadDMMessages(userID),5000);
}

function closeDMConversation(){
  clearInterval(dmMsgsInterval);
  dmCurrentUserID=null;
  showDMView('convs');
  loadDMConversations();
}

async function loadDMMessages(userID){
  try{
    const res=await fetch('/api/dm/messages?with='+encodeURIComponent(userID));
    const msgs=await res.json()||[];
    renderDMMessages(msgs);
  }catch(e){}
}

function renderDMMessages(msgs){
  const el=document.getElementById('dm-messages');
  if(!el) return;
  const wasAtBottom=el.scrollHeight-el.scrollTop-el.clientHeight<50;
  if(!msgs.length){
    el.innerHTML='<div class="dm-empty">'+(I18N['dm.no_messages']||'No messages yet. Say hello!')+'</div>';
    return;
  }
  el.innerHTML=msgs.map(m=>{
    const isMine=m.from_id===CURRENT_USER_ID;
    const cls=isMine?'dm-msg-mine':'dm-msg-their';
    const ts=new Date(m.created_at).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
    let statusHtml='';
    if(isMine){
      if(m.read && m.read_at){
        const readTs=new Date(m.read_at).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
        statusHtml='<span class="dm-msg-status" style="color:#00e85a">✓✓ '+readTs+'</span>';
      } else {
        statusHtml='<span class="dm-msg-status">✓</span>';
      }
    }
    const gifHtml=m.gif_url?'<img src="'+escHtmlDM(m.gif_url)+'" class="dm-gif" loading="lazy">':'';
    const delBtn=isMine?'<button class="dm-delete-btn" onclick="deleteDMMessage(\''+m.id+'\')">🗑</button>':'';
    return '<div class="dm-msg '+cls+'" data-id="'+m.id+'">'+
      delBtn+
      '<div class="dm-msg-content">'+renderTextLinks(m.content)+'</div>'+
      gifHtml+
      '<div class="dm-msg-meta"><span class="dm-msg-time">'+ts+'</span>'+statusHtml+'</div>'+
      '</div>';
  }).join('');
  if(wasAtBottom) el.scrollTop=el.scrollHeight;
}

async function sendDM(){
  if(!IS_LOGGED_IN||!dmCurrentUserID) return;
  const inp=document.getElementById('dm-input');
  const text=inp?inp.value.trim():'';
  const gif=dmGifUrl||'';
  if(!text&&!gif) return;
  try{
    const res=await fetch('/api/dm/send',{
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body:JSON.stringify({to_id:dmCurrentUserID,content:text,gif_url:gif})
    });
    if(res.ok){
      if(inp) inp.value='';
      dmGifUrl=null;
      document.getElementById('dm-gif-picker')?.classList.add('hidden');
      await loadDMMessages(dmCurrentUserID);
      setTimeout(()=>loadDMConversations(), 300);
    } else {
      const d=await res.json();
      if(d.error) alert(d.error);
    }
  }catch(e){}
}

async function deleteDMMessage(msgId){
  if(!confirm(I18N['dm.confirm_delete']||'Delete this message?')) return;
  try{
    await fetch('/api/dm/message?id='+encodeURIComponent(msgId),{method:'DELETE'});
    if(dmCurrentUserID) loadDMMessages(dmCurrentUserID);
  }catch(e){}
}

let dmSearchTimeout=null;
async function searchDMUser(q){
  clearTimeout(dmSearchTimeout);
  const results=document.getElementById('dm-search-results');
  const convList=document.getElementById('dm-conv-list');
  if(!q){
    results?.classList.add('hidden');
    convList?.classList.remove('hidden');
    return;
  }
  convList?.classList.add('hidden');
  results?.classList.remove('hidden');
  dmSearchTimeout=setTimeout(async()=>{
    try{
      const res=await fetch('/api/users/search?q='+encodeURIComponent(q));
      const users=await res.json()||[];
      if(!results) return;
      if(!users.length){
        results.innerHTML='<div class="dm-empty">'+(I18N['dm.no_users']||'No users found')+'</div>';
        return;
      }
      results.innerHTML=users.map(u=>{
        const avatarHtml=u.avatar?'<span class="dm-conv-avatar-wrap" style="display:inline-flex;width:36px;height:36px">'+u.avatar+'</span>':'<span class="dm-conv-avatar-wrap"></span>';
        return '<div class="dm-conv-item" data-user-id="'+escHtmlDM(u.id)+'" data-pseudo="'+escHtmlDM(u.pseudo)+'">'+
          avatarHtml+
          '<div class="dm-conv-info"><span class="dm-conv-pseudo">@'+escHtmlDM(u.pseudo)+'</span></div>'+
          '</div>';
      }).join('');
      results.querySelectorAll('.dm-conv-item').forEach(function(item){
        item.addEventListener('click',function(){
          var uid=item.dataset.userId;
          var ps=item.dataset.pseudo;
          var avWrap=item.querySelector('.dm-conv-avatar-wrap');
          var av=avWrap?avWrap.innerHTML:'';
          openDMConversation(uid,ps,av);
        });
      });
    }catch(e){}
  },400);
}

async function openDMSettings(){
  showDMView('settings');
  try{
    const res=await fetch('/api/dm/settings');
    const s=await res.json();
    const policy=s.policy||'everyone';
    document.querySelectorAll('input[name="dm-policy"]').forEach(r=>{r.checked=r.value===policy;});
  }catch(e){}
}

function closeDMSettings(){ showDMView('convs'); loadDMConversations(); }

async function saveDMSettings(){
  const selected=document.querySelector('input[name="dm-policy"]:checked');
  if(!selected) return;
  try{
    await fetch('/api/dm/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({policy:selected.value})});
    closeDMSettings();
  }catch(e){}
}

function updateDMBadge(){
  if(!IS_LOGGED_IN) return;
  fetch('/api/dm/unread')
    .then(function(r){return r.json();})
    .then(function(d){
      var badge=document.getElementById('dm-badge');
      if(!badge) return;
      var count=d.unread||0;
      if(count>0){
        badge.textContent=count>99?'99+':String(count);
        badge.style.display='flex';
      } else {
        badge.style.display='none';
      }
    }).catch(function(){});
}

function handleDMNotification(msg){
  updateDMBadge();
  // If window open on that conversation, reload
  if(dmWindowOpen&&dmCurrentView==='conv'&&dmCurrentUserID===msg.from_id){
    loadDMMessages(msg.from_id);
    try{fetch('/api/dm/read',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({with:msg.from_id})});}catch(e){}
    return;
  }
  if(dmWindowOpen&&dmCurrentView==='convs') loadDMConversations();
  showDMToast(msg);
}

function showDMToast(msg){
  const container=document.getElementById('dm-toast-container');
  if(!container) return;
  const toast=document.createElement('div');
  toast.className='dm-toast';
  const avatarHtml=msg.from_avatar?'<span class="dm-toast-avatar">'+msg.from_avatar+'</span>':'<span class="dm-toast-avatar"></span>';
  toast.innerHTML=avatarHtml+'<div class="dm-toast-body"><div class="dm-toast-pseudo">@'+escHtmlDM(msg.from_pseudo)+'</div><div class="dm-toast-preview">'+escHtmlDM(msg.preview||'')+'</div></div>';
  toast.onclick=()=>{
    if(!dmWindowOpen) toggleDMWindow();
    openDMConversation(msg.from_id,msg.from_pseudo,msg.from_avatar||'');
    toast.remove();
  };
  container.appendChild(toast);
  setTimeout(()=>toast.remove(),4000);
}

function toggleDMEmoji(e){
  e.stopPropagation();
  const picker=document.getElementById('dm-emoji-picker');
  if(!picker) return;
  if(typeof buildEmojiPicker==='function'&&picker.innerHTML===''){
    buildEmojiPicker('dm-emoji-picker',emoji=>{
      const inp=document.getElementById('dm-input');
      if(inp){inp.value+=emoji;inp.focus();}
      picker.classList.add('hidden');
    });
  }
  const isNowVisible=!picker.classList.contains('hidden');
  if(isNowVisible){
    picker.classList.add('hidden');
  } else {
    // Position above the emoji button so it doesn't go off-screen
    const btn=e.currentTarget;
    const btnRect=btn.getBoundingClientRect();
    const pickerH=300;
    const pickerW=290;
    picker.style.position='fixed';
    picker.style.zIndex='10001';
    picker.style.bottom='';
    picker.style.right='';
    let top=btnRect.top-pickerH-6;
    if(top<8) top=Math.min(8,btnRect.bottom+6);
    let left=btnRect.left;
    if(left+pickerW>window.innerWidth-8) left=window.innerWidth-pickerW-8;
    if(left<8) left=8;
    picker.style.top=top+'px';
    picker.style.left=left+'px';
    picker.classList.remove('hidden');
  }
  document.getElementById('dm-gif-picker')?.classList.add('hidden');
}

function toggleDMGif(e){
  e.stopPropagation();
  const p=document.getElementById('dm-gif-picker');
  if(!p) return;
  const isHidden=p.classList.toggle('hidden');
  document.getElementById('dm-emoji-picker')?.classList.add('hidden');
  if(!isHidden){
    const grid=document.getElementById('dm-gif-grid');
    if(grid&&grid.innerHTML==='') loadDMGifs('trending');
    setTimeout(()=>{const inp=document.getElementById('dm-gif-search');if(inp)inp.focus();},50);
  }
}

async function loadDMGifs(q){
  const grid=document.getElementById('dm-gif-grid');
  if(!grid) return;
  grid.innerHTML='<div style="color:var(--muted);font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">Loading...</div>';
  try{
    const res=await fetch('/api/chat/gif?q='+encodeURIComponent(q));
    if(!res.ok) throw new Error();
    const data=await res.json();
    if(!Array.isArray(data)||!data.length){
      grid.innerHTML='<div style="color:var(--muted);font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">No GIFs found</div>';
      return;
    }
    grid.innerHTML=data.map(g=>'<img src="'+escHtmlDM(g.preview||g.url)+'" data-gif="'+escHtmlDM(g.url)+'" class="gif-item" loading="lazy" style="width:100%;height:80px;object-fit:cover;cursor:pointer;border-radius:4px;border:2px solid transparent;" onmouseover="this.style.borderColor=\'#00e85a\'" onmouseout="this.style.borderColor=\'transparent\'" onclick="selectDMGif(this.dataset.gif)">').join('');
  }catch(err){
    grid.innerHTML='<div style="color:#ff6464;font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">Error loading GIFs</div>';
  }
}

function searchDMGifs(q){
  clearTimeout(dmGifSearchTimeout);
  const grid=document.getElementById('dm-gif-grid');
  if(!q){if(grid)grid.innerHTML='';return;}
  if(grid)grid.innerHTML='<div style="color:var(--muted);font-size:0.8rem;padding:0.5rem;text-align:center;grid-column:1/-1">Searching...</div>';
  dmGifSearchTimeout=setTimeout(()=>loadDMGifs(q),600);
}

function selectDMGif(url){
  dmGifUrl=url;
  document.getElementById('dm-gif-picker')?.classList.add('hidden');
  sendDM();
}

function escHtmlDM(t){
  if(!t) return '';
  return String(t).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

function renderTextLinks(text){
  if(!text)return '';
  return linkify(String(text));
}

// DM input enter key
document.addEventListener('DOMContentLoaded',()=>{
  const inp=document.getElementById('dm-input');
  if(inp) inp.addEventListener('keydown',e=>{if(e.key==='Enter'&&!e.shiftKey){e.preventDefault();sendDM();}});
  const ginp=document.getElementById('dm-group-input');
  if(ginp) ginp.addEventListener('keydown',e=>{if(e.key==='Enter'&&!e.shiftKey){e.preventDefault();sendGroupMessage();}});
  // Close DM pickers on outside click
  document.addEventListener('click',()=>{
    document.getElementById('dm-emoji-picker')?.classList.add('hidden');
    document.getElementById('dm-gif-picker')?.classList.add('hidden');
  });
});

// ─── GROUP DM SYSTEM ───
let dmCurrentGroupID=null;
let dmCurrentGroupName=null;
let dmCurrentGroupInfo=null;
let dmGroupMsgsInterval=null;
let dmGroupGifUrl=null;
let groupSelectedMembers=[];
let groupMemberSearchTimeout=null;
let groupAddMemberSearchTimeout=null;

function openCreateGroup(){
  groupSelectedMembers=[];
  showDMView('create-group');
  const nInp=document.getElementById('group-name-input');
  if(nInp) nInp.value='';
  const mInp=document.getElementById('group-member-search');
  if(mInp) mInp.value='';
  const res=document.getElementById('group-member-results');
  if(res) res.innerHTML='';
  renderGroupSelectedMembers();
}

function searchGroupMember(q){
  clearTimeout(groupMemberSearchTimeout);
  const res=document.getElementById('group-member-results');
  if(!q){if(res)res.innerHTML='';return;}
  groupMemberSearchTimeout=setTimeout(async()=>{
    try{
      const r=await fetch('/api/users/search?q='+encodeURIComponent(q));
      const users=await r.json()||[];
      if(!res) return;
      if(!users.length){res.innerHTML='<div class="dm-empty">'+(I18N['dm.no_users']||'No users found')+'</div>';return;}
      res.innerHTML=users.map(u=>{
        const added=groupSelectedMembers.some(m=>m.id===u.id);
        return '<div class="dm-conv-item'+(added?' dm-conv-item--added':'')+'" data-user-id="'+escHtmlDM(u.id)+'" data-pseudo="'+escHtmlDM(u.pseudo)+'">'+
          '<span class="dm-conv-avatar-wrap" style="display:inline-flex;width:28px;height:28px">'+u.avatar+'</span>'+
          '<span class="dm-conv-pseudo">@'+escHtmlDM(u.pseudo)+'</span>'+
          (added?'<span style="color:#4a6a4a;font-size:0.7rem;margin-left:auto">&check;</span>':'')+
          '</div>';
      }).join('');
      res.querySelectorAll('.dm-conv-item:not(.dm-conv-item--added)').forEach(function(item){
        item.addEventListener('click',function(){
          var uid=item.dataset.userId;
          var ps=item.dataset.pseudo;
          var avWrap=item.querySelector('.dm-conv-avatar-wrap');
          var av=avWrap?avWrap.innerHTML:'';
          addToGroupSelection(uid,ps,av);
        });
      });
    }catch(e){}
  },400);
}

function addToGroupSelection(id,pseudo,avatar){
  if(groupSelectedMembers.some(m=>m.id===id)) return;
  groupSelectedMembers.push({id,pseudo,avatar});
  renderGroupSelectedMembers();
  const q=document.getElementById('group-member-search')?.value;
  if(q) searchGroupMember(q);
}

function removeFromGroupSelection(id){
  groupSelectedMembers=groupSelectedMembers.filter(m=>m.id!==id);
  renderGroupSelectedMembers();
  const q=document.getElementById('group-member-search')?.value;
  if(q) searchGroupMember(q);
}

function renderGroupSelectedMembers(){
  const el=document.getElementById('group-selected-members');
  if(!el) return;
  el.innerHTML='';
  groupSelectedMembers.forEach(function(m){
    const chip=document.createElement('div');
    chip.className='dm-member-chip';
    chip.innerHTML='<span style="display:inline-flex;width:20px;height:20px;border-radius:50%;overflow:hidden;flex-shrink:0">'+m.avatar+'</span>'+
      '<span>@'+escHtmlDM(m.pseudo)+'</span>';
    const btn=document.createElement('button');
    btn.textContent='✕';
    btn.onclick=function(){removeFromGroupSelection(m.id);};
    chip.appendChild(btn);
    el.appendChild(chip);
  });
}

async function createGroup(){
  const name=document.getElementById('group-name-input')?.value.trim();
  if(!name){alert(I18N['dm.group_name_required']||'Enter a group name');return;}
  if(groupSelectedMembers.length<2){alert(I18N['dm.group_min_members']||'Add at least 2 members');return;}
  try{
    const res=await fetch('/api/dm/group/create',{
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body:JSON.stringify({name,member_ids:groupSelectedMembers.map(m=>m.id)})
    });
    const d=await res.json();
    if(d.success){
      showDMView('convs');
      setTimeout(()=>loadDMConversations(),300);
    } else {
      alert(d.error||'Error creating group');
    }
  }catch(e){}
}

async function openGroupConversation(groupId,groupName,memberInfos,isCreator,memberCount){
  clearInterval(dmGroupMsgsInterval);
  dmCurrentGroupID=groupId;
  dmCurrentGroupName=groupName;
  dmCurrentGroupInfo={id:groupId,name:groupName,member_infos:memberInfos||[],is_creator:isCreator,member_count:memberCount};
  const headerName=document.getElementById('dm-group-header-name');
  if(headerName) headerName.textContent=groupName;
  const memberCountEl=document.getElementById('dm-group-member-count');
  if(memberCountEl) memberCountEl.textContent=(memberCount||0)+' '+(I18N['dm.group_members']||'members');
  const avatarEl=document.getElementById('dm-group-header-avatar');
  if(avatarEl){
    const initials=(groupName||'?').split(' ').map(function(w){return w[0]||'';}).join('').toUpperCase().slice(0,2)||'G';
    avatarEl.textContent=initials;
  }
  showDMView('group');
  await loadGroupMessages(groupId);
  try{await fetch('/api/dm/group/read',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({group_id:groupId})});}catch(e){}
  updateDMBadge();
  dmGroupMsgsInterval=setInterval(function(){loadGroupMessages(groupId);},5000);
}

async function loadGroupMessages(groupId){
  try{
    const res=await fetch('/api/dm/group/messages?group_id='+encodeURIComponent(groupId));
    const msgs=await res.json()||[];
    renderGroupMessages(msgs);
  }catch(e){}
}

function renderGroupMessages(msgs){
  const el=document.getElementById('dm-group-messages');
  if(!el) return;
  const wasAtBottom=el.scrollHeight-el.scrollTop-el.clientHeight<50;
  if(!msgs.length){
    el.innerHTML='<div class="dm-empty">'+(I18N['dm.no_messages']||'No messages yet. Say hello!')+'</div>';
    return;
  }
  const avatarMap={};
  if(dmCurrentGroupInfo&&dmCurrentGroupInfo.member_infos){
    dmCurrentGroupInfo.member_infos.forEach(function(m){avatarMap[m.user_id]={avatar:m.avatar,pseudo:m.pseudo};});
  }
  el.innerHTML=msgs.map(function(m){
    const isMine=m.from_id===CURRENT_USER_ID;
    const cls=isMine?'dm-group-msg-mine':'dm-group-msg-their';
    const ts=new Date(m.created_at).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
    const gifHtml=m.gif_url?'<img src="'+escHtmlDM(m.gif_url)+'" class="dm-gif" loading="lazy">':'';
    const readers=(m.read_by||[]).filter(function(uid){return uid!==m.from_id;});
    var readByHtml='';
    if(readers.length>0){
      const shown=readers.slice(0,5);
      const extra=readers.length>5?readers.length-5:0;
      readByHtml='<div class="dm-group-readby">'+
        shown.map(function(uid){
          const info=avatarMap[uid];
          if(!info) return '';
          return '<span class="dm-readby-avatar" title="@'+escHtmlDM(info.pseudo)+'">'+info.avatar+'</span>';
        }).join('')+
        (extra?'<span style="color:#4a6a4a;font-size:9px">+'+extra+'</span>':'')+
        '</div>';
    }
    const senderInfo=avatarMap[m.from_id];
    const senderAvatarHtml=senderInfo?'<span class="dm-group-sender-avatar">'+senderInfo.avatar+'</span>':'';
    return '<div class="dm-msg '+cls+'" data-id="'+m.id+'">'+
      (isMine?'':senderAvatarHtml)+
      '<div style="min-width:0;flex:1">'+
      (isMine?'':'<div class="dm-group-msg-from">@'+escHtmlDM(m.from_pseudo)+'</div>')+
      '<div class="dm-msg-content">'+renderTextLinks(m.content)+'</div>'+
      gifHtml+
      '<div class="dm-msg-meta"><span class="dm-msg-time">'+ts+'</span></div>'+
      readByHtml+
      '</div>'+
      (isMine?senderAvatarHtml:'')+
      '</div>';
  }).join('');
  if(wasAtBottom) el.scrollTop=el.scrollHeight;
}

async function sendGroupMessage(){
  if(!IS_LOGGED_IN||!dmCurrentGroupID) return;
  const inp=document.getElementById('dm-group-input');
  const text=inp?inp.value.trim():'';
  if(!text&&!dmGroupGifUrl) return;
  try{
    const res=await fetch('/api/dm/group/send',{
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body:JSON.stringify({group_id:dmCurrentGroupID,content:text,gif_url:dmGroupGifUrl||''})
    });
    if(res.ok){
      if(inp) inp.value='';
      dmGroupGifUrl=null;
      await loadGroupMessages(dmCurrentGroupID);
      setTimeout(()=>loadDMConversations(),300);
    }
  }catch(e){}
}

function closeGroupConversation(){
  clearInterval(dmGroupMsgsInterval);
  dmCurrentGroupID=null;
  dmCurrentGroupInfo=null;
  showDMView('convs');
  loadDMConversations();
}

function openGroupInfo(){
  showDMView('group-info');
  renderGroupInfo();
}

function renderGroupInfo(){
  if(!dmCurrentGroupInfo) return;
  const renameSection=document.getElementById('dm-group-rename-section');
  const renameInput=document.getElementById('dm-group-rename-input');
  if(renameInput) renameInput.value=dmCurrentGroupInfo.name;
  if(renameSection) renameSection.style.display=dmCurrentGroupInfo.is_creator?'':'none';
  const addSection=document.getElementById('dm-group-add-section');
  if(addSection) addSection.style.display=dmCurrentGroupInfo.is_creator?'':'none';
  const membersTitle=document.getElementById('dm-group-members-title');
  if(membersTitle) membersTitle.textContent=(dmCurrentGroupInfo.member_count||0)+' '+(I18N['dm.group_members']||'members');
  const list=document.getElementById('dm-group-members-list');
  if(!list) return;
  const members=dmCurrentGroupInfo.member_infos||[];
  list.innerHTML='';
  members.forEach(function(m){
    const isMe=m.user_id===CURRENT_USER_ID;
    const canRemove=dmCurrentGroupInfo.is_creator&&!isMe;
    const row=document.createElement('div');
    row.className='dm-group-member-row';
    row.innerHTML='<span class="dm-conv-avatar-wrap" style="width:30px;height:30px;display:inline-flex;flex-shrink:0">'+m.avatar+'</span>'+
      '<span class="dm-group-member-pseudo">@'+escHtmlDM(m.pseudo)+(isMe?' (you)':'')+'</span>';
    if(canRemove){
      const btn=document.createElement('button');
      btn.className='dm-group-member-remove';
      btn.textContent='✕';
      btn.onclick=function(){removeMemberFromGroup(m.user_id);};
      row.appendChild(btn);
    }
    list.appendChild(row);
  });
}

async function removeMemberFromGroup(userID){
  if(!confirm(I18N['dm.group_confirm_remove']||'Remove this member?')) return;
  try{
    await fetch('/api/dm/group/leave',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({group_id:dmCurrentGroupID,user_id:userID})});
    const grpRes=await fetch('/api/dm/group/list');
    const groups=await grpRes.json()||[];
    const updated=groups.find(g=>g.id===dmCurrentGroupID);
    if(updated){
      dmCurrentGroupInfo={...dmCurrentGroupInfo,...updated};
      renderGroupInfo();
    }
  }catch(e){}
}

async function renameGroup(){
  const inp=document.getElementById('dm-group-rename-input');
  if(!inp) return;
  const name=inp.value.trim();
  if(!name) return;
  try{
    const res=await fetch('/api/dm/group/rename',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({group_id:dmCurrentGroupID,name})});
    if(res.ok){
      dmCurrentGroupName=name;
      if(dmCurrentGroupInfo) dmCurrentGroupInfo.name=name;
      const headerName=document.getElementById('dm-group-header-name');
      if(headerName) headerName.textContent=name;
    }
  }catch(e){}
}

async function leaveGroup(){
  if(!confirm(I18N['dm.group_confirm_leave']||'Leave this group?')) return;
  try{
    await fetch('/api/dm/group/leave',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({group_id:dmCurrentGroupID})});
    closeGroupConversation();
  }catch(e){}
}

function searchGroupAddMember(q){
  clearTimeout(groupAddMemberSearchTimeout);
  const res=document.getElementById('dm-group-add-results');
  if(!q){if(res)res.innerHTML='';return;}
  groupAddMemberSearchTimeout=setTimeout(async()=>{
    try{
      const r=await fetch('/api/users/search?q='+encodeURIComponent(q));
      const users=await r.json()||[];
      if(!res) return;
      if(!users.length){res.innerHTML='<div class="dm-empty">'+(I18N['dm.no_users']||'No users found')+'</div>';return;}
      const existingIds=(dmCurrentGroupInfo?.member_infos||[]).map(m=>m.user_id);
      const filtered=users.filter(u=>!existingIds.includes(u.id));
      if(!filtered.length){res.innerHTML='<div class="dm-empty">Already members</div>';return;}
      res.innerHTML=filtered.map(u=>'<div class="dm-conv-item" data-user-id="'+escHtmlDM(u.id)+'" data-pseudo="'+escHtmlDM(u.pseudo)+'">'+
        '<span class="dm-conv-avatar-wrap" style="display:inline-flex;width:28px;height:28px">'+u.avatar+'</span>'+
        '<span class="dm-conv-pseudo">@'+escHtmlDM(u.pseudo)+'</span></div>').join('');
      res.querySelectorAll('.dm-conv-item').forEach(function(item){
        item.addEventListener('click',async function(){
          const uid=item.dataset.userId;
          try{
            const r2=await fetch('/api/dm/group/add-member',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({group_id:dmCurrentGroupID,user_id:uid})});
            if(r2.ok){
              const searchInp=document.getElementById('dm-group-add-search');
              if(searchInp) searchInp.value='';
              if(res) res.innerHTML='';
              const grpRes=await fetch('/api/dm/group/list');
              const gs=await grpRes.json()||[];
              const updated=gs.find(g=>g.id===dmCurrentGroupID);
              if(updated){dmCurrentGroupInfo={...dmCurrentGroupInfo,...updated};renderGroupInfo();}
            }
          }catch(e){}
        });
      });
    }catch(e){}
  },400);
}

function handleGroupNotification(msg){
  updateDMBadge();
  if(dmWindowOpen&&dmCurrentView==='group'&&dmCurrentGroupID===msg.group_id){
    loadGroupMessages(msg.group_id);
    try{fetch('/api/dm/group/read',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({group_id:msg.group_id})});}catch(e){}
    return;
  }
  if(dmWindowOpen&&dmCurrentView==='convs') loadDMConversations();
  showGroupToast(msg);
}

function handleGroupInvite(msg){
  updateDMBadge();
  if(dmWindowOpen&&dmCurrentView==='convs') setTimeout(()=>loadDMConversations(),500);
  showGroupToast({group_id:msg.group_id,group_name:msg.group_name,preview:'👥 '+escHtmlDM(msg.from_pseudo)+' '+(I18N['dm.group_invited_you']||'added you to')+' '+escHtmlDM(msg.group_name)});
}

function showGroupToast(msg){
  const container=document.getElementById('dm-toast-container');
  if(!container) return;
  const toast=document.createElement('div');
  toast.className='dm-toast';
  const initials=(msg.group_name||'G').split(' ').map(function(w){return w[0]||'';}).join('').toUpperCase().slice(0,2)||'G';
  toast.innerHTML='<div class="dm-toast-avatar" style="background:#0d2010;color:#00e85a;font-weight:700;font-size:0.7rem;display:flex;align-items:center;justify-content:center">'+escHtmlDM(initials)+'</div>'+
    '<div class="dm-toast-body"><div class="dm-toast-pseudo">👥 '+escHtmlDM(msg.group_name||'Group')+'</div><div class="dm-toast-preview">'+escHtmlDM(msg.preview||msg.from_pseudo||'')+'</div></div>';
  toast.onclick=function(){
    if(!dmWindowOpen) toggleDMWindow();
    if(msg.group_id){
      var g=dmGroupsCache[msg.group_id];
      if(g) openGroupConversation(g.id,g.name,g.member_infos,g.is_creator,g.member_count);
    }
    toast.remove();
  };
  container.appendChild(toast);
  setTimeout(function(){toast.remove();},4000);
}

// ── Expose all page-level onclick functions to window ──
window.openPublishModal       = openPublishModal;
window.closePublishModal      = closePublishModal;
window.updatePublishCount     = updatePublishCount;
window.onPublishImgChange     = onPublishImgChange;
window.clearPublishImg        = clearPublishImg;
window.togglePublishEmoji     = togglePublishEmoji;
window.insertPublishEmoji     = insertPublishEmoji;
window.submitPost             = submitPost;
// aliases used in onclick attrs
window.togglePostEmoji        = togglePublishEmoji;
window.previewPostImage       = onPublishImgChange;
window.removePostImage        = clearPublishImg;
// DM functions
window.toggleDMWindow         = toggleDMWindow;
window.showDMView             = showDMView;
window.openDMConversation     = openDMConversation;
window.closeDMConversation    = closeDMConversation;
window.sendDM                 = sendDM;
window.sendDMMsg              = sendDM;
window.loadDMConversations    = loadDMConversations;
window.loadDMMessages         = loadDMMessages;
window.deleteDMMessage        = deleteDMMessage;
window.toggleDMEmoji          = toggleDMEmoji;
window.toggleDMGif            = toggleDMGif;
window.selectDMGif            = selectDMGif;
window.searchDMGif            = searchDMGifs;
window.searchDMUser           = searchDMUser;
window.openDMSettings         = openDMSettings;
window.closeDMSettings        = closeDMSettings;
window.saveDMSettings         = saveDMSettings;
window.openCreateGroup        = openCreateGroup;
window.createGroup            = createGroup;
window.sendGroupMessage       = sendGroupMessage;
window.openGroupConversation  = openGroupConversation;
window.closeGroupConversation = closeGroupConversation;
window.openGroupInfo          = openGroupInfo;
window.renameGroup            = renameGroup;
window.leaveGroup             = leaveGroup;
window.addGroupMember         = searchGroupAddMember;
window.removeGroupMember      = removeMemberFromGroup;
window.searchGroupMember      = searchGroupMember;
window.openProfileModal       = openProfileModal;
window.closeProfileModal      = closeProfileModal;
window.updateDMBadge          = updateDMBadge;
window.openImageLightbox      = openImageLightbox;
window.closeImageLightbox     = closeImageLightbox;

// ─── NOTIFICATIONS ────────────────────────────────────────────────────────────
var notifPanelOpen = false;

function toggleNotifPanel() {
  notifPanelOpen = !notifPanelOpen;
  var panel = document.getElementById('notif-panel');
  if (!panel) return;
  if (notifPanelOpen) {
    panel.classList.remove('hidden');
    loadNotifications();
  } else {
    panel.classList.add('hidden');
  }
}

function loadNotifications() {
  fetch('/api/notifications').then(function(r){ return r.json(); }).then(function(notifs) {
    renderNotifications(notifs);
    var unread = notifs.filter(function(n){ return !n.read; }).length;
    updateNotifBadge(unread);
  }).catch(function(){});
}

function renderNotifications(notifs) {
  const container = document.getElementById('notif-list');
  var markBtn = document.getElementById('notif-mark-all-btn');
  if (!container) return;
  var unread = notifs ? notifs.filter(function(n){ return !n.read; }).length : 0;
  if (markBtn) markBtn.style.display = unread > 0 ? 'inline-block' : 'none';
  if (!notifs || notifs.length === 0) {
    container.innerHTML = '<div style="padding:16px;text-align:center;color:#666;font-size:13px;">Aucune notification</div>';
    return;
  }
  container.innerHTML = notifs.map(function(n) {
    var bg = n.read ? 'transparent' : 'rgba(0,200,100,0.06)';
    var dot = !n.read ? '<div style="width:8px;height:8px;border-radius:50%;background:#00c864;flex-shrink:0;margin-top:4px;"></div>' : '';
    return '<div onclick="goToNotif(\'' + n._id + '\',\'' + n.postID + '\')" style="display:flex;gap:10px;align-items:flex-start;padding:12px 14px;background:' + bg + ';border-bottom:1px solid rgba(255,255,255,0.06);cursor:pointer;" onmouseover="this.style.background=\'rgba(255,255,255,0.04)\'" onmouseout="this.style.background=\'' + bg + '\'">'
      + '<div style="width:36px;height:36px;border-radius:50%;background:#1a3a2a;display:flex;align-items:center;justify-content:center;font-size:14px;font-weight:700;color:#00c864;flex-shrink:0;">'
        + (n.fromUsername || '?')[0].toUpperCase()
      + '</div>'
      + '<div style="flex:1;min-width:0;">'
        + '<div style="font-size:13px;color:#e0e0e0;margin-bottom:3px;"><strong style="color:#00c864;">@' + (n.fromUsername || 'Anonyme') + '</strong> a commenté votre publication</div>'
        + '<div style="font-size:12px;color:#888;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;">' + (n.commentPreview || '') + '</div>'
        + '<div style="font-size:11px;color:#555;margin-top:3px;">' + timeAgo(n.createdAt) + '</div>'
      + '</div>'
      + dot
    + '</div>';
  }).join('');
}

function goToNotif(id, postID) {
  fetch('/api/notifications/read/' + id, {method:'POST'});
  window.location.href = '/#post-' + postID;
  closeNotifPopup();
}

function closeNotifPopup() {
  notifPanelOpen = false;
  var panel = document.getElementById('notif-panel');
  if (panel) panel.classList.add('hidden');
}

function timeAgo(dateStr) {
  const diff = Math.floor((Date.now() - new Date(dateStr)) / 1000);
  if (diff < 60) return 'à l\'instant';
  if (diff < 3600) return 'il y a ' + Math.floor(diff/60) + 'min';
  if (diff < 86400) return 'il y a ' + Math.floor(diff/3600) + 'h';
  return 'il y a ' + Math.floor(diff/86400) + 'j';
}

function markAllNotifsRead() {
  fetch('/api/notifications/read', {method:'POST'}).then(function(){ loadNotifications(); }).catch(function(){});
}

function updateNotifBadge(count) {
  var badge = document.getElementById('notif-badge');
  if (!badge) return;
  if (count > 0) {
    badge.textContent = count > 99 ? '99+' : String(count);
    badge.style.display = 'flex';
  } else {
    badge.style.display = 'none';
  }
}

function loadNotifCount() {
  fetch('/api/notifications').then(function(r){ return r.json(); }).then(function(notifs) {
    var unread = notifs ? notifs.filter(function(n){ return !n.read; }).length : 0;
    updateNotifBadge(unread);
    if (notifPanelOpen) renderNotifications(notifs);
  }).catch(function(){});
}

// ─── NOTIFICATION POLLING (every 10s) ─────────────────────────────────────────
var _notifPollSince = Math.floor(Date.now() / 1000);

function pollNotifications() {
  fetch('/api/notifications/poll?since=' + _notifPollSince)
    .then(function(r){ return r.json(); })
    .then(function(notifs) {
      if (!notifs || !notifs.length) return;
      _notifPollSince = Math.floor(Date.now() / 1000);
      // Update badge
      loadNotifCount();
      // Show one toast per new notification
      notifs.forEach(function(n) { showNotifToast(n); });
    })
    .catch(function(){});
}

function showNotifToast(n) {
  var container = document.getElementById('notif-toast-container');
  if (!container) return;
  var toast = document.createElement('div');
  toast.className = 'notif-toast';
  var initial = (n.fromUsername || '?')[0].toUpperCase();
  var text = '<strong style="color:#00e85a;">@' + (n.fromUsername || 'Anonyme') + '</strong> ';
  if (n.type === 'mention') {
    text += 'vous a mentionné';
  } else {
    text += 'a commenté votre publication';
  }
  toast.innerHTML =
    '<div class="notif-toast-icon">' + initial + '</div>' +
    '<div class="notif-toast-body">' +
      '<div class="notif-toast-text">' + text + '</div>' +
      (n.commentPreview ? '<div class="notif-toast-preview">' + n.commentPreview + '</div>' : '') +
    '</div>' +
    '<button class="notif-toast-close" onclick="this.parentNode.remove()">✕</button>';
  toast.onclick = function(e) {
    if (e.target.classList.contains('notif-toast-close')) return;
    goToNotif(n._id, n.postID);
    toast.remove();
  };
  container.appendChild(toast);
  // Animate in
  requestAnimationFrame(function() {
    requestAnimationFrame(function() { toast.classList.add('toast-in'); });
  });
  // Auto-dismiss after 5s
  var timer = setTimeout(function() {
    toast.classList.remove('toast-in');
    toast.classList.add('toast-out');
    setTimeout(function() { toast.remove(); }, 400);
  }, 5000);
  toast.addEventListener('mouseenter', function() { clearTimeout(timer); });
}

// Close panel on outside click
document.addEventListener('click', function(e) {
  if (!notifPanelOpen) return;
  var panel = document.getElementById('notif-panel');
  var btn = document.getElementById('notif-btn');
  if (panel && btn && !panel.contains(e.target) && !btn.contains(e.target)) {
    toggleNotifPanel();
  }
});

function clickNotif(id, postID) { goToNotif(id, postID); }
window.toggleNotifPanel = toggleNotifPanel;
window.markAllNotifsRead = markAllNotifsRead;
window.clickNotif = clickNotif;

if (typeof IS_LOGGED_IN !== 'undefined' && IS_LOGGED_IN === 'true' || IS_LOGGED_IN === true) {
  loadNotifCount();
  setInterval(loadNotifCount, 30000);
  // Poll for new notifications every 10s
  setTimeout(function() {
    pollNotifications();
    setInterval(pollNotifications, 10000);
  }, 5000);
}

// ─── NAV SEARCH ───
function onNavSearchInput(){
  var q=document.getElementById('nav-search-q');
  var clear=document.getElementById('nav-search-clear');
  var err=document.getElementById('nav-search-error');
  if(clear)clear.style.display=q&&q.value?'block':'none';
  if(err)err.style.display='none';
}
function clearNavSearch(){
  var q=document.getElementById('nav-search-q');
  var clear=document.getElementById('nav-search-clear');
  var err=document.getElementById('nav-search-error');
  if(q){q.value='';q.focus();}
  if(clear)clear.style.display='none';
  if(err)err.style.display='none';
}
function validateNavSearch(e){
  var q=document.getElementById('nav-search-q');
  var err=document.getElementById('nav-search-error');
  if(!q)return true;
  var v=q.value.trim();
  if(!v){e.preventDefault();return false;}
  var isNum=/^\d+$/.test(v);
  var isSCO=v.indexOf('SCO')===0;
  var isHash=v.length===64&&/^[0-9a-fA-F]+$/.test(v);
  if(!isNum&&!isSCO&&!isHash){
    e.preventDefault();
    if(err){err.textContent='Aucun résultat trouvé';err.style.display='block';setTimeout(function(){err.style.display='none';},3000);}
    return false;
  }
  return true;
}
window.onNavSearchInput=onNavSearchInput;
window.clearNavSearch=clearNavSearch;
window.validateNavSearch=validateNavSearch;

</script>
<div id="img-lightbox" onclick="closeImageLightbox()"><img id="img-lightbox-img" src="" alt=""></div>
</body>
</html>`
}

// ─── SCO PURCHASE ──────────────────────────────────────────────────────────────

// SCOOrder represents a purchase order stored in MongoDB
type SCOOrder struct {
	ID           bson.ObjectID `bson:"_id,omitempty" json:"id"`
	Crypto       string        `bson:"crypto" json:"crypto"`
	AmountEUR    float64       `bson:"amount_eur" json:"amount_eur"`
	AmountCrypto float64       `bson:"amount_crypto" json:"amount_crypto"`
	SCOAmount    float64       `bson:"sco_amount" json:"sco_amount"`
	AddressToPay string        `bson:"address_to_pay" json:"address_to_pay"`
	SCOAddress   string        `bson:"sco_address" json:"sco_address"`
	Email        string        `bson:"email" json:"email"`
	Status       string        `bson:"status" json:"status"`
	CreatedAt    time.Time     `bson:"created_at" json:"created_at"`
	ExpiresAt    time.Time     `bson:"expires_at" json:"expires_at"`
	TxDetected   bool          `bson:"tx_detected" json:"tx_detected"`
	Memo         string        `bson:"memo,omitempty" json:"memo,omitempty"`
}

// apiScoPrice returns live BTC/DOGE/XRP→EUR rates and SCO price
func (e *Explorer) apiScoPrice(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("https://api.coingecko.com/api/v3/simple/price?ids=bitcoin,dogecoin,ripple&vs_currencies=eur")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"btc": 0, "doge": 0, "xrp": 0, "sco_price_eur": 0.07})
		return
	}
	defer resp.Body.Close()
	var cg map[string]map[string]float64
	json.NewDecoder(resp.Body).Decode(&cg)

	btcEUR := cg["bitcoin"]["eur"]
	dogeEUR := cg["dogecoin"]["eur"]
	xrpEUR := cg["ripple"]["eur"]

	var btcRate, dogeRate, xrpRate float64
	if btcEUR > 0 {
		btcRate = 1.0 / btcEUR
	}
	if dogeEUR > 0 {
		dogeRate = 1.0 / dogeEUR
	}
	if xrpEUR > 0 {
		xrpRate = 1.0 / xrpEUR
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"btc":           btcRate,
		"doge":          dogeRate,
		"xrp":           xrpRate,
		"sco_price_eur": 0.07,
	})
}

// apiScoOrder creates a new SCO purchase order
func (e *Explorer) apiScoOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}
	var req struct {
		Crypto     string  `json:"crypto"`
		AmountEUR  float64 `json:"amount_eur"`
		Email      string  `json:"email"`
		SCOAddress string  `json:"sco_address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "JSON invalide", 400)
		return
	}
	switch req.Crypto {
	case "btc":
		if req.AmountEUR < 10 {
			jsonError(w, "Minimum 10 EUR pour BTC", 400)
			return
		}
	case "doge", "xrp":
		if req.AmountEUR < 2 {
			jsonError(w, "Minimum 2 EUR pour "+strings.ToUpper(req.Crypto), 400)
			return
		}
	}
	req.SCOAddress = strings.TrimSpace(req.SCOAddress)
	if !strings.HasPrefix(req.SCOAddress, "SCO") {
		jsonError(w, "Adresse SCO invalide", 400)
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		jsonError(w, "Email invalide", 400)
		return
	}

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("https://api.coingecko.com/api/v3/simple/price?ids=bitcoin,dogecoin,ripple&vs_currencies=eur")
	if err != nil {
		jsonError(w, "Impossible de récupérer les cours", 500)
		return
	}
	defer resp.Body.Close()
	var cg map[string]map[string]float64
	json.NewDecoder(resp.Body).Decode(&cg)

	var cryptoRateEUR float64
	var addressToPay string
	switch req.Crypto {
	case "btc":
		cryptoRateEUR = cg["bitcoin"]["eur"]
		addressToPay = "15ZHZNfNpdK9QecvJz4FHkmiBSY2tXKpxm"
	case "doge":
		cryptoRateEUR = cg["dogecoin"]["eur"]
		addressToPay = "DKgohnvSCv4uchHoAUG4eXNEDXzZGdGC2E"
	case "xrp":
		cryptoRateEUR = cg["ripple"]["eur"]
		addressToPay = "rNxp4h8apvRis6mJf9Sh8C6iRxfrDWN7AV"
	default:
		jsonError(w, "Crypto non supportée (btc/doge/xrp)", 400)
		return
	}

	if cryptoRateEUR <= 0 {
		jsonError(w, "Cours indisponible", 500)
		return
	}

	amountCrypto := req.AmountEUR / cryptoRateEUR
	scoAmount := req.AmountEUR / 0.07
	now := time.Now()

	memo := ""
	if req.Crypto == "xrp" {
		memo = "415404296"
	}
	order := SCOOrder{
		Crypto:       req.Crypto,
		AmountEUR:    req.AmountEUR,
		AmountCrypto: amountCrypto,
		SCOAmount:    scoAmount,
		AddressToPay: addressToPay,
		SCOAddress:   req.SCOAddress,
		Email:        req.Email,
		Status:       "pending",
		CreatedAt:    now,
		ExpiresAt:    now.Add(30 * time.Minute),
		TxDetected:   false,
		Memo:         memo,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	col := db.Collection("sco_orders")
	result, err := col.InsertOne(ctx, order)
	if err != nil {
		jsonError(w, "Erreur création commande", 500)
		return
	}
	order.ID = result.InsertedID.(bson.ObjectID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(order)
}

// apiScoOrderStatus returns the status of a purchase order
func (e *Explorer) apiScoOrderStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sco/order/")
	if id == "" {
		jsonError(w, "ID requis", 400)
		return
	}
	oid, err := bson.ObjectIDFromHex(id)
	if err != nil {
		jsonError(w, "ID invalide", 400)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	col := db.Collection("sco_orders")
	var order SCOOrder
	if err := col.FindOne(ctx, bson.M{"_id": oid}).Decode(&order); err != nil {
		jsonError(w, "Commande introuvable", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(order)
}

// startScoOrderChecker runs a background loop checking pending orders every 30s
func (e *Explorer) startScoOrderChecker() {
	for range time.Tick(30 * time.Second) {
		e.checkScoOrders()
	}
}

func (e *Explorer) checkScoOrders() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	col := db.Collection("sco_orders")

	cursor, err := col.Find(ctx, bson.M{"status": "pending"})
	if err != nil {
		return
	}
	defer cursor.Close(ctx)

	var orders []SCOOrder
	if err := cursor.All(ctx, &orders); err != nil {
		return
	}

	now := time.Now()
	for _, order := range orders {
		if now.After(order.ExpiresAt) {
			col.UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{"$set": bson.M{"status": "expired"}})
			continue
		}

		detected := false
		switch order.Crypto {
		case "btc":
			detected = checkBtcPayment(order)
		case "doge":
			detected = checkDogePayment(order)
		case "xrp":
			detected = checkXrpPayment(order)
		}

		if !detected {
			continue
		}

		col.UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{"$set": bson.M{"status": "confirmed", "tx_detected": true}})

		// Send SCO from premine address
		premineAddr := "SCO30e1a847eaf8aac6b042c32d63108833"
		scoAmount := int(order.SCOAmount)
		if scoAmount > 0 {
			tx := transaction.NewTransaction(premineAddr, order.SCOAddress, scoAmount, 0)
			e.mp.Add(tx)
		}

		col.UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{"$set": bson.M{"status": "sent"}})

		// Send confirmation email
		emailBody := fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="UTF-8"></head>
<body style="background:#030a04;color:#cffadb;font-family:Inter,sans-serif;margin:0;padding:0;">
<div style="text-align:center;padding:20px 0 10px;">
  <img src="https://scorbits.com/static/scorbits_logo.png" alt="Scorbits" style="height:60px;width:auto;">
</div>
<div style="max-width:520px;margin:0 auto 40px;background:#071009;border:1px solid #006628;border-radius:12px;overflow:hidden;">
  <div style="background:#003314;padding:2rem;text-align:center;border-bottom:1px solid #006628;">
    <div style="font-size:1.4rem;font-weight:700;color:#cffadb;">Achat de SCO confirmé !</div>
  </div>
  <div style="padding:2rem;">
    <p style="color:#7ab98a;line-height:1.7;">Bonjour,</p>
    <p style="color:#7ab98a;line-height:1.7;">Votre achat de <strong style="color:#00e85a;">%d SCO</strong> a été confirmé. Ils ont été envoyés à votre adresse SCO :</p>
    <p style="font-family:monospace;color:#00e85a;word-break:break-all;background:#0d1a0d;padding:0.8rem;border-radius:6px;">%s</p>
    <p style="color:#7ab98a;line-height:1.7;">Merci d'avoir acheté des SCO sur <a href="https://scorbits.com" style="color:#00e85a;">Scorbits</a>.</p>
  </div>
  <div style="background:#030a04;padding:1rem;text-align:center;border-top:1px solid #0d2e15;">
    <p style="color:#2d6b3f;font-size:0.75rem;margin:0;">Scorbits (SCO) — Proof of Work</p>
  </div>
</div>
</body></html>`, scoAmount, order.SCOAddress)
		email.Send(order.Email, "Votre achat de SCO est confirmé", emailBody)

		fmt.Printf("[SCO Order] Commande %s traitée : %d SCO → %s\n", order.ID.Hex(), scoAmount, order.SCOAddress)
	}
}

// checkBtcPayment checks if a BTC payment has been received for the order
func checkBtcPayment(order SCOOrder) bool {
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Get("https://blockchain.info/rawaddr/" + order.AddressToPay)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var data struct {
		Txs []struct {
			Time int64 `json:"time"`
			Out  []struct {
				Value int64  `json:"value"`
				Addr  string `json:"addr"`
			} `json:"out"`
		} `json:"txs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false
	}
	expectedSatoshi := int64(order.AmountCrypto * 1e8)
	tolerance := int64(float64(expectedSatoshi) * 0.02)
	for _, tx := range data.Txs {
		if tx.Time < order.CreatedAt.Unix() {
			continue
		}
		for _, out := range tx.Out {
			if out.Addr == order.AddressToPay {
				diff := out.Value - expectedSatoshi
				if diff < 0 {
					diff = -diff
				}
				if diff <= tolerance {
					return true
				}
			}
		}
	}
	return false
}

// checkDogePayment checks if a DOGE payment has been received for the order
func checkDogePayment(order SCOOrder) bool {
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Get("https://dogechain.info/api/v1/address/transactions/" + order.AddressToPay)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var data struct {
		Transactions []struct {
			Time    int64 `json:"time"`
			Outputs []struct {
				Value   string `json:"value"`
				Address string `json:"address"`
			} `json:"outputs"`
		} `json:"transactions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false
	}
	for _, tx := range data.Transactions {
		if tx.Time < order.CreatedAt.Unix() {
			continue
		}
		for _, out := range tx.Outputs {
			if out.Address != order.AddressToPay {
				continue
			}
			val, err := strconv.ParseFloat(out.Value, 64)
			if err != nil {
				continue
			}
			diff := val - order.AmountCrypto
			if diff < 0 {
				diff = -diff
			}
			if diff <= order.AmountCrypto*0.02 {
				return true
			}
		}
	}
	return false
}

// apiScoOrderCancel cancels a pending SCO purchase order
func (e *Explorer) apiScoOrderCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}
	var req struct {
		OrderID string `json:"order_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrderID == "" {
		jsonError(w, "order_id requis", 400)
		return
	}
	oid, err := bson.ObjectIDFromHex(req.OrderID)
	if err != nil {
		jsonError(w, "ID invalide", 400)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	col := db.Collection("sco_orders")
	col.UpdateOne(ctx, bson.M{"_id": oid, "status": "pending"}, bson.M{"$set": bson.M{"status": "cancelled"}})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiScoTestPayment simulates a payment detection for a pending order (localhost only)
func (e *Explorer) apiScoTestPayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST requis", 405)
		return
	}

	// Restrict to localhost only
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		http.Error(w, "Forbidden", 403)
		return
	}

	orderID := strings.TrimPrefix(r.URL.Path, "/api/sco/test-payment/")
	if orderID == "" {
		jsonError(w, "ID requis", 400)
		return
	}
	oid, err := bson.ObjectIDFromHex(orderID)
	if err != nil {
		jsonError(w, "ID invalide", 400)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	col := db.Collection("sco_orders")

	var order SCOOrder
	if err := col.FindOne(ctx, bson.M{"_id": oid}).Decode(&order); err != nil {
		jsonError(w, "Commande introuvable", 404)
		return
	}

	if order.Status != "pending" {
		jsonError(w, "La commande n'est pas en attente (status: "+order.Status+")", 400)
		return
	}

	// Mark as confirmed
	col.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": bson.M{"status": "confirmed", "tx_detected": true}})

	// Send SCO from premine
	premineAddr := "SCO30e1a847eaf8aac6b042c32d63108833"
	scoAmount := int(order.SCOAmount)
	if scoAmount > 0 {
		tx := transaction.NewTransaction(premineAddr, order.SCOAddress, scoAmount, 0)
		e.mp.Add(tx)
	}

	col.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": bson.M{"status": "sent"}})

	// Send confirmation email
	emailBody := fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="UTF-8"></head>
<body style="background:#030a04;color:#cffadb;font-family:Inter,sans-serif;margin:0;padding:0;">
<div style="text-align:center;padding:20px 0 10px;">
  <img src="https://scorbits.com/static/scorbits_logo.png" alt="Scorbits" style="height:60px;width:auto;">
</div>
<div style="max-width:520px;margin:0 auto 40px;background:#071009;border:1px solid #006628;border-radius:12px;overflow:hidden;">
  <div style="background:#003314;padding:2rem;text-align:center;border-bottom:1px solid #006628;">
    <div style="font-size:1.4rem;font-weight:700;color:#cffadb;">Achat de SCO confirmé !</div>
  </div>
  <div style="padding:2rem;">
    <p style="color:#7ab98a;line-height:1.7;">Bonjour,</p>
    <p style="color:#7ab98a;line-height:1.7;">Votre achat de <strong style="color:#00e85a;">%d SCO</strong> a été confirmé. Ils ont été envoyés à votre adresse SCO :</p>
    <p style="font-family:monospace;color:#00e85a;word-break:break-all;background:#0d1a0d;padding:0.8rem;border-radius:6px;">%s</p>
    <p style="color:#7ab98a;line-height:1.7;">Merci d'avoir acheté des SCO sur <a href="https://scorbits.com" style="color:#00e85a;">Scorbits</a>.</p>
  </div>
  <div style="background:#030a04;padding:1rem;text-align:center;border-top:1px solid #0d2e15;">
    <p style="color:#2d6b3f;font-size:0.75rem;margin:0;">Scorbits (SCO) — Proof of Work</p>
  </div>
</div>
</body></html>`, scoAmount, order.SCOAddress)
	email.Send(order.Email, "Votre achat de SCO est confirmé", emailBody)

	fmt.Printf("[SCO TestPayment] Commande %s simulée : %d SCO → %s\n", order.ID.Hex(), scoAmount, order.SCOAddress)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":       true,
		"sco_sent": scoAmount,
		"to":       order.SCOAddress,
	})
}

// checkXrpPayment checks if an XRP payment has been received for the order
func checkXrpPayment(order SCOOrder) bool {
	hc := &http.Client{Timeout: 10 * time.Second}
	reqURL := fmt.Sprintf("https://data.ripple.com/v2/accounts/%s/transactions?type=Payment&limit=10", order.AddressToPay)
	resp, err := hc.Get(reqURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var data struct {
		Transactions []struct {
			Date string `json:"date"`
			Tx   struct {
				Amount      interface{} `json:"Amount"`
				Destination string      `json:"Destination"`
			} `json:"tx"`
		} `json:"transactions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false
	}
	for _, txEntry := range data.Transactions {
		t, err := time.Parse(time.RFC3339, txEntry.Date)
		if err != nil || t.Before(order.CreatedAt) {
			continue
		}
		if txEntry.Tx.Destination != order.AddressToPay {
			continue
		}
		var drops int64
		switch v := txEntry.Tx.Amount.(type) {
		case string:
			drops, _ = strconv.ParseInt(v, 10, 64)
		case float64:
			drops = int64(v)
		}
		xrpAmount := float64(drops) / 1e6
		diff := xrpAmount - order.AmountCrypto
		if diff < 0 {
			diff = -diff
		}
		if diff <= order.AmountCrypto*0.02 {
			return true
		}
	}
	return false
}

// ─── WALLETS PAGE ─────────────────────────────────────────────────────────────

func (e *Explorer) handleWallets(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page("Wallets SCO — Scorbits", walletsHTML(lang), "wallets", lang, user))
}

func (e *Explorer) handleTransactions(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	content := `
<div style="max-width:900px;margin:0 auto">
  <div class="stitle">All Transactions</div>
  <div class="tbox">
    <table>
      <thead><tr>
        <th>Block</th><th>From</th><th>To</th><th>Amount</th><th>Time</th>
      </tr></thead>
      <tbody id="tx-all-list"><tr><td colspan="5" style="text-align:center;color:var(--muted)">Loading...</td></tr></tbody>
    </table>
  </div>
</div>
<script>
(async () => {
  const res = await fetch('/api/transactions');
  const txs = await res.json() || [];
  const el = document.getElementById('tx-all-list');
  if (!txs.length) {
    el.innerHTML = '<tr><td colspan="5" style="text-align:center;color:var(--muted)">No transactions yet</td></tr>';
    return;
  }
  el.innerHTML = txs.map(t => {
    const fromShort = t.from.length > 18 ? t.from.slice(0,10)+'...'+t.from.slice(-6) : t.from;
    const toShort = t.to.length > 18 ? t.to.slice(0,10)+'...'+t.to.slice(-6) : t.to;
    const d = new Date(t.timestamp*1000).toLocaleString([], {day:'2-digit',month:'2-digit',hour:'2-digit',minute:'2-digit'});
    return '<tr onclick="window.location=\'/block/'+t.block+'\'">' +
      '<td><span class="badge">#'+t.block+'</span></td>' +
      '<td class="mono sm" title="'+t.from+'">'+fromShort+'</td>' +
      '<td class="mono sm" title="'+t.to+'">'+toShort+'</td>' +
      '<td class="green fw6">'+t.amount+' SCO</td>' +
      '<td class="muted sm">'+d+'</td>' +
    '</tr>';
  }).join('');
})();
</script>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page("Transactions — Scorbits", content, "explorer", lang, user))
}

func walletsHTML(lang string) string {
	fr := lang == "fr"
	tl := func(frStr, enStr string) string {
		if fr {
			return frStr
		}
		return enStr
	}
	return `
<style>
.wlt-wrap{max-width:900px;margin:0 auto;padding:2rem 1rem;}
.wlt-hero{margin-bottom:2.5rem;}
.wlt-hero h1{font-size:1.8rem;font-weight:700;color:#fff;margin-bottom:.4rem;}
.wlt-hero .wlt-subtitle{font-size:1rem;color:#00e85a;font-weight:600;margin-bottom:.7rem;}
.wlt-hero p{color:#888;font-size:.95rem;max-width:620px;line-height:1.6;}
.wlt-section-title{font-size:.7rem;text-transform:uppercase;letter-spacing:.12em;color:#555;margin-bottom:1.2rem;padding-bottom:.5rem;border-bottom:1px solid #1a1a1a;}

/* Official wallet card */
.wlt-official{background:#080f0a;border:1px solid #00e85a;border-radius:14px;padding:1.8rem;margin-bottom:2.5rem;box-shadow:0 0 18px rgba(0,232,90,.07);}
.wlt-official-header{display:flex;align-items:center;gap:.75rem;margin-bottom:1rem;}
.wlt-official-header h2{font-size:1.15rem;font-weight:700;color:#fff;margin:0;}
.wlt-badge-recommended{background:#00e85a;color:#000;font-size:.65rem;font-weight:800;text-transform:uppercase;letter-spacing:.08em;padding:2px 9px;border-radius:20px;flex-shrink:0;}
.wlt-official p{color:#aaa;font-size:.92rem;line-height:1.6;margin-bottom:1.2rem;}
.wlt-advantages{list-style:none;padding:0;margin:0 0 1.3rem 0;display:grid;grid-template-columns:1fr 1fr;gap:.5rem;}
@media(max-width:520px){.wlt-advantages{grid-template-columns:1fr;}}
.wlt-advantages li{font-size:.87rem;color:#ccc;display:flex;align-items:center;gap:.5rem;}
.wlt-advantages li::before{content:'✓';color:#00e85a;font-weight:700;flex-shrink:0;}
.wlt-warning{background:#0d0d0d;border:1px solid #2a2a2a;border-radius:8px;padding:.75rem 1rem;font-size:.8rem;color:#666;line-height:1.5;margin-bottom:1.4rem;}
.wlt-warning strong{color:#888;}
.wlt-btn-primary{display:inline-block;background:#00e85a;color:#000;font-weight:700;font-size:.9rem;padding:.65rem 1.6rem;border-radius:8px;text-decoration:none;transition:opacity .2s;}
.wlt-btn-primary:hover{opacity:.85;}

/* External wallets grid */
.wlt-ext-intro{color:#666;font-size:.88rem;margin-bottom:1.4rem;}
.wlt-ext-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:1rem;margin-bottom:2.5rem;}
@media(max-width:700px){.wlt-ext-grid{grid-template-columns:1fr 1fr;}}
@media(max-width:440px){.wlt-ext-grid{grid-template-columns:1fr;}}
.wlt-ext-card{background:#0d0d0d;border:1px solid #333;border-radius:12px;padding:1.3rem 1rem;opacity:.6;position:relative;}
.wlt-ext-card-title{font-size:.95rem;font-weight:700;color:#ccc;margin-bottom:.5rem;}
.wlt-ext-card-desc{font-size:.82rem;color:#666;line-height:1.5;}
.wlt-badge-soon{display:inline-block;background:#f5a623;color:#000;font-size:.62rem;font-weight:800;text-transform:uppercase;letter-spacing:.07em;padding:2px 8px;border-radius:20px;margin-bottom:.6rem;}

/* FAQ accordion */
.wlt-faq{margin-bottom:2rem;}
.wlt-faq-item{border-bottom:1px solid #1a1a1a;}
.wlt-faq-q{width:100%;background:none;border:none;text-align:left;padding:.9rem 0;cursor:pointer;display:flex;align-items:center;justify-content:space-between;gap:1rem;color:#ccc;font-size:.92rem;font-weight:600;}
.wlt-faq-q:hover{color:#fff;}
.wlt-faq-chevron{flex-shrink:0;transition:transform .25s;color:#555;}
.wlt-faq-item.open .wlt-faq-chevron{transform:rotate(180deg);}
.wlt-faq-a{display:none;padding:.1rem 0 .9rem 0;color:#777;font-size:.88rem;line-height:1.65;}
.wlt-faq-item.open .wlt-faq-a{display:block;}
</style>

<div class="wlt-wrap">
  <!-- Section 1: Hero -->
  <div class="wlt-hero">
    <h1>` + tl("Wallets Scorbits (SCO)", "Scorbits Wallets (SCO)") + `</h1>
    <div class="wlt-subtitle">` + tl("Comment stocker et sécuriser vos SCO", "How to store and secure your SCO") + `</div>
    <p>` + tl("Scorbits (SCO) est une blockchain indépendante. Pour stocker vos SCO, vous avez besoin d'un wallet compatible avec le réseau Scorbits.", "Scorbits (SCO) is an independent blockchain. To store your SCO, you need a wallet compatible with the Scorbits network.") + `</p>
  </div>

  <!-- Section 2: Official wallet -->
  <div class="wlt-section-title">` + tl("Wallet officiel", "Official Wallet") + `</div>
  <div class="wlt-official">
    <div class="wlt-official-header">
      <h2>` + tl("Wallet Web Officiel — scorbits.com", "Official Web Wallet — scorbits.com") + `</h2>
      <span class="wlt-badge-recommended">` + tl("Recommandé", "Recommended") + `</span>
    </div>
    <p>` + tl("Le wallet intégré à scorbits.com est le wallet officiel de Scorbits. Il vous permet d'envoyer, recevoir et gérer vos SCO directement depuis votre navigateur.", "The wallet integrated into scorbits.com is the official Scorbits wallet. It lets you send, receive, and manage your SCO directly from your browser.") + `</p>
    <ul class="wlt-advantages">
      <li>` + tl("Accessible depuis n'importe quel appareil", "Accessible from any device") + `</li>
      <li>` + tl("Aucune installation requise", "No installation required") + `</li>
      <li>` + tl("Historique complet des transactions", "Full transaction history") + `</li>
      <li>` + tl("Notifications en temps réel", "Real-time notifications") + `</li>
    </ul>
    <div class="wlt-warning">
      <strong>` + tl("Note :", "Note:") + `</strong> ` + tl("Wallet custodial — vos clés privées sont gérées par le serveur Scorbits. Ne stockez pas de montants importants sans sauvegarder votre adresse SCO.", "Custodial wallet — your private keys are managed by the Scorbits server. Do not store large amounts without saving your SCO address.") + `
    </div>
    <a href="/wallet/register" class="wlt-btn-primary">` + tl("Créer un wallet", "Create a wallet") + `</a>
  </div>

  <!-- Section 3: External wallets -->
  <div class="wlt-section-title">` + tl("Wallets externes", "External Wallets") + `</div>
  <p class="wlt-ext-intro">` + tl("Les intégrations avec les wallets externes sont en cours de développement et seront disponibles progressivement.", "Integrations with external wallets are under development and will be available progressively.") + `</p>
  <div class="wlt-ext-grid">
    <div class="wlt-ext-card">
      <div class="wlt-badge-soon">` + tl("Bientôt disponible", "Coming soon") + `</div>
      <div class="wlt-ext-card-title">Coinomi</div>
      <div class="wlt-ext-card-desc">` + tl("Support multi-crypto, disponible sur Windows, Mac, iOS et Android", "Multi-crypto support, available on Windows, Mac, iOS and Android") + `</div>
    </div>
    <div class="wlt-ext-card">
      <div class="wlt-badge-soon">` + tl("Bientôt disponible", "Coming soon") + `</div>
      <div class="wlt-ext-card-title">Trust Wallet</div>
      <div class="wlt-ext-card-desc">` + tl("Wallet mobile populaire, iOS et Android", "Popular mobile wallet, iOS and Android") + `</div>
    </div>
    <div class="wlt-ext-card">
      <div class="wlt-badge-soon">` + tl("Bientôt disponible", "Coming soon") + `</div>
      <div class="wlt-ext-card-title">` + tl("Wallet Desktop Scorbits", "Scorbits Desktop Wallet") + `</div>
      <div class="wlt-ext-card-desc">` + tl("Application desktop officielle Scorbits avec clés privées locales — en développement", "Official Scorbits desktop application with local private keys — in development") + `</div>
    </div>
  </div>

  <!-- Section 4: FAQ -->
  <div class="wlt-section-title">FAQ</div>
  <div class="wlt-faq">
    <div class="wlt-faq-item">
      <button class="wlt-faq-q" onclick="toggleFaq(this)">
        <span>` + tl("Puis-je utiliser MetaMask ?", "Can I use MetaMask?") + `</span>
        <svg class="wlt-faq-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
      </button>
      <div class="wlt-faq-a">` + tl("Non, MetaMask est conçu pour Ethereum. Scorbits est une blockchain indépendante SHA-256.", "No, MetaMask is designed for Ethereum. Scorbits is an independent SHA-256 blockchain.") + `</div>
    </div>
    <div class="wlt-faq-item">
      <button class="wlt-faq-q" onclick="toggleFaq(this)">
        <span>` + tl("Mes SCO sont-ils en sécurité ?", "Are my SCO safe?") + `</span>
        <svg class="wlt-faq-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
      </button>
      <div class="wlt-faq-a">` + tl("Le wallet web de scorbits.com est sécurisé. Pour une sécurité maximale, ne partagez jamais votre mot de passe et activez un email de récupération.", "The scorbits.com web wallet is secure. For maximum security, never share your password and enable a recovery email.") + `</div>
    </div>
    <div class="wlt-faq-item">
      <button class="wlt-faq-q" onclick="toggleFaq(this)">
        <span>` + tl("Quand les wallets externes seront-ils disponibles ?", "When will external wallets be available?") + `</span>
        <svg class="wlt-faq-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
      </button>
      <div class="wlt-faq-a">` + tl("Les intégrations externes seront développées après le listing sur les exchanges. Rejoignez notre communauté pour être informé.", "External integrations will be developed after listing on exchanges. Join our community to stay informed.") + `</div>
    </div>
  </div>
</div>

<script>
function toggleFaq(btn){
  var item=btn.parentElement;
  item.classList.toggle('open');
}
</script>
`
}

// ─── /pool page ───────────────────────────────────────────────────────────────

func (e *Explorer) handlePool(w http.ResponseWriter, r *http.Request) {
	lang := i18n.DetectLang(r)
	user, _ := auth.GetSession(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, page("Scorbits Pool", poolHTML(lang), "pool", lang, user))
}

func poolHTML(lang string) string {
	fr := lang == "fr"
	tl := func(frStr, enStr string) string {
		if fr {
			return frStr
		}
		return enStr
	}

	// HTML-embedded translations
	poolOnline  := tl("Pool en ligne", "Pool Online")
	heroDesc    := tl("Mine SCO en rejoignant le pool officiel Scorbits. Les récompenses sont partagées proportionnellement au hashrate fourni.", "Mine SCO by joining the official Scorbits pool. Rewards are shared proportionally based on hashrate contributed.")
	lblHashrate := tl("Hashrate pool", "Pool Hashrate")
	lblWorkers  := tl("Workers actifs", "Active Workers")
	lblBlocks   := tl("Blocs trouvés", "Blocks Found")
	lblFee      := tl("Frais pool", "Pool Fee")
	lblCountdown := tl("Prochain paiement dans", "Next payout in")
	h2Connect   := tl("Se connecter au pool", "Connect to the pool")
	lblServer   := tl("Serveur", "Server")
	lblProtocol := tl("Protocole", "Protocol")
	lblUser     := tl("Utilisateur", "Username")
	lblPassword := tl("Mot de passe", "Password")
	h2HowTo    := tl("Comment miner avec le pool", "How to mine with the pool")
	step1Text  := tl("Télécharge le miner CLI Scorbits depuis", "Download the Scorbits CLI miner from")
	step1Mine  := tl("la page Mine", "the Mine page")
	step1Src   := tl("ou compile depuis les sources.", "or compile from source.")
	step2Text  := tl("Lance le miner en mode pool :", "Launch the miner in pool mode:")
	step3Text  := tl("Les SCO minés sont distribués automatiquement vers ton adresse SCO. Vérifie ton solde sur", "Mined SCO are automatically distributed to your SCO address. Check your balance on")
	step3Wallet := tl("ton wallet", "your wallet")
	h2Workers  := tl("Workers connectés", "Connected Workers")
	h2Blocks   := tl("Derniers blocs trouvés", "Latest blocks found")
	loadingTxt := tl("Chargement...", "Loading...")

	// JS string variables (injected as JS literals using double-quote delimiters)
	jsNoWorkers  := tl("Aucun worker connecté actuellement.", "No workers connected at the moment.")
	jsAddrTh    := tl("Adresse", "Address")
	jsLastAct   := tl("Derniere activite", "Last activity")
	jsLastUpd   := tl("Mis a jour :", "Last update:")
	jsPoolErr   := tl("Impossible de contacter le pool. Verifiez que le service est actif.", "Unable to reach the pool. Please check that the service is running.")
	jsNoBlocks  := tl("Aucun bloc trouve par le pool pour l'instant.", "No blocks found by the pool yet.")
	jsMiner     := tl("Mineur", "Miner")
	jsReward    := tl("Recompense", "Reward")
	jsBlocksErr := tl("Impossible de charger les blocs du pool.", "Unable to load pool blocks.")
	jsAgoFmt    := tl("il y a %v%s", "%v%s ago")

	return `<style>
.pool-wrap{max-width:960px;margin:0 auto;padding:2rem 1rem;}
.pool-hero{margin-bottom:2.5rem;}
.pool-hero h1{font-size:2rem;font-weight:700;color:#fff;margin-bottom:.5rem;}
.pool-hero p{color:#8899aa;font-size:1rem;max-width:560px;}
.pool-stats-grid{display:grid;grid-template-columns:repeat(4,1fr);gap:1rem;margin-bottom:2rem;}
.pool-stat-card{background:#0d1f35;border:1px solid #1a3a5c;border-radius:12px;padding:1.2rem 1rem;text-align:center;}
.pool-stat-val{font-size:1.7rem;font-weight:700;color:#00ff88;font-variant-numeric:tabular-nums;}
.pool-stat-val.blue{color:#4fc3f7;}
.pool-stat-lbl{font-size:.72rem;color:#5577aa;text-transform:uppercase;letter-spacing:.08em;margin-top:.4rem;}
.pool-section{margin-bottom:2rem;}
.pool-section h2{font-size:1.1rem;font-weight:600;color:#cce0ff;margin-bottom:1rem;border-left:3px solid #1a3a5c;padding-left:.75rem;}
.pool-table{width:100%;border-collapse:collapse;}
.pool-table th{font-size:.72rem;color:#5577aa;text-transform:uppercase;letter-spacing:.06em;padding:.5rem .75rem;text-align:left;border-bottom:1px solid #1a3a5c;}
.pool-table td{font-size:.88rem;color:#cce0ff;padding:.55rem .75rem;border-bottom:1px solid #0f2040;}
.pool-table tr:last-child td{border-bottom:none;}
.pool-table td.mono{font-family:monospace;font-size:.82rem;color:#aac4e0;}
.pool-table td.green{color:#00ff88;}
.pool-table td.blue{color:#4fc3f7;}
.pool-table td.muted{color:#556677;}
.pool-connect-box{background:#0d1f35;border:1px solid #1a3a5c;border-radius:12px;padding:1.5rem;}
.pool-connect-box h3{font-size:1rem;font-weight:600;color:#cce0ff;margin-bottom:1rem;}
.pool-connect-row{display:flex;align-items:center;gap:.75rem;margin-bottom:.75rem;flex-wrap:wrap;}
.pool-connect-label{font-size:.78rem;color:#5577aa;text-transform:uppercase;letter-spacing:.06em;min-width:80px;}
.pool-connect-val{font-family:monospace;font-size:.95rem;color:#00ff88;background:#060f1e;border:1px solid #1a3a5c;border-radius:6px;padding:.3rem .75rem;}
.pool-steps{list-style:none;padding:0;margin:0;}
.pool-steps li{display:flex;gap:1rem;margin-bottom:1rem;align-items:flex-start;}
.pool-step-num{flex-shrink:0;width:28px;height:28px;border-radius:50%;background:#1a3a5c;color:#4fc3f7;font-size:.85rem;font-weight:700;display:flex;align-items:center;justify-content:center;}
.pool-step-body{font-size:.9rem;color:#aac4e0;line-height:1.5;}
.pool-step-body code{background:#060f1e;border:1px solid #1a3a5c;border-radius:4px;padding:.15rem .4rem;font-size:.82rem;color:#00ff88;}
.pool-badge{display:inline-flex;align-items:center;gap:.35rem;background:#0a2010;border:1px solid #1a4a20;border-radius:20px;padding:.25rem .75rem;font-size:.82rem;color:#00ff88;}
.pool-badge .dot{width:8px;height:8px;border-radius:50%;background:#00ff88;animation:blink-dot 1.4s infinite;}
.pool-loading{color:#5577aa;font-size:.9rem;text-align:center;padding:2rem;}
.pool-error{color:#ff6655;font-size:.85rem;padding:.5rem .75rem;background:#200a08;border:1px solid #4a1a10;border-radius:8px;}
.pool-refresh-info{font-size:.75rem;color:#334455;margin-top:.5rem;text-align:right;}
@media(max-width:680px){
  .pool-stats-grid{grid-template-columns:1fr 1fr;}
}
@media(max-width:400px){
  .pool-stats-grid{grid-template-columns:1fr;}
}
</style>

<div class="pool-wrap">

  <!-- Hero -->
  <div class="pool-hero">
    <div style="margin-bottom:.75rem;">
      <span class="pool-badge"><span class="dot"></span> ` + poolOnline + `</span>
    </div>
    <h1>Scorbits Pool</h1>
    <p>` + heroDesc + `</p>
  </div>

  <!-- Stats globales -->
  <div class="pool-stats-grid" id="pool-stats-grid">
    <div class="pool-stat-card"><div class="pool-stat-val blue" id="ps-hashrate">—</div><div class="pool-stat-lbl">` + lblHashrate + `</div></div>
    <div class="pool-stat-card"><div class="pool-stat-val" id="ps-workers">—</div><div class="pool-stat-lbl">` + lblWorkers + `</div></div>
    <div class="pool-stat-card"><div class="pool-stat-val blue" id="ps-blocks">—</div><div class="pool-stat-lbl">` + lblBlocks + `</div></div>
    <div class="pool-stat-card"><div class="pool-stat-val" id="ps-fee">2%</div><div class="pool-stat-lbl">` + lblFee + `</div></div>
    <div class="pool-stat-card"><div class="pool-stat-val" id="ps-countdown" style="font-size:1.3rem;">--:--:--</div><div class="pool-stat-lbl">` + lblCountdown + `</div></div>
  </div>

  <!-- Section : se connecter -->
  <div class="pool-section">
    <h2>` + h2Connect + `</h2>
    <div class="pool-connect-box">
      <div class="pool-connect-row">
        <span class="pool-connect-label">` + lblServer + `</span>
        <span class="pool-connect-val">51.91.122.48</span>
      </div>
      <div class="pool-connect-row">
        <span class="pool-connect-label">Port</span>
        <span class="pool-connect-val">3333</span>
      </div>
      <div class="pool-connect-row">
        <span class="pool-connect-label">` + lblProtocol + `</span>
        <span class="pool-connect-val">Stratum TCP</span>
      </div>
      <div class="pool-connect-row">
        <span class="pool-connect-label">` + lblUser + `</span>
        <span class="pool-connect-val">VOTRE_ADRESSE_SCO</span>
      </div>
      <div class="pool-connect-row">
        <span class="pool-connect-label">` + lblPassword + `</span>
        <span class="pool-connect-val">x</span>
      </div>
    </div>
  </div>

  <!-- Section : comment miner -->
  <div class="pool-section">
    <h2>` + h2HowTo + `</h2>
    <ul class="pool-steps">
      <li>
        <div class="pool-step-num">1</div>
        <div class="pool-step-body">` + step1Text + ` <a href="/mine" style="color:#4fc3f7;">` + step1Mine + `</a> ` + step1Src + `</div>
      </li>
      <li>
        <div class="pool-step-num">2</div>
        <div class="pool-step-body">` + step2Text + `<br>
          <code>scorbits-miner-windows.exe --address VOTRE_ADRESSE_SCO --pool stratum+tcp://pool.scorbits.com:3333 --threads 4</code>
        </div>
      </li>
      <li>
        <div class="pool-step-num">3</div>
        <div class="pool-step-body">` + step3Text + ` <a href="/wallet/dashboard" style="color:#4fc3f7;">` + step3Wallet + `</a>.</div>
      </li>
    </ul>
  </div>

  <!-- Section : workers connectés -->
  <div class="pool-section">
    <h2>` + h2Workers + ` <span id="workers-count" style="color:#5577aa;font-weight:400;font-size:.85rem;"></span></h2>
    <div id="pool-workers-container"><div class="pool-loading">` + loadingTxt + `</div></div>
    <div class="pool-refresh-info" id="pool-refresh-ts"></div>
  </div>

  <!-- Section : derniers blocs -->
  <div class="pool-section">
    <h2>` + h2Blocks + `</h2>
    <div id="pool-blocks-container"><div class="pool-loading">` + loadingTxt + `</div></div>
  </div>

</div>

<script>
var POOL_API = '';
var POOL_T = {
  noWorkers:  "` + jsNoWorkers + `",
  addrTh:     "` + jsAddrTh + `",
  lastAct:    "` + jsLastAct + `",
  lastUpd:    "` + jsLastUpd + `",
  poolErr:    "` + jsPoolErr + `",
  noBlocks:   "` + jsNoBlocks + `",
  miner:      "` + jsMiner + `",
  reward:     "` + jsReward + `",
  blocksErr:  "` + jsBlocksErr + `",
  agoFmt:     "` + jsAgoFmt + `"
};

function fmtHash(h) {
  if (!h || h === 0) return '0 H/s';
  if (h >= 1e9) return (h/1e9).toFixed(2) + ' GH/s';
  if (h >= 1e6) return (h/1e6).toFixed(2) + ' MH/s';
  if (h >= 1e3) return (h/1e3).toFixed(2) + ' KH/s';
  return h + ' H/s';
}

function fmtTime(iso) {
  if (!iso) return '—';
  var d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  var now = new Date();
  var diff = Math.floor((now - d) / 1000);
  var fmt = POOL_T.agoFmt;
  function ago(n, unit) { return fmt.replace('%v', n).replace('%s', unit); }
  if (diff < 60) return ago(diff, 's');
  if (diff < 3600) return ago(Math.floor(diff/60), 'min');
  if (diff < 86400) return ago(Math.floor(diff/3600), 'h');
  return d.toLocaleDateString();
}

function shortHash(h) {
  if (!h || h.length < 16) return h || '—';
  return h.slice(0,8) + '...' + h.slice(-8);
}

function loadStats() {
  fetch(POOL_API + '/api/proxy/pool/stats')
    .then(function(r){ return r.json(); })
    .then(function(d) {
      document.getElementById('ps-hashrate').textContent = fmtHash(d.pool_hashrate || 0);
      document.getElementById('ps-workers').textContent = d.workers_online || 0;
      document.getElementById('ps-blocks').textContent = d.blocks_found || 0;
    })
    .catch(function(){
      document.getElementById('ps-hashrate').textContent = 'N/A';
    });
}

function loadWorkers() {
  fetch(POOL_API + '/api/proxy/pool/workers')
    .then(function(r){ return r.json(); })
    .then(function(workers) {
      var count = workers ? workers.length : 0;
      document.getElementById('workers-count').textContent = '(' + count + ')';
      var c = document.getElementById('pool-workers-container');
      if (!count) {
        c.innerHTML = '<div class="pool-loading">' + POOL_T.noWorkers + '</div>';
        return;
      }
      var rows = workers.map(function(w) {
        return '<tr>' +
          '<td class="mono">' + (w.address ? w.address.slice(0,10)+'...'+w.address.slice(-6) : '—') + '</td>' +
          '<td>' + (w.worker || 'worker') + '</td>' +
          '<td class="blue">' + fmtHash(w.hashrate || 0) + '</td>' +
          '<td class="green">' + (w.shares_valid || 0) + '</td>' +
          '<td class="muted">' + fmtTime(w.last_share) + '</td>' +
        '</tr>';
      }).join('');
      c.innerHTML = '<table class="pool-table"><thead><tr>' +
        '<th>' + POOL_T.addrTh + '</th><th>Worker</th><th>Hashrate</th><th>Shares</th><th>' + POOL_T.lastAct + '</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
      var now = new Date();
      document.getElementById('pool-refresh-ts').textContent = POOL_T.lastUpd + ' ' + now.toLocaleTimeString();
    })
    .catch(function() {
      document.getElementById('pool-workers-container').innerHTML =
        '<div class="pool-error">' + POOL_T.poolErr + '</div>';
    });
}

function loadBlocks() {
  fetch(POOL_API + '/api/proxy/pool/blocks')
    .then(function(r){ return r.json(); })
    .then(function(blocks) {
      var c = document.getElementById('pool-blocks-container');
      if (!blocks || !blocks.length) {
        c.innerHTML = '<div class="pool-loading">' + POOL_T.noBlocks + '</div>';
        return;
      }
      var rows = blocks.slice(0,20).map(function(b) {
        var addr = b.FoundBy || '';
        var addrShort = addr ? addr.slice(0,10)+'...'+addr.slice(-6) : '—';
        return '<tr>' +
          '<td class="blue">#' + (b.BlockIndex || '?') + '</td>' +
          '<td class="mono">' + shortHash(b.BlockHash || '') + '</td>' +
          '<td class="mono">' + addrShort + '</td>' +
          '<td class="green">' + (b.NetReward || b.Reward || 11) + ' SCO</td>' +
          '<td class="muted">' + fmtTime(b.Timestamp || '') + '</td>' +
        '</tr>';
      }).join('');
      c.innerHTML = '<table class="pool-table"><thead><tr>' +
        '<th>Bloc</th><th>Hash</th><th>' + POOL_T.miner + '</th><th>' + POOL_T.reward + '</th><th>Date</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
    })
    .catch(function() {
      document.getElementById('pool-blocks-container').innerHTML =
        '<div class="pool-error">' + POOL_T.blocksErr + '</div>';
    });
}

function refreshAll() {
  loadStats();
  loadWorkers();
  loadBlocks();
}

refreshAll();
setInterval(refreshAll, 30000);

function updateCountdown() {
  var now = new Date();
  var totalSeconds = now.getHours() * 3600 + now.getMinutes() * 60 + now.getSeconds();
  var cycleSeconds = 12 * 3600;
  var nextPayout = cycleSeconds - (totalSeconds % cycleSeconds);
  var h = Math.floor(nextPayout / 3600);
  var m = Math.floor((nextPayout % 3600) / 60);
  var s = nextPayout % 60;
  var el = document.getElementById('ps-countdown');
  if (el) el.textContent = String(h).padStart(2,'0') + ':' + String(m).padStart(2,'0') + ':' + String(s).padStart(2,'0');
}
updateCountdown();
setInterval(updateCountdown, 1000);
</script>
`
}

// ─── Pool proxy handlers (relay browser requests to localhost:3334) ────────────

const poolBackend = "http://localhost:3334"

func proxyPoolEndpoint(w http.ResponseWriter, target string) {
	resp, err := http.Get(target)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		fmt.Fprintf(w, `{"error":"pool unreachable"}`)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (e *Explorer) apiProxyPoolStats(w http.ResponseWriter, r *http.Request) {
	proxyPoolEndpoint(w, poolBackend+"/api/pool/stats")
}

func (e *Explorer) apiProxyPoolWorkers(w http.ResponseWriter, r *http.Request) {
	proxyPoolEndpoint(w, poolBackend+"/api/pool/workers")
}

func (e *Explorer) apiProxyPoolBlocks(w http.ResponseWriter, r *http.Request) {
	proxyPoolEndpoint(w, poolBackend+"/api/pool/blocks")
}

func (e *Explorer) apiProxyPoolBalance(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	proxyPoolEndpoint(w, poolBackend+"/api/pool/balance?address="+address)
}
