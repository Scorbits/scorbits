// cmd/pool/main.go — Scorbits Stratum Mining Pool Server
// Protocole : Stratum TCP :3333  |  Stats API : :3334
// Deploy : go build -o pool ./cmd/pool && sudo systemctl restart scorbits-pool
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ═══════════════════════════════════════════════════════════════════
//  CONFIGURATION
// ═══════════════════════════════════════════════════════════════════

const (
	StratumPort     = ":3333"
	StatsPort       = ":3334"
	BlockReward     = 11.0         // SCO par bloc
	PoolFee         = 0.02         // 2%
	ShareDifficulty = 6            // difficulté des shares (plus facile que le bloc)
	JobRefreshSec   = 15           // polling template toutes les 15s
	PayoutInterval  = 12 * time.Hour // paiements automatiques toutes les 12 heures
	MinPayout       = 1.0          // minimum 1 SCO pour déclencher un paiement
	MaxWorkerIdle   = 5 * time.Minute
)

var (
	NodeAPI     = env("NODE_API", "http://localhost:8080")
	MongoURI    = env("MONGO_URI", "mongodb+srv://user:pass@cluster3.ukliz8p.mongodb.net")
	PoolAddress = env("POOL_ADDRESS", "SCO_POOL_ADRESSE_ICI") // adresse wallet du pool
	PoolSecret  = env("POOL_SECRET", "")                      // clé secrète pour /api/pool-payout
)

// ═══════════════════════════════════════════════════════════════════
//  PROTOCOLE STRATUM  (JSON-RPC over TCP, line-delimited)
// ═══════════════════════════════════════════════════════════════════

type StratumReq struct {
	ID     interface{}   `json:"id"`
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
}

type StratumResp struct {
	ID     interface{} `json:"id"`
	Result interface{} `json:"result"`
	Error  interface{} `json:"error"`
}

type StratumNotif struct {
	ID     interface{} `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

// ═══════════════════════════════════════════════════════════════════
//  JOB  (modèle de bloc à miner)
// ═══════════════════════════════════════════════════════════════════

type Job struct {
	ID           string
	BlockIndex   int
	PrevHash     string
	Transactions []string  // tableau de strings, tel que le blockchain les attend
	TxData       string    // strings.Join(Transactions, ";") — pré-calculé pour le hash
	Timestamp    int64
	Difficulty   int
	Target       *big.Int  // target bloc
	ShareTarget  *big.Int  // target share (plus facile)
	CreatedAt    time.Time
}

// ─── Block template renvoyé par le nœud ───────────────────────────

type BlockTemplate struct {
	Index        int      `json:"index"`
	PreviousHash string   `json:"previousHash"`
	Transactions []string `json:"transactions"` // tableau de strings
	Difficulty   int      `json:"difficulty"`
}

// ─── Réponse intermédiaire du nœud (transactions peut être string ou []string) ──

type blockTemplateRaw struct {
	Index        int             `json:"index"`
	PreviousHash string          `json:"previousHash"`
	Transactions json.RawMessage `json:"transactions"`
	Difficulty   int             `json:"difficulty"`
}

// ═══════════════════════════════════════════════════════════════════
//  WORKER  (connexion TCP d'un mineur)
// ═══════════════════════════════════════════════════════════════════

type Worker struct {
	mu           sync.Mutex
	conn         net.Conn
	reader       *bufio.Reader
	id           string // extraNonce1 unique (8 hex chars = int32)
	nonceBase    int64  // extraNonce1 parsé en int32, décalé de 32 bits
	address      string // adresse SCO du mineur
	workerName   string
	authorized   bool
	currentJobID string
	sharesValid  int64
	sharesTotal  int64
	lastShare   time.Time
	connectedAt time.Time
	hashrate    float64
}

func (w *Worker) send(v interface{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, _ := json.Marshal(v)
	data = append(data, '\n')
	w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	w.conn.Write(data)
}

func (w *Worker) respond(id, result, errVal interface{}) {
	w.send(StratumResp{ID: id, Result: result, Error: errVal})
}

func (w *Worker) notify(method string, params interface{}) {
	w.send(StratumNotif{ID: nil, Method: method, Params: params})
}

// ═══════════════════════════════════════════════════════════════════
//  MONGODB — schémas
// ═══════════════════════════════════════════════════════════════════

type Share struct {
	ID            bson.ObjectID `bson:"_id,omitempty"`
	WorkerAddress string        `bson:"worker_address"`
	WorkerName    string        `bson:"worker_name"`
	JobID         string        `bson:"job_id"`
	BlockIndex    int           `bson:"block_index"`
	Nonce         int64         `bson:"nonce"`
	Hash          string        `bson:"hash"`
	IsBlock       bool          `bson:"is_block"`
	Paid          bool          `bson:"paid"`
	Timestamp     time.Time     `bson:"timestamp"`
}

type MinerBalance struct {
	ID          bson.ObjectID `bson:"_id,omitempty"`
	Address     string        `bson:"address"`
	Pending     float64       `bson:"pending"`
	TotalEarned float64       `bson:"total_earned"`
	TotalPaid   float64       `bson:"total_paid"`
	UpdatedAt   time.Time     `bson:"updated_at"`
}

type FoundBlock struct {
	ID          bson.ObjectID `bson:"_id,omitempty"`
	BlockIndex  int           `bson:"block_index"`
	BlockHash   string        `bson:"block_hash"`
	Nonce       int64         `bson:"nonce"`
	FoundBy     string        `bson:"found_by"`
	Reward      float64       `bson:"reward"`
	FeeAmount   float64       `bson:"fee_amount"`
	NetReward   float64       `bson:"net_reward"`
	Distributed bool          `bson:"distributed"`
	Timestamp   time.Time     `bson:"timestamp"`
}

// ═══════════════════════════════════════════════════════════════════
//  POOL STATE
// ═══════════════════════════════════════════════════════════════════

type Pool struct {
	mu          sync.RWMutex
	currentJob  *Job
	previousJob *Job
	workers     map[string]*Worker // workerID → Worker
	startedAt   time.Time
	totalShares int64 // atomic
	blocksFound int64 // atomic
	dbShares    *mongo.Collection
	dbBalances  *mongo.Collection
	dbBlocks    *mongo.Collection
}

var P = &Pool{
	workers:   make(map[string]*Worker),
	startedAt: time.Now(),
}

// ═══════════════════════════════════════════════════════════════════
//  CRYPTO HELPERS
// ═══════════════════════════════════════════════════════════════════

// computeHash : même format que blockchain/block.go
// SHA256(fmt.Sprintf("%d%d%s%s%d%s", index, timestamp, txData, prevHash, nonce, address))
// txData = strings.Join(transactions, ";")
func computeHash(index int, timestamp int64, txs []string, prevHash string, nonce int64, address string) string {
	txData := strings.Join(txs, ";")
	raw := fmt.Sprintf("%d%d%s%s%d%s", index, timestamp, txData, prevHash, nonce, address)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// difficultyToTarget : N zéros hex → target = 0x00...0FFFF...
func difficultyToTarget(difficulty int) *big.Int {
	bits := 256 - difficulty*4
	if bits <= 0 {
		return big.NewInt(0)
	}
	t := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	t.Sub(t, big.NewInt(1))
	return t
}

func meetsTarget(hashHex string, target *big.Int) bool {
	h := new(big.Int)
	h.SetString(hashHex, 16)
	return h.Cmp(target) <= 0
}


func newID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ═══════════════════════════════════════════════════════════════════
//  MONGODB — init & helpers
// ═══════════════════════════════════════════════════════════════════

func initMongo() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(MongoURI))
	if err != nil {
		log.Fatalf("MongoDB connect: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("MongoDB ping: %v", err)
	}

	db := client.Database("scorbits_pool")
	P.dbShares = db.Collection("shares")
	P.dbBalances = db.Collection("balances")
	P.dbBlocks = db.Collection("blocks")

	// Index shares : worker + block_index
	P.dbShares.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "worker_address", Value: 1}, {Key: "block_index", Value: 1}},
	})
	// Index balances : address unique
	P.dbBalances.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "address", Value: 1}},
		Options: options.Index().SetUnique(true),
	})

	log.Println("✅ MongoDB connecté")
}

func dbSaveShare(s Share) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.ID = bson.NewObjectID()
	P.dbShares.InsertOne(ctx, s)
}

func dbAddBalance(address string, amount float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	filter := bson.M{"address": address}
	update := bson.M{
		"$inc": bson.M{"pending": amount, "total_earned": amount},
		"$set": bson.M{"updated_at": time.Now()},
		"$setOnInsert": bson.M{"address": address, "total_paid": 0.0},
	}
	P.dbBalances.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
}

func dbSaveBlock(b FoundBlock) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.ID = bson.NewObjectID()
	P.dbBlocks.InsertOne(ctx, b)
}

// ═══════════════════════════════════════════════════════════════════
//  BLOCK TEMPLATE — polling du nœud
// ═══════════════════════════════════════════════════════════════════

func fetchTemplate() (*Job, error) {
	resp, err := http.Get(NodeAPI + "/api/block-template")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("node retourné %d", resp.StatusCode)
	}

	var raw blockTemplateRaw
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	// Désérialiser transactions : accepte []string ou string (JSON encodé)
	var txStrings []string
	if err := json.Unmarshal(raw.Transactions, &txStrings); err != nil {
		// Si c'est un string JSON (double-encodé), on décode une deuxième fois
		var txStr string
		if err2 := json.Unmarshal(raw.Transactions, &txStr); err2 == nil {
			json.Unmarshal([]byte(txStr), &txStrings)
		}
	}
	if len(txStrings) == 0 {
		txStrings = []string{"empty-block"}
	}

	job := &Job{
		ID:           newID(),
		BlockIndex:   raw.Index,
		PrevHash:     raw.PreviousHash,
		Transactions: txStrings,
		TxData:       strings.Join(txStrings, ";"),
		Timestamp:    time.Now().Unix(),
		Difficulty:   raw.Difficulty,
		Target:       difficultyToTarget(raw.Difficulty),
		ShareTarget:  difficultyToTarget(ShareDifficulty),
		CreatedAt:    time.Now(),
	}
	return job, nil
}

func jobUpdater() {
	for {
		newJob, err := fetchTemplate()
		if err != nil {
			log.Printf("⚠️  block-template: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}

		P.mu.Lock()
		if P.currentJob == nil ||
			P.currentJob.BlockIndex != newJob.BlockIndex ||
			P.currentJob.PrevHash != newJob.PrevHash {
			// Nouveau bloc : sauvegarde l'ancien job pour la grace period, puis broadcast
			P.previousJob = P.currentJob
			P.currentJob = newJob
			P.mu.Unlock()
			log.Printf("📋 Nouveau job: bloc #%d  diff=%d", newJob.BlockIndex, newJob.Difficulty)
			broadcastJob(newJob, true)
		} else {
			// Même bloc : ne rien changer — les miners hashent avec les params du notify original
			// Modifier Timestamp ou Transactions ici causerait des low-diff (hash mismatch)
			P.mu.Unlock()
		}

		time.Sleep(JobRefreshSec * time.Second)
	}
}

// ═══════════════════════════════════════════════════════════════════
//  STRATUM SERVER
// ═══════════════════════════════════════════════════════════════════

func startStratum() {
	ln, err := net.Listen("tcp", StratumPort)
	if err != nil {
		log.Fatalf("❌ Stratum listen %s: %v", StratumPort, err)
	}
	log.Printf("⛏️  Stratum server prêt sur %s", StratumPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	// Générer un extranonce1 unique (8 hex chars = int32)
	rawID := newID()
	extranonce1 := rawID[:8]
	e1, _ := strconv.ParseInt(extranonce1, 16, 64)

	w := &Worker{
		conn:        conn,
		reader:      bufio.NewReader(conn),
		id:          extranonce1,
		nonceBase:   e1 << 32,
		connectedAt: time.Now(),
	}

	P.mu.Lock()
	P.workers[w.id] = w
	P.mu.Unlock()

	log.Printf("🔌 Connexion: %s", conn.RemoteAddr())

	defer func() {
		conn.Close()
		P.mu.Lock()
		delete(P.workers, w.id)
		P.mu.Unlock()
		if w.address != "" {
			log.Printf("❌ Déconnexion: %s (%s)", w.address, conn.RemoteAddr())
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(MaxWorkerIdle))
		line, err := w.reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var req StratumReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			log.Printf("Parse error %s: %v", conn.RemoteAddr(), err)
			continue
		}
		dispatch(w, req)
	}
}

func dispatch(w *Worker, req StratumReq) {
	switch req.Method {
	case "mining.subscribe":
		onSubscribe(w, req)
	case "mining.authorize":
		onAuthorize(w, req)
	case "mining.submit":
		onSubmit(w, req)
	case "mining.hashrate":
		onHashrate(w, req)
	case "mining.extranonce.subscribe":
		w.respond(req.ID, true, nil)
	default:
		w.respond(req.ID, nil, []interface{}{20, "Unknown method", nil})
	}
}

// ─── mining.hashrate ──────────────────────────────────────────────

func onHashrate(w *Worker, req StratumReq) {
	if len(req.Params) < 1 {
		return
	}
	hrStr, _ := req.Params[0].(string)
	hrVal, err := strconv.ParseFloat(hrStr, 64)
	if err != nil || hrVal <= 0 {
		return
	}
	log.Printf("📊 Hashrate reçu — worker=%s hashrate=%.0f", w.address, hrVal)
	w.mu.Lock()
	w.hashrate = hrVal
	w.mu.Unlock()
}

// ─── mining.subscribe ─────────────────────────────────────────────

func onSubscribe(w *Worker, req StratumReq) {
	// extranonce2_size = 4 bytes → miner envoie 8 hex chars
	result := []interface{}{
		[]interface{}{
			[]interface{}{"mining.set_difficulty", "diff1"},
			[]interface{}{"mining.notify", "ntf1"},
		},
		w.id, // extranonce1 (8 hex chars = int32)
		4,    // extranonce2_size (4 bytes)
	}
	w.respond(req.ID, result, nil)
	log.Printf("📥 Subscribe: %s (extranonce1=%s)", w.conn.RemoteAddr(), w.id)

	w.notify("mining.set_difficulty", []interface{}{ShareDifficulty})

	P.mu.RLock()
	job := P.currentJob
	P.mu.RUnlock()
	if job != nil {
		sendJob(w, job, true)
	}
}

// ─── mining.authorize ─────────────────────────────────────────────

func onAuthorize(w *Worker, req StratumReq) {
	if len(req.Params) < 1 {
		w.respond(req.ID, false, []interface{}{25, "Missing params", nil})
		return
	}
	workerStr, _ := req.Params[0].(string)
	parts := strings.SplitN(workerStr, ".", 2)
	addr := parts[0]
	name := "default"
	if len(parts) > 1 {
		name = parts[1]
	}

	if !strings.HasPrefix(addr, "SCO") || len(addr) < 15 {
		w.respond(req.ID, false, []interface{}{24, "Adresse SCO invalide", nil})
		return
	}

	w.mu.Lock()
	w.address = addr
	w.workerName = name
	w.authorized = true
	w.mu.Unlock()

	w.respond(req.ID, true, nil)
	log.Printf("✅ Autorisé: %s.%s", addr, name)

	P.mu.RLock()
	job := P.currentJob
	P.mu.RUnlock()
	if job != nil {
		sendJob(w, job, true)
	}
}

// ─── mining.submit ────────────────────────────────────────────────
// Params : [workerName, jobID, extranonce2, ntime, nonce]
// nonce (params[4]) : hex string du int64 exact utilisé par le miner
// On le parse directement — même valeur que le miner a passée à computePoolHash

func onSubmit(w *Worker, req StratumReq) {
	if !w.authorized {
		w.respond(req.ID, false, []interface{}{24, "Non autorisé", nil})
		return
	}
	if len(req.Params) < 5 {
		w.respond(req.ID, false, []interface{}{20, "Params invalides", nil})
		return
	}

	jobID, _ := req.Params[1].(string)
	ntimeHex, _ := req.Params[3].(string)
	nonceHex, _ := req.Params[4].(string)

	// Parse le nonce hex → int64 via uint64 pour gérer les nonces avec le bit haut à 1
	nonceU, _ := strconv.ParseUint(nonceHex, 16, 64)
	nonce := int64(nonceU)

	// Parse le timestamp utilisé par le miner (ntime = params[3], hex)
	ntimeU, _ := strconv.ParseUint(ntimeHex, 16, 64)
	ntime := int64(ntimeU)

	P.mu.RLock()
	job := P.currentJob
	P.mu.RUnlock()

	if job == nil {
		w.respond(req.ID, false, []interface{}{21, "Pas de job", nil})
		return
	}
	if job.ID != jobID {
		P.mu.RLock()
		prevJob := P.previousJob
		P.mu.RUnlock()
		if prevJob != nil && prevJob.ID == jobID && time.Since(prevJob.CreatedAt) <= 60*time.Second {
			job = prevJob
		} else {
			log.Printf("❌ Share périmé — worker=%s jobID reçu=%s jobID actuel=%s", w.address, jobID, job.ID)
			w.respond(req.ID, false, []interface{}{21, "Job périmé", nil})
			return
		}
	}

	// Calculer le hash avec le timestamp et txData exacts du miner (ntime depuis params[3])
	hash := computeHash(job.BlockIndex, ntime, job.Transactions, job.PrevHash, nonce, PoolAddress)

	// Incrémenter compteur global (tous les submits)
	atomic.AddInt64(&P.totalShares, 1)

	// Vérifier la difficulté share
	if !meetsTarget(hash, job.ShareTarget) {
		log.Printf("❌ Share low-diff — worker=%s hash=%s target=%s", w.address, hash[:16], strings.Repeat("0", ShareDifficulty))
		w.respond(req.ID, false, []interface{}{23, "Difficulté trop faible", nil})
		return
	}

	// ✅ Share valide
	now := time.Now()
	w.mu.Lock()
	w.sharesValid++
	w.lastShare = now
	atomic.AddInt64(&w.sharesTotal, 1)
	w.mu.Unlock()

	log.Printf("✅ Share valide — %s.%s  hash=%s…", w.address, w.workerName, hash[:16])

	go dbSaveShare(Share{
		WorkerAddress: w.address,
		WorkerName:    w.workerName,
		JobID:         jobID,
		BlockIndex:    job.BlockIndex,
		Nonce:         nonce,
		Hash:          hash,
		IsBlock:       false,
		Timestamp:     time.Now(),
	})

	// Le hash atteint-il la difficulté du bloc ?
	if meetsTarget(hash, job.Target) {
		log.Printf("🎉🎉🎉 BLOC TROUVÉ par %s ! Bloc #%d Hash=%s", w.address, job.BlockIndex, hash[:32])
		go onBlockFound(job, nonce, ntime, hash, w.address)
	}

	w.respond(req.ID, true, nil)
}

// ─── Bloc trouvé : soumettre + distribuer ─────────────────────────

func onBlockFound(job *Job, nonce, timestamp int64, hash, finderAddr string) {
	// Soumettre au nœud via /api/submit-block
	// Utilise le timestamp exact que le miner a utilisé pour calculer le hash
	payload := map[string]interface{}{
		"index":        job.BlockIndex,
		"previousHash": job.PrevHash,
		"transactions": job.Transactions, // []string
		"timestamp":    timestamp,        // ntime exact du miner
		"nonce":        nonce,            // int64
		"hash":         hash,
		"difficulty":   job.Difficulty,
		"minerAddress": PoolAddress,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(NodeAPI+"/api/submit-block", "application/json", strings.NewReader(string(body)))
	if err != nil {
		log.Printf("⚠️  Soumission bloc échouée: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("⚠️  Soumission bloc échouée: status %d — %s", resp.StatusCode, string(b))
		return
	}

	atomic.AddInt64(&P.blocksFound, 1)

	fee := BlockReward * PoolFee
	netReward := BlockReward - fee

	dbSaveBlock(FoundBlock{
		BlockIndex:  job.BlockIndex,
		BlockHash:   hash,
		Nonce:       nonce,
		FoundBy:     finderAddr,
		Reward:      BlockReward,
		FeeAmount:   fee,
		NetReward:   netReward,
		Distributed: false,
		Timestamp:   time.Now(),
	})

	go distributeReward(job.BlockIndex, netReward)

	// Relancer le job dès que le nouveau bloc est connu
	time.Sleep(3 * time.Second)
	if newJob, err := fetchTemplate(); err == nil {
		P.mu.Lock()
		P.currentJob = newJob
		P.mu.Unlock()
		broadcastJob(newJob, true)
	}
}

// ═══════════════════════════════════════════════════════════════════
//  DISTRIBUTION PROPORTIONNELLE DES RÉCOMPENSES (PPLNS)
// ═══════════════════════════════════════════════════════════════════

func distributeReward(blockIndex int, netReward float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pipeline := []bson.M{
		{"$match": bson.M{"block_index": blockIndex, "is_block": false}},
		{"$group": bson.M{
			"_id":    "$worker_address",
			"shares": bson.M{"$sum": 1},
		}},
	}
	cursor, err := P.dbShares.Aggregate(ctx, pipeline)
	if err != nil {
		log.Printf("❌ Agrégation shares: %v", err)
		return
	}
	defer cursor.Close(ctx)

	type entry struct {
		Address string `bson:"_id"`
		Shares  int    `bson:"shares"`
	}
	var entries []entry
	cursor.All(ctx, &entries)

	total := 0
	for _, e := range entries {
		total += e.Shares
	}
	if total == 0 {
		log.Printf("⚠️  Aucun share pour le bloc #%d — impossible de distribuer", blockIndex)
		return
	}

	log.Printf("💰 Distribution %.4f SCO → %d mineurs (%d shares)", netReward, len(entries), total)
	for _, e := range entries {
		portion := float64(e.Shares) / float64(total) * netReward
		dbAddBalance(e.Address, portion)
		log.Printf("   → %s  %.6f SCO (%d shares)", e.Address, portion, e.Shares)
	}

	P.dbBlocks.UpdateOne(ctx,
		bson.M{"block_index": blockIndex},
		bson.M{"$set": bson.M{"distributed": true}},
	)
}

// ═══════════════════════════════════════════════════════════════════
//  PAIEMENTS AUTOMATIQUES
// ═══════════════════════════════════════════════════════════════════

func payoutLoop() {
	t := time.NewTicker(PayoutInterval)
	for range t.C {
		runPayouts()
	}
}

func runPayouts() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cursor, err := P.dbBalances.Find(ctx, bson.M{"pending": bson.M{"$gte": MinPayout}})
	if err != nil {
		return
	}
	defer cursor.Close(ctx)

	var balances []MinerBalance
	cursor.All(ctx, &balances)

	for _, b := range balances {
		log.Printf("💸 Paiement → %s  %.4f SCO", b.Address, b.Pending)
		if err := sendSCO(b.Address, b.Pending); err != nil {
			log.Printf("⚠️  Paiement échoué %s: %v", b.Address, err)
			continue
		}
		P.dbBalances.UpdateOne(ctx,
			bson.M{"address": b.Address},
			bson.M{
				"$set": bson.M{"pending": 0.0, "updated_at": time.Now()},
				"$inc": bson.M{"total_paid": b.Pending},
			},
		)
	}
}

func sendSCO(toAddress string, amount float64) error {
	payload := map[string]interface{}{
		"to":     toAddress,
		"amount": amount,
		"secret": PoolSecret,
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(NodeAPI+"/api/pool-payout", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("payout API %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════
//  JOB BROADCAST
// ═══════════════════════════════════════════════════════════════════

func sendJob(w *Worker, job *Job, cleanJobs bool) {
	// Params : [job_id, block_index, prevhash, txdata, timestamp_hex, difficulty_hex, pool_address, clean_jobs]
	// block_index et pool_address sont nécessaires pour que le miner calcule le hash correct.
	params := []interface{}{
		job.ID,
		job.BlockIndex,
		job.PrevHash,
		job.TxData,
		fmt.Sprintf("%x", job.Timestamp),
		fmt.Sprintf("%x", job.Difficulty),
		PoolAddress,
		cleanJobs,
	}
	w.notify("mining.notify", params)
	w.mu.Lock()
	w.currentJobID = job.ID
	w.mu.Unlock()
}

func broadcastJob(job *Job, cleanJobs bool) {
	P.mu.RLock()
	workers := make([]*Worker, 0, len(P.workers))
	for _, w := range P.workers {
		workers = append(workers, w)
	}
	P.mu.RUnlock()

	count := 0
	for _, w := range workers {
		if w.authorized {
			sendJob(w, job, cleanJobs)
			count++
		}
	}
	if count > 0 {
		log.Printf("📡 Job diffusé à %d workers", count)
	}
}

// ═══════════════════════════════════════════════════════════════════
//  API STATS  (port :3334 — pour le site web)
// ═══════════════════════════════════════════════════════════════════

func startStatsAPI() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pool/stats", statsHandler)
	mux.HandleFunc("/api/pool/workers", workersHandler)
	mux.HandleFunc("/api/pool/blocks", blocksHandler)
	mux.HandleFunc("/api/pool/balance", balanceHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	withCORS := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Content-Type", "application/json")
			next.ServeHTTP(w, r)
		})
	}

	log.Printf("📊 Stats API sur %s", StatsPort)
	http.ListenAndServe(StatsPort, withCORS(mux))
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	P.mu.RLock()
	job := P.currentJob
	activeWorkers := 0
	totalHashrate := 0.0
	for _, wk := range P.workers {
		if wk.authorized {
			activeWorkers++
			wk.mu.Lock()
			totalHashrate += wk.hashrate
			wk.mu.Unlock()
		}
	}
	P.mu.RUnlock()

	diff, blockIdx := 0, 0
	if job != nil {
		diff = job.Difficulty
		blockIdx = job.BlockIndex
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"workers_online": activeWorkers,
		"pool_hashrate":  totalHashrate,
		"blocks_found":   atomic.LoadInt64(&P.blocksFound),
		"total_shares":   atomic.LoadInt64(&P.totalShares),
		"current_block":  blockIdx,
		"difficulty":     diff,
		"share_diff":     ShareDifficulty,
		"pool_fee_pct":   PoolFee * 100,
		"pool_address":   PoolAddress,
		"min_payout":     MinPayout,
		"uptime_s":       time.Since(P.startedAt).Seconds(),
		"stratum":        "stratum+tcp://pool.scorbits.com:3333",
	})
}

func workersHandler(w http.ResponseWriter, r *http.Request) {
	P.mu.RLock()
	var out []map[string]interface{}
	for _, wk := range P.workers {
		if wk.authorized {
			wk.mu.Lock()
			out = append(out, map[string]interface{}{
				"address":      wk.address,
				"worker":       wk.workerName,
				"shares_valid": wk.sharesValid,
				"shares_total": wk.sharesTotal,
				"hashrate":     wk.hashrate,
				"last_share":   wk.lastShare,
				"connected_at": wk.connectedAt,
			})
			wk.mu.Unlock()
		}
	}
	P.mu.RUnlock()
	json.NewEncoder(w).Encode(out)
}

func blocksHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "timestamp", Value: -1}}).SetLimit(20)
	cursor, err := P.dbBlocks.Find(ctx, bson.M{}, opts)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer cursor.Close(ctx)
	var blocks []FoundBlock
	cursor.All(ctx, &blocks)
	json.NewEncoder(w).Encode(blocks)
}

func balanceHandler(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("address")
	if addr == "" {
		http.Error(w, `{"error":"address requis"}`, 400)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var b MinerBalance
	err := P.dbBalances.FindOne(ctx, bson.M{"address": addr}).Decode(&b)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"address":      addr,
			"pending":      0.0,
			"total_earned": 0.0,
			"total_paid":   0.0,
		})
		return
	}
	json.NewEncoder(w).Encode(b)
}

// ═══════════════════════════════════════════════════════════════════
//  MAIN
// ═══════════════════════════════════════════════════════════════════

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("🚀 Scorbits Mining Pool — démarrage...")

	initMongo()

	if job, err := fetchTemplate(); err == nil {
		P.mu.Lock()
		P.currentJob = job
		P.mu.Unlock()
		log.Printf("📋 Job initial: bloc #%d  diff=%d", job.BlockIndex, job.Difficulty)
	} else {
		log.Printf("⚠️  Impossible de récupérer le template initial: %v", err)
	}

	go jobUpdater()
	go payoutLoop()
	go startStatsAPI()

	startStratum()
}
