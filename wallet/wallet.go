package wallet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

type Wallet struct {
	PrivateKey *ecdsa.PrivateKey
	PublicKey  []byte
	Address    string
}

func NewWallet() (*Wallet, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("erreur génération clé: %w", err)
	}

	pubKey := append(
		privateKey.PublicKey.X.Bytes(),
		privateKey.PublicKey.Y.Bytes()...,
	)

	address := generateAddress(pubKey)

	return &Wallet{
		PrivateKey: privateKey,
		PublicKey:  pubKey,
		Address:    address,
	}, nil
}

func generateAddress(pubKey []byte) string {
	h := sha256.Sum256(pubKey)
	return "SCO" + hex.EncodeToString(h[:])[:32]
}

func (w *Wallet) Sign(data string) (string, error) {
	hash := sha256.Sum256([]byte(data))
	r, s, err := ecdsa.Sign(rand.Reader, w.PrivateKey, hash[:])
	if err != nil {
		return "", fmt.Errorf("erreur signature: %w", err)
	}
	sig := append(r.Bytes(), s.Bytes()...)
	return hex.EncodeToString(sig), nil
}

func (w *Wallet) Verify(data string, signature string) bool {
	hash := sha256.Sum256([]byte(data))
	sigBytes, err := hex.DecodeString(signature)
	if err != nil || len(sigBytes) < 64 {
		return false
	}
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])
	return ecdsa.Verify(&w.PrivateKey.PublicKey, hash[:], r, s)
}

func (w *Wallet) Print() {
	fmt.Printf("Adresse : %s\n", w.Address)
	fmt.Printf("Clé pub : %s\n", hex.EncodeToString(w.PublicKey))
}
