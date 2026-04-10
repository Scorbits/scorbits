package mempool

import (
	"encoding/json"
	"fmt"
	"os"
	"scorbits/transaction"
	"sync"
)

const MempoolFile = "mempool.json"

type Mempool struct {
	mu           sync.Mutex
	Transactions []*transaction.Transaction
}

func NewMempool() *Mempool {
	mp := &Mempool{
		Transactions: make([]*transaction.Transaction, 0),
	}
	mp.Load()
	return mp
}

// hasPendingFrom vérifie si une adresse a déjà une transaction en attente.
// Protection anti-double spend : on refuse une 2ème tx de la même adresse
// tant que la première n'est pas confirmée dans un bloc.
// À appeler avec le mutex déjà verrouillé.
func (m *Mempool) hasPendingFrom(address string) bool {
	for _, tx := range m.Transactions {
		if tx.From == address {
			return true
		}
	}
	return false
}

const TreasuryAddress = "SCOb2560d1f890413d245bedb2ae24cf1db"

func (m *Mempool) Add(tx *transaction.Transaction) bool {
	if !tx.IsValid() {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Exception trésorerie : les paiements publicitaires vers l'adresse de trésorerie
	// peuvent coexister avec d'autres tx en attente (pas de restriction anti-double spend)
	if tx.To != TreasuryAddress && tx.From != "" && m.hasPendingFrom(tx.From) {
		fmt.Printf("[Mempool] Double spend refusé : %s a déjà une tx en attente\n", tx.From[:16])
		return false
	}

	m.Transactions = append(m.Transactions, tx)
	m.save()
	return true
}

func (m *Mempool) GetPending(limit int) []*transaction.Transaction {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit > len(m.Transactions) {
		limit = len(m.Transactions)
	}
	return m.Transactions[:limit]
}

func (m *Mempool) Clear(confirmed []*transaction.Transaction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make(map[string]bool)
	for _, tx := range confirmed {
		ids[tx.ID] = true
	}
	remaining := make([]*transaction.Transaction, 0)
	for _, tx := range m.Transactions {
		if !ids[tx.ID] {
			remaining = append(remaining, tx)
		}
	}
	m.Transactions = remaining
	m.save()
}

// ClearByStrings removes mempool transactions whose string representation
// matches any entry in txStrings (used after WASM-mined blocks are accepted).
func (m *Mempool) ClearByStrings(txStrings []string) {
	if len(txStrings) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	strs := make(map[string]bool, len(txStrings))
	for _, s := range txStrings {
		strs[s] = true
	}
	remaining := make([]*transaction.Transaction, 0, len(m.Transactions))
	for _, tx := range m.Transactions {
		if !strs[tx.ToString()] {
			remaining = append(remaining, tx)
		}
	}
	m.Transactions = remaining
	m.save()
}

func (m *Mempool) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Transactions)
}

func (m *Mempool) save() {
	data, _ := json.MarshalIndent(m.Transactions, "", "  ")
	os.WriteFile(MempoolFile, data, 0644)
}

func (m *Mempool) Load() {
	data, err := os.ReadFile(MempoolFile)
	if err != nil {
		return
	}
	json.Unmarshal(data, &m.Transactions)
	if len(m.Transactions) > 0 {
		fmt.Printf("[Mempool] %d transaction(s) rechargée(s)\n", len(m.Transactions))
	}
}

func (m *Mempool) Print() {
	fmt.Printf("Mempool : %d transaction(s) en attente\n", len(m.Transactions))
	for _, tx := range m.Transactions {
		tx.Print()
	}
}
