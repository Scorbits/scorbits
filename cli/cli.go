package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"scorbits/blockchain"
	"scorbits/mempool"
	"scorbits/miner"
	"scorbits/network"
	"scorbits/transaction"
	"scorbits/wallet"
	"strconv"
	"strings"
	"time"
)

const WalletFile = "wallet.json"

type SavedWallet struct {
	Address    string `json:"address"`
	PrivKeyHex string `json:"priv_key"`
	PubKeyHex  string `json:"pub_key"`
}

type CLI struct {
	bc *blockchain.Blockchain
	mp *mempool.Mempool
}

func NewCLI(bc *blockchain.Blockchain, mp *mempool.Mempool) *CLI {
	return &CLI{bc: bc, mp: mp}
}

func shortAddr(addr string, n int) string {
	if len(addr) <= n {
		return addr
	}
	return addr[:n] + "..."
}

func (c *CLI) Run(args []string) {
	if len(args) < 1 {
		c.printHelp()
		return
	}

	switch args[0] {
	case "wallet-new":
		c.cmdWalletNew()
	case "wallet-show":
		c.cmdWalletShow()
	case "balance":
		if len(args) < 2 {
			fmt.Println("Usage: balance <adresse>")
			return
		}
		c.cmdBalance(args[1])
	case "send":
		if len(args) < 4 {
			fmt.Println("Usage: send <de> <vers> <montant>")
			return
		}
		amount, err := strconv.Atoi(args[3])
		if err != nil {
			fmt.Println("Montant invalide")
			return
		}
		c.cmdSend(args[1], args[2], amount)
	case "chain":
		c.cmdChain()
	case "status":
		c.cmdStatus()
	case "mine":
		if len(args) < 2 {
			fmt.Println("Usage: mine <adresse> [--threads N]")
			return
		}
		threads := 1
		for i := 2; i < len(args); i++ {
			if args[i] == "--threads" && i+1 < len(args) {
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n < 1 {
					fmt.Println("Erreur : --threads doit être un entier >= 1")
					return
				}
				threads = n
				i++
			}
		}
		c.cmdMine(args[1], threads)
	case "node":
		if len(args) < 2 {
			fmt.Println("Usage: node <port> [pairs]")
			return
		}
		peers := ""
		if len(args) >= 3 {
			peers = args[2]
		}
		c.cmdNode(args[1], peers)
	case "mempool":
		c.cmdMempool()
	default:
		fmt.Printf("Commande inconnue : %s\n", args[0])
		c.printHelp()
	}
}

func (c *CLI) printHelp() {
	fmt.Println(`
=== SCORBITS CLI ===

  wallet-new              Créer un nouveau wallet
  wallet-show             Afficher le wallet sauvegardé
  balance <adresse>       Voir le solde d'une adresse
  send <de> <vers> <amt>  Envoyer des SCO
  chain                   Afficher la blockchain
  status                  État général
  mempool                 Transactions en attente
  node <port> [pairs]     Démarrer un nœud P2P
  mine <adresse> [--threads N]  Miner des SCO (défaut: 1 thread)
`)
}

func (c *CLI) cmdMine(address string, threads int) {
	if !strings.HasPrefix(address, "SCO") {
		fmt.Println("Erreur : l'adresse doit commencer par SCO")
		return
	}
	if threads > 1 {
		miner.RunMulti(c.bc, address, miner.DefaultPeer, threads)
	} else {
		miner.Run(c.bc, address, miner.DefaultPeer)
	}
}

func (c *CLI) cmdWalletNew() {
	w, err := wallet.NewWallet()
	if err != nil {
		fmt.Printf("Erreur: %v\n", err)
		return
	}

	saved := SavedWallet{
		Address:    w.Address,
		PubKeyHex:  fmt.Sprintf("%x", w.PublicKey),
		PrivKeyHex: fmt.Sprintf("%x", w.PrivateKey.D.Bytes()),
	}
	data, _ := json.MarshalIndent(saved, "", "  ")
	os.WriteFile(WalletFile, data, 0600)

	fmt.Println("=== Nouveau wallet créé ===")
	fmt.Printf("Adresse : %s\n", w.Address)
	fmt.Printf("Sauvegardé dans : %s\n", WalletFile)
	fmt.Println("IMPORTANT: sauvegardez ce fichier en lieu sûr.")
}

func (c *CLI) cmdWalletShow() {
	data, err := os.ReadFile(WalletFile)
	if err != nil {
		fmt.Println("Aucun wallet trouvé. Utilisez wallet-new.")
		return
	}
	var saved SavedWallet
	json.Unmarshal(data, &saved)
	fmt.Println("=== Votre wallet ===")
	fmt.Printf("Adresse : %s\n", saved.Address)
	fmt.Printf("Clé pub : %s\n", saved.PubKeyHex)
	fmt.Printf("Solde   : %d SCO\n", c.bc.GetBalance(saved.Address))
}

func (c *CLI) cmdBalance(address string) {
	balance := c.bc.GetBalance(address)
	fmt.Printf("Solde de %s : %d SCO\n", shortAddr(address, 16), balance)
}

func (c *CLI) cmdSend(from, to string, amount int) {
	fee := 1
	if amount > 100 {
		fee = 2
	}
	tx := transaction.NewTransaction(from, to, amount, fee)
	if c.mp.Add(tx) {
		fmt.Printf("Transaction ajoutée au mempool\n")
		fmt.Printf("  %d SCO de %s vers %s\n", amount, shortAddr(from, 12), shortAddr(to, 12))
		fmt.Printf("  Frais: %d SCO (dont %d SCO trésorerie)\n", tx.Fee, tx.Treasury)
	} else {
		fmt.Println("Transaction invalide")
	}
}

func (c *CLI) cmdChain() {
	c.bc.Print()
}

func (c *CLI) cmdStatus() {
	last := c.bc.GetLastBlock()
	fmt.Println("=== SCORBITS — État du réseau ===")
	fmt.Printf("Blocs          : %d\n", len(c.bc.Blocks))
	fmt.Printf("Difficulté     : %d\n", last.Difficulty)
	fmt.Printf("Supply         : %d / %d SCO\n", c.bc.TotalSupply, blockchain.MaxSupply)
	fmt.Printf("Dernier bloc   : #%d (%s)\n", last.Index,
		time.Unix(last.Timestamp, 0).Format("2006-01-02 15:04:05"))
	fmt.Printf("Mempool        : %d tx en attente\n", c.mp.Size())
	fmt.Printf("Chaîne valide  : %v\n", c.bc.IsValid())
}

func (c *CLI) cmdMempool() {
	c.mp.Print()
}

func (c *CLI) cmdNode(port, peersStr string) {
	node := network.NewNode(port, c.bc)
	for _, peer := range network.ParsePeers(peersStr) {
		node.AddPeer(peer)
	}
	if len(node.Peers) > 0 {
		node.SyncWithPeers()
	}
	node.PrintStatus()
	fmt.Printf("Nœud démarré sur le port %s. Ctrl+C pour arrêter.\n", port)
	node.Start()
}
