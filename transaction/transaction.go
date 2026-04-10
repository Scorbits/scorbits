package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

const TreasuryFeePercent = 0 // 0.1% arrondi sur petits montants — voir calcul ci-dessous

type Transaction struct {
	ID        string
	From      string
	To        string
	Amount    int
	Fee       int
	Treasury  int
	Timestamp int64
	Signature string
}

func NewTransaction(from, to string, amount, fee int) *Transaction {
	// 0.1% de trésorerie, minimum 0 si montant trop petit
	// Ex: amount=100 → treasury=0 (0.1% de 100 = 0.1, arrondi à 0)
	// Ex: amount=1000 → treasury=1 (0.1% de 1000 = 1)
	treasury := amount / 1000 // 0.1% du montant
	if treasury < 0 {
		treasury = 0
	}

	tx := &Transaction{
		From:      from,
		To:        to,
		Amount:    amount,
		Fee:       fee,
		Treasury:  treasury,
		Timestamp: time.Now().Unix(),
	}
	tx.ID = tx.calculateID()
	return tx
}

func (tx *Transaction) calculateID() string {
	record := fmt.Sprintf("%s%s%d%d%d",
		tx.From, tx.To, tx.Amount, tx.Fee, tx.Timestamp)
	h := sha256.Sum256([]byte(record))
	return hex.EncodeToString(h[:])
}

func (tx *Transaction) ToString() string {
	return fmt.Sprintf("%s->%s:%dSCO:fee%d:treasury%d",
		tx.From, tx.To, tx.Amount, tx.Fee, tx.Treasury)
}

func (tx *Transaction) IsValid() bool {
	if tx.From == "" || tx.To == "" {
		return false
	}
	if tx.Amount <= 0 {
		return false
	}
	if tx.Fee < 0 {
		return false
	}
	return true
}

func (tx *Transaction) Print() {
	fmt.Printf("TX %s\n", tx.ID[:16])
	fmt.Printf("  De      : %s\n", tx.From)
	fmt.Printf("  Vers    : %s\n", tx.To)
	fmt.Printf("  Montant : %d SCO\n", tx.Amount)
	fmt.Printf("  Frais   : %d SCO\n", tx.Fee)
	fmt.Printf("  Trésorerie (0.1%%): %d SCO\n", tx.Treasury)
}