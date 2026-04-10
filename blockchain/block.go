package blockchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const (
	InitialReward = 11
	HalvingBlocks = 840000
	MaxSupply     = 99000000
	InitialDiff   = 5
)

type Block struct {
	Index        int
	Timestamp    int64
	Transactions []string
	PreviousHash string
	Hash         string
	Nonce        int
	Difficulty   int
	Reward       int
	MinerAddress string
}

// CalculateHash computes SHA256(index+timestamp+txData+previousHash+nonce+minerAddress)
func (b *Block) CalculateHash() string {
	txData := strings.Join(b.Transactions, ";")
	input := fmt.Sprintf("%d%d%s%s%d%s",
		b.Index, b.Timestamp, txData, b.PreviousHash, b.Nonce, b.MinerAddress)
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

func (b *Block) Mine() {
	target := strings.Repeat("0", b.Difficulty)
	start := time.Now()
	lastReport := start
	fmt.Printf("[Mineur] Bloc #%d | Difficulté: %d | Récompense: %d SCO\n",
		b.Index, b.Difficulty, b.Reward)
	for !strings.HasPrefix(b.Hash, target) {
		b.Nonce++
		b.Hash = b.CalculateHash()
		if since := time.Since(lastReport); since >= 10*time.Second {
			fmt.Printf("[Mineur] Toujours en train de miner... nonce: %d\n", b.Nonce)
			lastReport = time.Now()
		}
	}
	fmt.Printf("[Trouvé!] Bloc #%d | Nonce: %d | Hash: %.16s... | Durée: %s\n",
		b.Index, b.Nonce, b.Hash, time.Since(start).Round(time.Second))
}

// MineMulti lance threads goroutines en parallèle pour trouver un nonce valide.
// Thread t commence au nonce t et avance par pas de threads (t, t+N, t+2N, ...).
// Quand une goroutine trouve un hash valide, elle annule les autres.
// Le hashrate affiché est la somme de tous les threads.
func (b *Block) MineMulti(threads int) {
	if threads <= 1 {
		b.Mine()
		return
	}

	target := strings.Repeat("0", b.Difficulty)
	start := time.Now()

	fmt.Printf("[Mineur] Bloc #%d | Difficulté: %d | Récompense: %d SCO | %d threads\n",
		b.Index, b.Difficulty, b.Reward, threads)

	type result struct {
		nonce int
		hash  string
	}

	found := make(chan result, 1)
	var totalHashes int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Snapshot des champs immuables pendant le minage
	idx := b.Index
	prevHash := b.PreviousHash
	minerAddr := b.MinerAddress
	ts := b.Timestamp
	txData := strings.Join(b.Transactions, ";")

	for t := 0; t < threads; t++ {
		go func(startNonce int) {
			nonce := startNonce
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				input := fmt.Sprintf("%d%d%s%s%d%s", idx, ts, txData, prevHash, nonce, minerAddr)
				h := sha256.Sum256([]byte(input))
				hash := hex.EncodeToString(h[:])
				atomic.AddInt64(&totalHashes, 1)
				if strings.HasPrefix(hash, target) {
					select {
					case found <- result{nonce, hash}:
						cancel()
					default:
					}
					return
				}
				nonce += threads
			}
		}(t)
	}

	// Rapport de hashrate toutes les 10s
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h := atomic.LoadInt64(&totalHashes)
				elapsed := time.Since(start).Seconds()
				if elapsed > 0 {
					fmt.Printf("[Mineur] %d threads | %.0f H/s | hashes: %d\n",
						threads, float64(h)/elapsed, h)
				}
			}
		}
	}()

	r := <-found
	b.Nonce = r.nonce
	b.Hash = r.hash

	h := atomic.LoadInt64(&totalHashes)
	elapsed := time.Since(start)
	hashrate := 0.0
	if elapsed.Seconds() > 0 {
		hashrate = float64(h) / elapsed.Seconds()
	}
	fmt.Printf("[Trouvé!] Bloc #%d | Nonce: %d | Hash: %.16s... | Durée: %s | %.0f H/s (%d threads)\n",
		b.Index, b.Nonce, b.Hash, elapsed.Round(time.Second), hashrate, threads)
}

// NewBlockMulti crée un bloc et le mine avec threads goroutines en parallèle.
func NewBlockMulti(index int, transactions []string, previousHash string, minerAddress string, difficulty int, threads int) *Block {
	block := &Block{
		Index:        index,
		Timestamp:    time.Now().Unix(),
		Transactions: transactions,
		PreviousHash: previousHash,
		Nonce:        0,
		Difficulty:   difficulty,
		Reward:       CalculateReward(index),
		MinerAddress: minerAddress,
	}
	block.Hash = block.CalculateHash()
	block.MineMulti(threads)
	return block
}

func CalculateReward(blockIndex int) int {
	halvings := blockIndex / HalvingBlocks
	reward := InitialReward
	for i := 0; i < halvings; i++ {
		reward /= 2
		if reward == 0 {
			return 0
		}
	}
	return reward
}

func NewBlock(index int, transactions []string, previousHash string, minerAddress string, difficulty int) *Block {
	block := &Block{
		Index:        index,
		Timestamp:    time.Now().Unix(),
		Transactions: transactions,
		PreviousHash: previousHash,
		Nonce:        0,
		Difficulty:   difficulty,
		Reward:       CalculateReward(index),
		MinerAddress: minerAddress,
	}
	block.Hash = block.CalculateHash()
	block.Mine()
	return block
}

// GenesisBlock — hash fixe, pas de minage
func GenesisBlock() *Block {
	return &Block{
		Index:     0,
		Timestamp: 1742000000,
		Transactions: []string{
			"Scorbits Genesis Block - SCO - 2026",
			"PREMINE:SCO30e1a847eaf8aac6b042c32d63108833:11000000",
		},
		PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
		Hash:         "000000000000000000000000000000000000000000000000scorbitsgenesis00",
		Nonce:        0,
		Difficulty:   InitialDiff,
		Reward:       11000000,
		MinerAddress: "genesis",
	}
}
