package blockchain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	TargetBlockTime    = 3 * 60 // 180 secondes
	DifficultyInterval = 10     // ajustement tous les 10 blocs (~40 min)
	MaxDifficulty      = 32     // plafond prod
	MinDifficulty      = 4      // plancher

	// Sécurité : nombre max de blocs qu'une réorg peut annuler
	MaxReorgDepth = 100

	// TreasuryAddress — adresse de trésorerie qui perçoit les frais de 0.1%
	TreasuryAddress = "SCOb2560d1f890413d245bedb2ae24cf1db"

)

// Checkpoints — blocs de confiance ancrés dans le code source.
// Une chaîne entrante ne peut pas réorganiser ces blocs.
// À mettre à jour manuellement tous les ~10 000 blocs après vérification.
var Checkpoints = map[int]string{
	// Format : index -> hash attendu
	// Exemple (à remplir après lancement mainnet) :
	// 10000: "000000ab...",
	// 20000: "000000cd...",
}

type Blockchain struct {
	Blocks           []*Block
	Difficulty       int
	TotalSupply      int
	DifficultyForced bool
}

func NewBlockchain() *Blockchain {
	genesis := GenesisBlock()
	return &Blockchain{
		Blocks:      []*Block{genesis},
		Difficulty:  InitialDiff,
		TotalSupply: genesis.Reward,
	}
}

func (bc *Blockchain) GetLastBlock() *Block {
	return bc.Blocks[len(bc.Blocks)-1]
}

// AdjustDifficulty — ajustement asymétrique basé sur le temps réel des 5 derniers blocs.
// Si elapsed < target (blocs trop rapides) → +3 ; sinon → -1. Cible : 4 min/bloc.
func (bc *Blockchain) AdjustDifficulty() int {
	if len(bc.Blocks) < DifficultyInterval {
		return bc.Difficulty
	}
	first := bc.Blocks[len(bc.Blocks)-DifficultyInterval]
	last := bc.Blocks[len(bc.Blocks)-1]
	elapsed := last.Timestamp - first.Timestamp
	if elapsed <= 0 {
		return bc.Difficulty
	}
	target := int64(4 * 60 * DifficultyInterval) // 4 min × 5 blocs = 1200s
	if elapsed < target {
		newDiff := bc.Difficulty + 1
		if newDiff > MaxDifficulty {
			return MaxDifficulty
		}
		return newDiff
	}
	newDiff := bc.Difficulty - 1
	if newDiff < MinDifficulty {
		return MinDifficulty
	}
	return newDiff
}

// waitBeforeBlock applique l'anti-spike avant de créer un bloc :
// minimum 10s depuis le dernier bloc (toujours actif).
func (bc *Blockchain) waitBeforeBlock(previous *Block, nextIndex int) {
	minTs := previous.Timestamp + 120
	if now := time.Now().Unix(); now < minTs {
		time.Sleep(time.Duration(minTs-now) * time.Second)
	}
}

func (bc *Blockchain) AddBlock(transactions []string, minerAddress string) *Block {
	previous := bc.GetLastBlock()
	nextIndex := previous.Index + 1
	bc.waitBeforeBlock(previous, nextIndex)
	block := NewBlock(nextIndex, transactions, previous.Hash, minerAddress, bc.Difficulty)
	bc.TotalSupply += block.Reward
	bc.Blocks = append(bc.Blocks, block)
	return block
}

// AddBlockMulti crée et mine un bloc avec threads goroutines en parallèle.
func (bc *Blockchain) AddBlockMulti(transactions []string, minerAddress string, threads int) *Block {
	previous := bc.GetLastBlock()
	nextIndex := previous.Index + 1
	bc.waitBeforeBlock(previous, nextIndex)
	block := NewBlockMulti(nextIndex, transactions, previous.Hash, minerAddress, bc.Difficulty, threads)
	bc.TotalSupply += block.Reward
	bc.Blocks = append(bc.Blocks, block)
	return block
}

// ValidateCheckpoints vérifie que tous les checkpoints connus sont respectés.
// Retourne false si un bloc d'une chaîne candidate viole un checkpoint.
func ValidateCheckpoints(blocks []*Block) bool {
	for _, b := range blocks {
		if expected, ok := Checkpoints[b.Index]; ok {
			if b.Hash != expected {
				fmt.Printf("[Sécurité] Checkpoint violé au bloc #%d : attendu %s, reçu %s\n",
					b.Index, expected[:16], b.Hash[:16])
				return false
			}
		}
	}
	return true
}

// CanReorg vérifie qu'une chaîne entrante ne réorganise pas plus de MaxReorgDepth blocs.
// currentLen = longueur de notre chaîne locale
// incomingLen = longueur de la chaîne candidate
// commonAncestor = index du bloc commun le plus récent
func CanReorg(currentLen, incomingLen, commonAncestorIndex int) bool {
	reorgDepth := currentLen - 1 - commonAncestorIndex
	if reorgDepth > MaxReorgDepth {
		fmt.Printf("[Sécurité] Réorg refusée : profondeur %d > max %d\n", reorgDepth, MaxReorgDepth)
		return false
	}
	return true
}

// FindCommonAncestor retourne l'index du bloc commun le plus récent entre
// la chaîne locale et une chaîne candidate.
func (bc *Blockchain) FindCommonAncestor(incoming []*Block) int {
	localHashes := make(map[string]int)
	for _, b := range bc.Blocks {
		localHashes[b.Hash] = b.Index
	}
	// Parcourir la chaîne entrante depuis la fin
	for i := len(incoming) - 1; i >= 0; i-- {
		if idx, ok := localHashes[incoming[i].Hash]; ok {
			return idx
		}
	}
	return 0 // genesis = ancêtre commun minimum
}

// IsConsecutiveMiner retourne true si minerAddress a miné les 2 derniers blocs consécutifs.
// Dans ce cas le prochain bloc de ce mineur doit être refusé.
func (bc *Blockchain) IsConsecutiveMiner(minerAddress string) bool {
	n := len(bc.Blocks)
	if n < 1 {
		return false
	}
	return bc.Blocks[n-1].MinerAddress == minerAddress
}

// ActiveMinersLast24h retourne le nombre d'adresses de mineurs uniques
// ayant miné au moins un bloc dans les dernières 86400 secondes.
func (bc *Blockchain) ActiveMinersLast24h() int {
	cutoff := time.Now().Unix() - 86400
	unique := make(map[string]struct{})
	for _, b := range bc.Blocks {
		if b.Timestamp >= cutoff && b.MinerAddress != "genesis" {
			unique[b.MinerAddress] = struct{}{}
		}
	}
	return len(unique)
}

// IsValid valide la chaîne locale (hash + liens).
// Note: le block delay (180s entre blocs) est appliqué à la soumission mais pas
// re-vérifié ici pour permettre le chargement de chaînes existantes légitimes.
func (bc *Blockchain) IsValid() bool {
	for i := 1; i < len(bc.Blocks); i++ {
		cur := bc.Blocks[i]
		prev := bc.Blocks[i-1]
		if cur.Hash != cur.CalculateHash() {
			return false
		}
		if cur.PreviousHash != prev.Hash {
			return false
		}
	}
	return true
}

// IsValidCandidate valide une chaîne candidate avec checkpoints + limite de réorg.
// À utiliser dans node.go avant d'accepter une chaîne d'un pair.
func (bc *Blockchain) IsValidCandidate(incoming []*Block) bool {
	// 1. Chaîne plus longue ?
	if len(incoming) <= len(bc.Blocks) {
		return false
	}

	// 2. Checkpoints respectés ?
	if !ValidateCheckpoints(incoming) {
		return false
	}

	// 3. Profondeur de réorg acceptable ?
	commonAncestor := bc.FindCommonAncestor(incoming)
	if !CanReorg(len(bc.Blocks), len(incoming), commonAncestor) {
		return false
	}

	// 4. Chaîne structurellement valide (hash + liens) ?
	temp := &Blockchain{Blocks: incoming}
	if !temp.IsValid() {
		return false
	}

	return true
}

func (bc *Blockchain) GetBalance(address string) int {
	balance := 0
	for _, block := range bc.Blocks {
		if block.MinerAddress == address {
			balance += block.Reward
		}
		for _, tx := range block.Transactions {
			switch {
			case strings.HasPrefix(tx, "PREMINE:"):
				// Format: "PREMINE:<address>:<amount>"
				parts := strings.Split(tx, ":")
				if len(parts) == 3 && parts[1] == address {
					if amount, err := strconv.Atoi(parts[2]); err == nil {
						balance += amount
					}
				}
			case strings.HasPrefix(tx, "TREASURY:"):
				// Format: "TREASURY:<address>:<amount>"
				parts := strings.Split(tx, ":")
				if len(parts) == 3 && parts[1] == address {
					if amount, err := strconv.Atoi(parts[2]); err == nil {
						balance += amount
					}
				}
			case strings.Contains(tx, "->") && strings.Contains(tx, "SCO:fee"):
				// Format: "from->to:100SCO:fee1:treasury0"
				arrowParts := strings.SplitN(tx, "->", 2)
				if len(arrowParts) != 2 {
					continue
				}
				from := arrowParts[0]
				rest := arrowParts[1] // "to:100SCO:fee1:treasury0"
				colonParts := strings.SplitN(rest, ":", 4)
				if len(colonParts) < 4 {
					continue
				}
				to := colonParts[0]
				amount, _ := strconv.Atoi(strings.TrimSuffix(colonParts[1], "SCO"))
				fee, _ := strconv.Atoi(strings.TrimPrefix(colonParts[2], "fee"))
				treasury, _ := strconv.Atoi(strings.TrimPrefix(colonParts[3], "treasury"))
				if from == address {
					balance -= amount + fee + treasury
				}
				if to == address {
					balance += amount
				}
				if address == TreasuryAddress {
					balance += treasury
				}
			}
		}
	}
	return balance
}

func (bc *Blockchain) Print() {
	for _, b := range bc.Blocks {
		fmt.Printf("Bloc #%d | Difficulté: %d | Récompense: %d SCO | Mineur: %s\n",
			b.Index, b.Difficulty, b.Reward, b.MinerAddress)
		fmt.Printf("  Hash     : %s\n", b.Hash)
		fmt.Printf("  Précédent: %s\n", b.PreviousHash)
		fmt.Printf("  Temps    : %s\n", time.Unix(b.Timestamp, 0).Format("2006-01-02 15:04:05"))
		for _, tx := range b.Transactions {
			fmt.Printf("  TX       : %s\n", tx)
		}
		fmt.Println()
	}
	fmt.Printf("Supply total en circulation : %d SCO / %d SCO max\n", bc.TotalSupply, MaxSupply)
	fmt.Printf("Blockchain valide : %v\n", bc.IsValid())
}
