package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	flagAddress = flag.String("address", "", "Your SCO address (SCOxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx)")
	flagNode    = flag.String("node", "https://scorbits.com", "Node URL (e.g. https://scorbits.com)")
	flagThreads = flag.Int("threads", 1, "Number of mining threads")
	flagPool    = flag.String("pool", "", "Pool stratum URL (e.g. stratum+tcp://pool.scorbits.com:3333)")
	flagWorker  = flag.String("worker", "worker1", "Worker name for pool mode")
)

// ═══════════════════════════════════════════════════════════════════
//  SOLO — types & helpers (inchangés)
// ═══════════════════════════════════════════════════════════════════

type WorkResponse struct {
	BlockIndex    int      `json:"block_index"`
	PreviousHash  string   `json:"previous_hash"`
	Difficulty    int      `json:"difficulty"`
	Reward        int      `json:"reward"`
	Timestamp     int64    `json:"timestamp"`
	LastTimestamp int64    `json:"last_timestamp"`
	Transactions  []string `json:"transactions"`
}

type SubmitRequest struct {
	BlockIndex   int      `json:"block_index"`
	Nonce        int      `json:"nonce"`
	Hash         string   `json:"hash"`
	MinerAddress string   `json:"miner_address"`
	Timestamp    int64    `json:"timestamp"`
	Transactions []string `json:"transactions"`
}

type SubmitResponse struct {
	Success    bool   `json:"success"`
	BlockIndex int    `json:"block_index"`
	Reward     int    `json:"reward"`
	Hash       string `json:"hash"`
	Error      string `json:"error"`
}

func calculateHash(index int, timestamp int64, transactions []string, previousHash string, nonce int, minerAddress string) string {
	txData := strings.Join(transactions, ";")
	input := fmt.Sprintf("%d%d%s%s%d%s", index, timestamp, txData, previousHash, nonce, minerAddress)
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])
}

func fetchWork(node string) (*WorkResponse, error) {
	resp, err := http.Get(node + "/mining/work?address=" + *flagAddress)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var work WorkResponse
	if err := json.NewDecoder(resp.Body).Decode(&work); err != nil {
		return nil, err
	}
	return &work, nil
}

func submitBlock(node string, req SubmitRequest) (*SubmitResponse, int, error) {
	body, _ := json.Marshal(req)
	resp, err := http.Post(node+"/mining/submit", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var sr SubmitResponse
	json.NewDecoder(resp.Body).Decode(&sr)
	return &sr, resp.StatusCode, nil
}

// ═══════════════════════════════════════════════════════════════════
//  POOL — types & helpers
// ═══════════════════════════════════════════════════════════════════

// stratumMsg représente un message JSON-RPC Stratum
type stratumMsg struct {
	ID     interface{}     `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  interface{}     `json:"error"`
	Params json.RawMessage `json:"params"`
}

// poolJob contient les données d'un job reçu via mining.notify
type poolJob struct {
	id          string
	blockIndex  int
	prevHash    string
	txData      string
	timestamp   int64
	difficulty  int
	shareDiff   int
	poolAddress string
	extranonce1 string // 8 hex chars, fourni par le serveur au subscribe
}

// mineResult est envoyé par un thread mineur quand un share est trouvé
type mineResult struct {
	job         *poolJob
	extranonce2 string // 8 hex chars
	nonce       int64  // nonce blockchain = (extranonce1 << 32) | extranonce2
	hash        string
}

// extractPoolHost : "stratum+tcp://host:port" → "host:port"
func extractPoolHost(poolURL string) string {
	s := strings.TrimPrefix(poolURL, "stratum+tcp://")
	s = strings.TrimPrefix(s, "stratum://")
	return s
}

// buildNonce : nonce blockchain = (extranonce1_uint32 << 32) | extranonce2_uint32
func buildNonce(extranonce1Hex, extranonce2Hex string) int64 {
	e1, _ := strconv.ParseUint(extranonce1Hex, 16, 64)
	e2, _ := strconv.ParseUint(extranonce2Hex, 16, 64)
	return int64((e1 << 32) | (e2 & 0xFFFFFFFF))
}

// computePoolHash : même format exact que blockchain/block.go
// SHA256(fmt.Sprintf("%d%d%s%s%d%s", index, ts, txData, prevHash, nonce, poolAddress))
func computePoolHash(index int, timestamp int64, txData, prevHash string, nonce int64, poolAddress string) string {
	input := fmt.Sprintf("%d%d%s%s%d%s", index, timestamp, txData, prevHash, nonce, poolAddress)
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])
}

// sendStratum envoie un message JSON-RPC sur la connexion TCP
func sendStratum(conn net.Conn, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(data, '\n'))
	return err
}

// parseNotify parse les params d'un mining.notify
// Format attendu : [job_id, block_index, prevhash, txdata, timestamp_hex, difficulty_hex, pool_address, clean_jobs]
func parseNotify(raw json.RawMessage, extranonce1 string, shareDiff int) *poolJob {
	var params []json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil || len(params) < 8 {
		return nil
	}
	var jobID, prevHash, txData, tsHex, diffHex, poolAddress string
	var blockIndex int
	json.Unmarshal(params[0], &jobID)
	json.Unmarshal(params[1], &blockIndex)
	json.Unmarshal(params[2], &prevHash)
	json.Unmarshal(params[3], &txData)
	json.Unmarshal(params[4], &tsHex)
	json.Unmarshal(params[5], &diffHex)
	json.Unmarshal(params[6], &poolAddress)

	ts, _ := strconv.ParseInt(tsHex, 16, 64)
	diff64, _ := strconv.ParseInt(diffHex, 16, 64)
	diff := int(diff64)
	if diff <= 0 {
		diff = 1
	}

	return &poolJob{
		id:          jobID,
		blockIndex:  blockIndex,
		prevHash:    prevHash,
		txData:      txData,
		timestamp:   ts,
		difficulty:  diff,
		shareDiff:   shareDiff,
		poolAddress: poolAddress,
		extranonce1: extranonce1,
	}
}

// ═══════════════════════════════════════════════════════════════════
//  STATUS LINE (partagé solo/pool)
// ═══════════════════════════════════════════════════════════════════

var statusLine atomic.Value // string

// ═══════════════════════════════════════════════════════════════════
//  POOL — session
// ═══════════════════════════════════════════════════════════════════

func runPoolSession(conn net.Conn, address, workerName string, threads int,
	hashCounter, totalShares *int64, stop chan struct{}) error {

	reader := bufio.NewReader(conn)
	workerAuth := address + "." + workerName

	extranonce1 := ""
	shareDiff := 6 // défaut, mis à jour par mining.set_difficulty

	var (
		jobMu      sync.Mutex
		currentJob *poolJob
		mineStop   chan struct{}
		mineWg     sync.WaitGroup
		submitCh   = make(chan mineResult, threads*4)
	)

	// ─── goroutine de soumission des shares (séquentielle) ──────────
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		for result := range submitCh {
			jobMu.Lock()
			isOK := currentJob != nil && currentJob.id == result.job.id
			jobMu.Unlock()
			if !isOK {
				continue
			}
			err := sendStratum(conn, map[string]interface{}{
				"id":     4,
				"method": "mining.submit",
				"params": []interface{}{
					workerAuth,
					result.job.id,
					result.extranonce2,
					fmt.Sprintf("%x", result.job.timestamp),
					fmt.Sprintf("%x", result.nonce),
				},
			})
			if err == nil {
				atomic.AddInt64(totalShares, 1)
				fmt.Printf("\n  [%s]  ✅ Share soumis — hash=%s…  e2=%s",
					time.Now().Format("15:04:05"), result.hash[:16], result.extranonce2)
			}
		}
	}()

	stopMining := func() {
		if mineStop != nil {
			select {
			case <-mineStop:
			default:
				close(mineStop)
			}
			mineWg.Wait()
			mineStop = nil
		}
	}

	startMining := func(job *poolJob) {
		stopMining()
		mineStop = make(chan struct{})
		ms := mineStop
		shareTarget := strings.Repeat("0", job.shareDiff)

		statusLine.Store(fmt.Sprintf("[POOL] Mining bloc #%d (diff %d, share %d)",
			job.blockIndex, job.difficulty, job.shareDiff))

		for t := 0; t < threads; t++ {
			mineWg.Add(1)
			// Chaque thread commence à un offset différent dans l'espace extranonce2
			startE2 := uint32(rand.Int31()) + uint32(t)*uint32(0xFFFFFFFF/max(threads, 1))
			go func(startE2 uint32, ms chan struct{}) {
				defer mineWg.Done()
				e2 := startE2
				for {
					select {
					case <-ms:
						return
					default:
					}
					e2Hex := fmt.Sprintf("%08x", e2)
					nonce := buildNonce(job.extranonce1, e2Hex)
					hash := computePoolHash(job.blockIndex, job.timestamp,
						job.txData, job.prevHash, nonce, job.poolAddress)
					atomic.AddInt64(hashCounter, 1)

					if strings.HasPrefix(hash, shareTarget) {
						r := mineResult{job: job, extranonce2: e2Hex, nonce: nonce, hash: hash}
						select {
						case submitCh <- r:
						case <-ms:
							return
						default:
							// channel plein : share ignoré, on continue
						}
					}
					e2++
				}
			}(startE2, ms)
		}
	}

	defer func() {
		stopMining()
		close(submitCh)
		<-submitDone
	}()

	// ─── envoi subscribe + authorize ────────────────────────────────
	if err := sendStratum(conn, map[string]interface{}{
		"id":     1,
		"method": "mining.subscribe",
		"params": []interface{}{"scorbits-miner/1.0"},
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	if err := sendStratum(conn, map[string]interface{}{
		"id":     2,
		"method": "mining.authorize",
		"params": []interface{}{workerAuth, "x"},
	}); err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	// ─── boucle de lecture ───────────────────────────────────────────
	msgCh := make(chan stratumMsg, 32)
	readErr := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				readErr <- err
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var msg stratumMsg
			if json.Unmarshal([]byte(line), &msg) == nil {
				select {
				case msgCh <- msg:
				default:
				}
			}
		}
	}()

	for {
		select {
		case <-stop:
			return nil

		case err := <-readErr:
			return err

		case msg := <-msgCh:
			if msg.Method != "" {
				// Notification du pool
				switch msg.Method {
				case "mining.set_difficulty":
					var raw []json.RawMessage
					if json.Unmarshal(msg.Params, &raw) == nil && len(raw) > 0 {
						var d float64
						if json.Unmarshal(raw[0], &d) == nil && d > 0 {
							shareDiff = int(d)
						}
					}

				case "mining.notify":
					if extranonce1 == "" {
						continue
					}
					job := parseNotify(msg.Params, extranonce1, shareDiff)
					if job == nil {
						continue
					}
					jobMu.Lock()
					currentJob = job
					jobMu.Unlock()
					startMining(job)
				}
			} else if msg.ID != nil {
				// Réponse à nos requêtes
				idFloat, ok := msg.ID.(float64)
				if !ok {
					continue
				}
				switch int(idFloat) {
				case 1: // subscribe response
					if msg.Result != nil && msg.Error == nil {
						var result []json.RawMessage
						if json.Unmarshal(msg.Result, &result) == nil && len(result) >= 2 {
							json.Unmarshal(result[1], &extranonce1)
							fmt.Printf("\n  [POOL] Souscrit — extranonce1=%s", extranonce1)
							statusLine.Store(fmt.Sprintf("[POOL] Souscrit (extranonce1=%s)", extranonce1))
						}
					}
				case 2: // authorize response
					var authOK bool
					if json.Unmarshal(msg.Result, &authOK) == nil {
						if authOK {
							fmt.Printf("\n  [POOL] ✅ Autorisé: %s", workerAuth)
						} else {
							fmt.Printf("\n  [POOL] ❌ Autorisation refusée: %s", workerAuth)
						}
					}
				case 4: // submit response
					var shareOK bool
					if json.Unmarshal(msg.Result, &shareOK) == nil && !shareOK {
						fmt.Printf("\n  [POOL] ❌ Share rejeté")
					}
				}
			}
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ═══════════════════════════════════════════════════════════════════
//  POOL — boucle principale avec reconnexion
// ═══════════════════════════════════════════════════════════════════

func minePool(poolURL, address, workerName string, threads int) {
	host := extractPoolHost(poolURL)

	fmt.Printf("\n  Scorbits Miner\n")
	fmt.Printf("  [POOL] Connexion à %s\n", poolURL)
	fmt.Printf("  Address : %s\n", address)
	fmt.Printf("  Worker  : %s.%s\n", address, workerName)
	fmt.Printf("  Threads : %d\n\n", threads)

	statusLine.Store("[POOL] Démarrage...")

	// Arrêt propre
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	stop := make(chan struct{})
	go func() {
		<-sigCh
		fmt.Println("\n  Stopping miner...")
		close(stop)
	}()

	var hashCounter int64
	var totalShares int64

	// Affichage hashrate
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var prevHash int64
		prevTime := time.Now()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				cur := atomic.LoadInt64(&hashCounter)
				elapsed := now.Sub(prevTime).Seconds()
				var rate float64
				if elapsed > 0 {
					rate = float64(cur-prevHash) / elapsed
				}
				prevHash = cur
				prevTime = now
				shares := atomic.LoadInt64(&totalShares)
				status := statusLine.Load().(string)
				fmt.Printf("\n  [%s]  %s  |  Hashrate: %s  |  Shares: %d",
					now.Format("15:04:05"), status, formatHashrate(rate), shares)
			case <-stop:
				return
			}
		}
	}()

	for {
		select {
		case <-stop:
			fmt.Println()
			return
		default:
		}

		statusLine.Store(fmt.Sprintf("[POOL] Connexion à %s...", host))
		conn, err := net.DialTimeout("tcp", host, 15*time.Second)
		if err != nil {
			statusLine.Store(fmt.Sprintf("[POOL] Erreur connexion: %v — retry 10s", err))
			select {
			case <-time.After(10 * time.Second):
				continue
			case <-stop:
				fmt.Println()
				return
			}
		}

		sessionErr := runPoolSession(conn, address, workerName, threads, &hashCounter, &totalShares, stop)
		conn.Close()

		select {
		case <-stop:
			fmt.Println()
			return
		default:
		}

		if sessionErr != nil {
			statusLine.Store(fmt.Sprintf("[POOL] Déconnecté: %v — retry 10s", sessionErr))
		} else {
			statusLine.Store("[POOL] Déconnecté — retry 10s")
		}
		select {
		case <-time.After(10 * time.Second):
		case <-stop:
			fmt.Println()
			return
		}
	}
}

// ═══════════════════════════════════════════════════════════════════
//  SOLO — boucle principale (inchangée)
// ═══════════════════════════════════════════════════════════════════

func mineSolo(node string, threads int) {
	fmt.Printf("\n  Scorbits Miner\n")
	fmt.Printf("  [SOLO] Connexion à %s\n", node)
	fmt.Printf("  Address : %s\n", *flagAddress)
	fmt.Printf("  Threads : %d\n\n", threads)

	statusLine.Store("Starting...")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	stop := make(chan struct{})
	go func() {
		<-sigCh
		fmt.Println("\n  Stopping miner...")
		close(stop)
	}()

	var hashCounter int64
	var totalBlocks int64

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var prevHash int64
		var prevTime time.Time = time.Now()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				cur := atomic.LoadInt64(&hashCounter)
				elapsed := now.Sub(prevTime).Seconds()
				var rate float64
				if elapsed > 0 {
					rate = float64(cur-prevHash) / elapsed
				}
				prevHash = cur
				prevTime = now
				blocks := atomic.LoadInt64(&totalBlocks)
				status := statusLine.Load().(string)
				fmt.Printf("\n  [%s]  %s  |  Hashrate: %s  |  Blocks: %d",
					now.Format("15:04:05"),
					status,
					formatHashrate(rate),
					blocks,
				)
			case <-stop:
				return
			}
		}
	}()

	var lastSubmitTs int64
	var lastAcceptedTs int64

	for {
		select {
		case <-stop:
			fmt.Println()
			return
		default:
		}

		statusLine.Store("Fetching job...")
		work, err := fetchWork(node)
		if err != nil {
			statusLine.Store(fmt.Sprintf("Network error: %v", err))
			select {
			case <-time.After(5 * time.Second):
				continue
			case <-stop:
				fmt.Println()
				return
			}
		}
		if work.LastTimestamp > lastAcceptedTs {
			lastAcceptedTs = work.LastTimestamp
		}

		target := strings.Repeat("0", work.Difficulty)
		found := make(chan SubmitRequest, threads)
		mineStop := make(chan struct{})
		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-mineStop:
					return
				case <-ticker.C:
					newWork, err := fetchWork(node)
					if err == nil && newWork.BlockIndex != work.BlockIndex {
						select {
						case <-mineStop:
						default:
							close(mineStop)
						}
						return
					}
				}
			}
		}()

		for t := 0; t < threads; t++ {
			wg.Add(1)
			startNonce := rand.Int() + t*10_000_000
			go func(startNonce int) {
				defer wg.Done()
				if wait := atomic.LoadInt64(&lastAcceptedTs) + 10 - time.Now().Unix(); wait > 0 {
					select {
					case <-mineStop:
						return
					case <-time.After(time.Duration(wait) * time.Second):
					}
				}
				nonce := startNonce
				for {
					select {
					case <-mineStop:
						return
					default:
					}
					ts := time.Now().Unix()
					h := calculateHash(work.BlockIndex, ts, work.Transactions, work.PreviousHash, nonce, *flagAddress)
					atomic.AddInt64(&hashCounter, 1)
					if strings.HasPrefix(h, target) {
						req := SubmitRequest{
							BlockIndex:   work.BlockIndex,
							Nonce:        nonce,
							Hash:         h,
							MinerAddress: *flagAddress,
							Timestamp:    ts,
							Transactions: work.Transactions,
						}
						select {
						case found <- req:
						default:
						}
					}
					nonce++
				}
			}(startNonce)
		}

		statusLine.Store(fmt.Sprintf("Mining block #%d (diff %d)", work.BlockIndex, work.Difficulty))

		needNewJob := false
		for !needNewJob {
			select {
			case <-stop:
				select {
				case <-mineStop:
				default:
					close(mineStop)
				}
				wg.Wait()
				fmt.Println()
				return
			case <-mineStop:
				needNewJob = true
				continue
			case req := <-found:
				now := time.Now().Unix()
				if now-lastSubmitTs < 2 {
					continue
				}
				lastSubmitTs = now

				sr, code, err := submitBlock(node, req)
				if err != nil {
					statusLine.Store(fmt.Sprintf("Submit error: %v", err))
					continue
				}
				if sr.Success {
					atomic.AddInt64(&totalBlocks, 1)
					atomic.StoreInt64(&lastAcceptedTs, req.Timestamp)
					fmt.Printf("\n  [%s]  Block #%d accepted! Hash: %s...  Reward: %d SCO\n",
						time.Now().Format("15:04:05"), sr.BlockIndex, req.Hash[:16], sr.Reward)
					statusLine.Store(fmt.Sprintf("Block #%d found!", sr.BlockIndex))
					needNewJob = true
				} else if code == 429 {
					statusLine.Store("Rate limited (429) — backing off 30s...")
					select {
					case <-time.After(30 * time.Second):
					case <-stop:
						select {
						case <-mineStop:
						default:
							close(mineStop)
						}
						wg.Wait()
						fmt.Println()
						return
					}
				} else if code == 409 {
					statusLine.Store("Stale block (409) — fetching new job...")
					needNewJob = true
				} else {
					statusLine.Store(fmt.Sprintf("Rejected: %s — retrying...", sr.Error))
				}
			}
		}

		select {
		case <-mineStop:
		default:
			close(mineStop)
		}
		wg.Wait()
	}
}

// ═══════════════════════════════════════════════════════════════════
//  MAIN
// ═══════════════════════════════════════════════════════════════════

func waitEnter() {
	fmt.Println("Appuyez sur Entrée pour fermer...")
	bufio.NewReader(os.Stdin).ReadString('\n') //nolint
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "\nPANIC: %v\n", r)
			waitEnter()
		}
	}()

	flag.Parse()

	if *flagAddress == "" {
		fmt.Print("Enter your SCO address (SCO...): ")
		fmt.Scan(flagAddress)
	}
	if !strings.HasPrefix(*flagAddress, "SCO") {
		fmt.Println("Error: address must start with SCO")
		os.Exit(1)
	}

	threads := *flagThreads
	if threads < 1 {
		threads = 1
	}

	if *flagPool != "" {
		// Mode pool Stratum
		minePool(*flagPool, *flagAddress, *flagWorker, threads)
	} else {
		// Mode solo (défaut)
		node := strings.TrimRight(*flagNode, "/")
		mineSolo(node, threads)
	}

	fmt.Println("\nMiner arrêté.")
	waitEnter()
}

func formatHashrate(h float64) string {
	switch {
	case h >= 1_000_000:
		return fmt.Sprintf("%.2f MH/s", h/1_000_000)
	case h >= 1_000:
		return fmt.Sprintf("%.2f KH/s", h/1_000)
	default:
		return fmt.Sprintf("%.0f H/s", h)
	}
}
