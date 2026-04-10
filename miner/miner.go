package miner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"scorbits/blockchain"
	"time"
)

const DefaultPeer = "51.91.122.48:3000"

type networkMessage struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// SyncChain synchronise la blockchain locale avec un nœud pair via TCP.
func SyncChain(bc *blockchain.Blockchain, peer string) {
	conn, err := net.DialTimeout("tcp", peer, 5*time.Second)
	if err != nil {
		fmt.Printf("[Sync] Impossible de joindre %s: %v\n", peer, err)
		return
	}
	defer conn.Close()

	req := networkMessage{Type: "GET_BLOCKS", Payload: ""}
	data, _ := json.Marshal(req)
	fmt.Fprintf(conn, "%s\n", string(data))

	conn.SetDeadline(time.Now().Add(30 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 8*1024*1024), 8*1024*1024)

	if !scanner.Scan() {
		fmt.Printf("[Sync] Pas de réponse du nœud\n")
		return
	}

	var resp networkMessage
	if err := json.Unmarshal([]byte(scanner.Text()), &resp); err != nil {
		fmt.Printf("[Sync] Erreur parsing réponse: %v\n", err)
		return
	}
	if resp.Type != "BLOCKS" {
		return
	}

	var blocks []*blockchain.Block
	if err := json.Unmarshal([]byte(resp.Payload), &blocks); err != nil {
		return
	}
	if bc.IsValidCandidate(blocks) {
		bc.Blocks = blocks
		bc.TotalSupply = 0
		for _, b := range blocks {
			bc.TotalSupply += b.Reward
		}
		bc.Difficulty = blocks[len(blocks)-1].Difficulty
		fmt.Printf("[Sync] Chaîne synchronisée: %d blocs (difficulté: %d)\n",
			len(blocks), bc.Difficulty)
	} else {
		fmt.Printf("[Sync] Chaîne locale déjà à jour\n")
	}
}

// submitBlock envoie un bloc miné au nœud via TCP NEW_BLOCK.
func submitBlock(block *blockchain.Block, peer string) {
	data, _ := json.Marshal(block)
	msg := networkMessage{Type: "NEW_BLOCK", Payload: string(data)}
	msgData, _ := json.Marshal(msg)

	conn, err := net.DialTimeout("tcp", peer, 5*time.Second)
	if err != nil {
		fmt.Printf("[Mineur] Erreur connexion au nœud %s: %v\n", peer, err)
		return
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\n", string(msgData))
	fmt.Printf("[Mineur] Bloc #%d soumis au nœud %s\n", block.Index, peer)
}

// Run lance la boucle de minage en continu (1 thread).
// bc doit déjà être chargé. La chaîne est d'abord synchronisée avec le pair.
func Run(bc *blockchain.Blockchain, minerAddress, peer string) {
	fmt.Println("=== Scorbits Miner ===")
	fmt.Printf("Nœud    : %s\n", peer)
	fmt.Printf("Adresse : %s\n", minerAddress)
	fmt.Println()

	fmt.Printf("[Sync] Synchronisation avec %s...\n", peer)
	SyncChain(bc, peer)

	last := bc.GetLastBlock()
	fmt.Printf("[Mineur] Chaîne: %d blocs | Difficulté: %d | Dernier bloc: #%d\n",
		len(bc.Blocks), bc.Difficulty, last.Index)
	fmt.Println()

	for {
		block := bc.AddBlock([]string{"empty-block"}, minerAddress)
		submitBlock(block, peer)
		bc.Save()
		SyncChain(bc, peer)
	}
}

// RunMulti lance la boucle de minage avec threads goroutines en parallèle.
// Chaque goroutine teste un sous-ensemble de nonces (départ t, pas threads).
// Le hashrate affiché est la somme de tous les threads.
func RunMulti(bc *blockchain.Blockchain, minerAddress, peer string, threads int) {
	fmt.Println("=== Scorbits Miner ===")
	fmt.Printf("Nœud    : %s\n", peer)
	fmt.Printf("Adresse : %s\n", minerAddress)
	fmt.Printf("Threads : %d\n", threads)
	fmt.Println()

	fmt.Printf("[Sync] Synchronisation avec %s...\n", peer)
	SyncChain(bc, peer)

	last := bc.GetLastBlock()
	fmt.Printf("[Mineur] Chaîne: %d blocs | Difficulté: %d | Dernier bloc: #%d\n",
		len(bc.Blocks), bc.Difficulty, last.Index)
	fmt.Println()

	for {
		block := bc.AddBlockMulti([]string{"empty-block"}, minerAddress, threads)
		submitBlock(block, peer)
		bc.Save()
		SyncChain(bc, peer)
	}
}
