package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
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
)

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
	resp, err := http.Get(node + "/mining/work")
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

// status is shared between hashrate printer and mining loop for display
var statusLine atomic.Value // string

func main() {
	flag.Parse()

	if *flagAddress == "" {
		fmt.Print("Enter your SCO address (SCO...): ")
		fmt.Scan(flagAddress)
	}
	if !strings.HasPrefix(*flagAddress, "SCO") {
		fmt.Println("Error: address must start with SCO")
		os.Exit(1)
	}

	node := strings.TrimRight(*flagNode, "/")
	threads := *flagThreads
	if threads < 1 {
		threads = 1
	}

	fmt.Printf("\n  Scorbits Miner\n")
	fmt.Printf("  Address : %s\n", *flagAddress)
	fmt.Printf("  Node    : %s\n", node)
	fmt.Printf("  Threads : %d\n\n", threads)

	statusLine.Store("Starting...")

	// Graceful shutdown
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

	// Hashrate printer — runs forever, never exits except on stop
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
				fmt.Printf("\r  [%s]  %s  |  Hashrate: %s  |  Blocks: %d    ",
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

	// lastSubmitTs: unix timestamp of the last submission sent to the server
	// lastAcceptedTs: timestamp of the last accepted block (to enforce anti-spike locally)
	var lastSubmitTs int64
	var lastAcceptedTs int64

	// Main mining loop — never exits except on stop signal
	for {
		select {
		case <-stop:
			fmt.Println()
			return
		default:
		}

		// Fetch job — retry forever on error
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
		// Use last_timestamp from job if available to enforce anti-spike locally
		if work.LastTimestamp > lastAcceptedTs {
			lastAcceptedTs = work.LastTimestamp
		}

		target := strings.Repeat("0", work.Difficulty)
		// found is buffered so goroutines don't block; multiple threads may find simultaneously
		found := make(chan SubmitRequest, threads)
		mineStop := make(chan struct{})
		var wg sync.WaitGroup

		for t := 0; t < threads; t++ {
			wg.Add(1)
			startNonce := rand.Int() + t*10_000_000
			go func(startNonce int) {
				defer wg.Done()
				// Sleep out anti-spike at startup (< 10s since last accepted block)
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
						default: // channel full, discard and keep mining
						}
						// DO NOT return — keep mining for next attempt if this one is rejected
					}
					nonce++
				}
			}(startNonce)
		}

		statusLine.Store(fmt.Sprintf("Mining block #%d (diff %d)", work.BlockIndex, work.Difficulty))

		// Submit loop for this job — goroutines keep running across rejections
		needNewJob := false
		for !needNewJob {
			select {
			case <-stop:
				close(mineStop)
				wg.Wait()
				fmt.Println()
				return

			case req := <-found:
				// Client-side rate limit: minimum 2s between submissions
				now := time.Now().Unix()
				if now-lastSubmitTs < 2 {
					// Too fast, discard this solution and wait for the next one
					continue
				}
				lastSubmitTs = now

				sr, code, err := submitBlock(node, req)
				if err != nil {
					statusLine.Store(fmt.Sprintf("Submit error: %v", err))
					// goroutines keep running, will try again on next solution
					continue
				}
				if sr.Success {
					atomic.AddInt64(&totalBlocks, 1)
					atomic.StoreInt64(&lastAcceptedTs, req.Timestamp)
					fmt.Printf("\n  [%s]  Block #%d accepted! Hash: %s...  Reward: %d SCO\n",
						time.Now().Format("15:04:05"), sr.BlockIndex, req.Hash[:16], sr.Reward)
					statusLine.Store(fmt.Sprintf("Block #%d found!", sr.BlockIndex))
					needNewJob = true // stop goroutines and fetch fresh job
				} else if code == 429 {
					// Rate limited — back off 30s
					statusLine.Store("Rate limited (429) — backing off 30s...")
					select {
					case <-time.After(30 * time.Second):
					case <-stop:
						close(mineStop)
						wg.Wait()
						fmt.Println()
						return
					}
				} else if code == 409 {
					// Stale tip — another miner found a block first
					statusLine.Store("Stale block (409) — fetching new job...")
					needNewJob = true
				} else {
					// 400 rejection (block delay, anti-spike, hash error, etc.)
					// Goroutines keep running — they'll produce a new solution with a later timestamp
					statusLine.Store(fmt.Sprintf("Rejected: %s — retrying...", sr.Error))
				}
			}
		}

		// Stop goroutines for this job, then loop to fetch new job
		close(mineStop)
		wg.Wait()
	}
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
