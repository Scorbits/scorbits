package network

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"scorbits/blockchain"
	"strings"
	"sync"
	"time"
)

const (
	// Rate limiting : nombre max de blocs acceptés par pair par minute
	MaxBlocksPerPeerPerMinute = 10
)

type Node struct {
	Port       string
	Blockchain *blockchain.Blockchain
	Peers      []string
	mu         sync.Mutex

	// Rate limiting : peer -> (compteur, timestamp début fenêtre)
	blockRateLimit map[string]*rateWindow
	rateMu         sync.Mutex
}

type rateWindow struct {
	Count       int
	WindowStart time.Time
}

type Message struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// PeerStatus contient le statut d'un pair
type PeerStatus struct {
	Address string `json:"address"`
	Online  bool   `json:"online"`
	Blocks  int    `json:"blocks"`
	Peers   int    `json:"peers"`
}

func NewNode(port string, bc *blockchain.Blockchain) *Node {
	return &Node{
		Port:           port,
		Blockchain:     bc,
		Peers:          make([]string, 0),
		blockRateLimit: make(map[string]*rateWindow),
	}
}

func (n *Node) AddPeer(address string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.Peers {
		if p == address {
			return
		}
	}
	n.Peers = append(n.Peers, address)
	fmt.Printf("[Nœud:%s] Pair ajouté : %s\n", n.Port, address)
}

// GetPeers retourne une copie de la liste des pairs
func (n *Node) GetPeers() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	peers := make([]string, len(n.Peers))
	copy(peers, n.Peers)
	return peers
}

// checkBlockRateLimit vérifie si un pair dépasse la limite de blocs par minute.
// Retourne true si le pair est autorisé, false si rate limit atteint.
func (n *Node) checkBlockRateLimit(peerAddr string) bool {
	n.rateMu.Lock()
	defer n.rateMu.Unlock()

	now := time.Now()
	w, exists := n.blockRateLimit[peerAddr]

	if !exists || now.Sub(w.WindowStart) > time.Minute {
		// Nouvelle fenêtre
		n.blockRateLimit[peerAddr] = &rateWindow{Count: 1, WindowStart: now}
		return true
	}

	w.Count++
	if w.Count > MaxBlocksPerPeerPerMinute {
		fmt.Printf("[Sécurité] Rate limit atteint pour %s (%d blocs/min)\n", peerAddr, w.Count)
		return false
	}
	return true
}

// Start démarre le serveur TCP du nœud
func (n *Node) Start() error {
	listener, err := net.Listen("tcp", ":"+n.Port)
	if err != nil {
		return fmt.Errorf("erreur démarrage nœud: %w", err)
	}
	defer listener.Close()
	fmt.Printf("[Nœud:%s] En écoute...\n", n.Port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go n.handleConnection(conn)
	}
}

func (n *Node) handleConnection(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 8*1024*1024), 8*1024*1024) // 8MB — handle large BLOCKS payloads
	for scanner.Scan() {
		line := scanner.Text()
		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		n.handleMessage(msg, conn)
	}
}

func (n *Node) handleMessage(msg Message, conn net.Conn) {
	peerAddr := conn.RemoteAddr().String()

	switch msg.Type {

	case "GET_BLOCKS":
		n.mu.Lock()
		data, _ := json.Marshal(n.Blockchain.Blocks)
		n.mu.Unlock()
		response := Message{Type: "BLOCKS", Payload: string(data)}
		n.send(conn, response)
		fmt.Printf("[Nœud:%s] Chaîne envoyée à %s\n", n.Port, peerAddr)

	case "BLOCKS":
		var blocks []*blockchain.Block
		if err := json.Unmarshal([]byte(msg.Payload), &blocks); err != nil {
			fmt.Printf("[Nœud:%s] Erreur parsing BLOCKS: %v\n", n.Port, err)
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()

		fmt.Printf("[Nœud:%s] BLOCKS reçu: %d blocs (local: %d)\n", n.Port, len(blocks), len(n.Blockchain.Blocks))
		// Utiliser IsValidCandidate : checkpoints + limite réorg + validation structurelle
		if n.Blockchain.IsValidCandidate(blocks) {
			n.Blockchain.Blocks = blocks
			n.Blockchain.TotalSupply = 0
			for _, b := range blocks {
				n.Blockchain.TotalSupply += b.Reward
			}
			if err := n.Blockchain.Save(); err != nil {
				fmt.Printf("[Nœud:%s] Erreur sauvegarde après sync: %v\n", n.Port, err)
			} else {
				fmt.Printf("[Nœud:%s] Chaîne synchronisée et sauvegardée (%d blocs)\n", n.Port, len(blocks))
			}
		} else {
			fmt.Printf("[Nœud:%s] Chaîne du pair ignorée (longueur insuffisante ou invalide)\n", n.Port)
		}

	case "NEW_BLOCK":
		// Rate limiting par pair
		if !n.checkBlockRateLimit(peerAddr) {
			fmt.Printf("[Sécurité] Bloc refusé (rate limit) de %s\n", peerAddr)
			return
		}

		var block blockchain.Block
		if err := json.Unmarshal([]byte(msg.Payload), &block); err != nil {
			return
		}

		n.mu.Lock()
		last := n.Blockchain.GetLastBlock()

		// Anti-spike : timestamp au moins 10s après le bloc précédent
		if block.Timestamp < last.Timestamp+10 {
			n.mu.Unlock()
			fmt.Printf("[Sécurité] Bloc rejeté : anti-spike (timestamp trop proche)\n")
			return
		}

		// Vérification checkpoint sur le nouveau bloc
		if expected, ok := blockchain.Checkpoints[block.Index]; ok {
			if block.Hash != expected {
				n.mu.Unlock()
				fmt.Printf("[Sécurité] Nouveau bloc #%d viole un checkpoint\n", block.Index)
				return
			}
		}

		accepted := false
		if block.PreviousHash == last.Hash && block.Hash == block.CalculateHash() {
			n.Blockchain.Blocks = append(n.Blockchain.Blocks, &block)
			n.Blockchain.TotalSupply += block.Reward
			fmt.Printf("[Nœud:%s] Nouveau bloc #%d accepté\n", n.Port, block.Index)
			n.Blockchain.Save()
			accepted = true
		}
		n.mu.Unlock()

		// Propager le bloc aux autres pairs (hors de la section critique)
		if accepted {
			n.BroadcastBlock(&block)
		}

	case "HELLO":
		// Un nœud s'annonce avec son port d'écoute
		peerPort := msg.Payload
		peerIP, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		if err != nil {
			return
		}
		peerFull := net.JoinHostPort(peerIP, peerPort)
		n.AddPeer(peerFull)
		fmt.Printf("[Nœud:%s] HELLO reçu de %s\n", n.Port, peerFull)
		// Renvoyer la liste de tous nos pairs connus pour que ce nœud rejoigne le réseau
		peers := n.GetPeers()
		data, _ := json.Marshal(peers)
		n.send(conn, Message{Type: "PEERS", Payload: string(data)})

	case "GET_PEERS":
		peers := n.GetPeers()
		data, _ := json.Marshal(peers)
		n.send(conn, Message{Type: "PEERS", Payload: string(data)})

	case "PEERS":
		var peers []string
		if err := json.Unmarshal([]byte(msg.Payload), &peers); err != nil {
			return
		}
		for _, peer := range peers {
			n.AddPeer(peer)
		}
		fmt.Printf("[Nœud:%s] %d pairs reçus\n", n.Port, len(peers))

	case "GET_STATUS":
		n.mu.Lock()
		blocks := len(n.Blockchain.Blocks)
		peersCount := len(n.Peers)
		n.mu.Unlock()
		type statusPayload struct {
			Blocks int `json:"blocks"`
			Peers  int `json:"peers"`
		}
		data, _ := json.Marshal(statusPayload{Blocks: blocks, Peers: peersCount})
		n.send(conn, Message{Type: "STATUS", Payload: string(data)})

	case "PING":
		n.send(conn, Message{Type: "PONG", Payload: n.Port})
	}
}

func (n *Node) send(conn net.Conn, msg Message) {
	data, _ := json.Marshal(msg)
	fmt.Fprintf(conn, "%s\n", string(data))
}

// SendToPeer envoie un message à un pair
func (n *Node) SendToPeer(peer string, msg Message) error {
	conn, err := net.DialTimeout("tcp", peer, 3*time.Second)
	if err != nil {
		return fmt.Errorf("impossible de joindre %s: %w", peer, err)
	}
	defer conn.Close()
	n.send(conn, msg)

	if msg.Type == "GET_BLOCKS" {
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 8*1024*1024), 8*1024*1024) // 8MB — handle large BLOCKS payloads
		if scanner.Scan() {
			var response Message
			if err := json.Unmarshal([]byte(scanner.Text()), &response); err != nil {
				fmt.Printf("[Nœud:%s] Erreur parsing réponse GET_BLOCKS: %v\n", n.Port, err)
				return nil
			}
			n.handleMessage(response, conn)
		} else if err := scanner.Err(); err != nil {
			fmt.Printf("[Nœud:%s] Erreur lecture réponse GET_BLOCKS: %v\n", n.Port, err)
		}
	}
	return nil
}

// RegisterWithPeer se connecte à un pair, s'annonce (HELLO), reçoit sa liste de pairs
// et les ajoute automatiquement pour rejoindre tout le réseau.
func (n *Node) RegisterWithPeer(peer string) error {
	conn, err := net.DialTimeout("tcp", peer, 3*time.Second)
	if err != nil {
		return fmt.Errorf("impossible de joindre %s: %w", peer, err)
	}
	defer conn.Close()

	// Envoi HELLO avec notre port
	n.send(conn, Message{Type: "HELLO", Payload: n.Port})

	// Lecture de la réponse PEERS
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1*1024*1024), 1*1024*1024)
	if scanner.Scan() {
		var response Message
		if err := json.Unmarshal([]byte(scanner.Text()), &response); err == nil && response.Type == "PEERS" {
			var peers []string
			if err := json.Unmarshal([]byte(response.Payload), &peers); err == nil {
				for _, p := range peers {
					if p != "" {
						n.AddPeer(p)
					}
				}
				fmt.Printf("[Nœud:%s] Enregistré auprès de %s, %d pairs reçus\n", n.Port, peer, len(peers))
			}
		}
	}
	return nil
}

// GetPeersStatus retourne le statut (online/offline, nb blocs, nb pairs) de chaque pair connu
func (n *Node) GetPeersStatus() []PeerStatus {
	peers := n.GetPeers()
	result := make([]PeerStatus, 0, len(peers))
	for _, peer := range peers {
		status := PeerStatus{Address: peer}
		conn, err := net.DialTimeout("tcp", peer, 2*time.Second)
		if err != nil {
			result = append(result, status)
			continue
		}
		status.Online = true

		// Demander le statut
		n.send(conn, Message{Type: "GET_STATUS"})
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		scanner := bufio.NewScanner(conn)
		if scanner.Scan() {
			var resp Message
			if err := json.Unmarshal([]byte(scanner.Text()), &resp); err == nil && resp.Type == "STATUS" {
				var s struct {
					Blocks int `json:"blocks"`
					Peers  int `json:"peers"`
				}
				if err := json.Unmarshal([]byte(resp.Payload), &s); err == nil {
					status.Blocks = s.Blocks
					status.Peers = s.Peers
				}
			}
		}
		conn.Close()
		result = append(result, status)
	}
	return result
}

// SyncWithPeers demande la chaîne à tous les pairs
func (n *Node) SyncWithPeers() {
	for _, peer := range n.Peers {
		fmt.Printf("[Nœud:%s] Synchronisation avec %s...\n", n.Port, peer)
		err := n.SendToPeer(peer, Message{Type: "GET_BLOCKS"})
		if err != nil {
			fmt.Printf("[Nœud:%s] Erreur: %v\n", n.Port, err)
		}
	}
}

// BroadcastBlock annonce un nouveau bloc à tous les pairs
func (n *Node) BroadcastBlock(block *blockchain.Block) {
	data, _ := json.Marshal(block)
	msg := Message{Type: "NEW_BLOCK", Payload: string(data)}
	for _, peer := range n.Peers {
		n.SendToPeer(peer, msg)
	}
}

func (n *Node) PrintStatus() {
	fmt.Printf("[Nœud:%s] Pairs: %d | Blocs: %d | Supply: %d SCO\n",
		n.Port, len(n.Peers), len(n.Blockchain.Blocks), n.Blockchain.TotalSupply)
}

func (n *Node) Ping(peer string) bool {
	err := n.SendToPeer(peer, Message{Type: "PING"})
	return err == nil
}

func (n *Node) GetOnlinePeers() []string {
	online := []string{}
	for _, peer := range n.Peers {
		conn, err := net.DialTimeout("tcp", peer, 2*time.Second)
		if err == nil {
			conn.Close()
			online = append(online, peer)
		}
	}
	return online
}

// ParsePeers parse une liste de pairs séparés par des virgules
func ParsePeers(peersStr string) []string {
	if peersStr == "" {
		return []string{}
	}
	peers := strings.Split(peersStr, ",")
	result := []string{}
	for _, p := range peers {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
