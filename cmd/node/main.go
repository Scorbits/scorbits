package main

import (
	"flag"
	"fmt"
	"os"
	"scorbits/blockchain"
	"scorbits/network"
	"time"
)

func main() {
	port := flag.String("port", "3000", "Port TCP du nœud (défaut: 3000)")
	peersStr := flag.String("peers", "", "Pairs initiaux séparés par virgules (ex: 51.91.122.48:3000)")
	dataDir := flag.String("data", ".", "Répertoire contenant scorbits_chain.json")
	flag.Parse()

	// Changer de répertoire pour que blockchain.Load() trouve le bon fichier
	if *dataDir != "." {
		if err := os.Chdir(*dataDir); err != nil {
			fmt.Printf("[Node] Erreur chdir vers %s: %v\n", *dataDir, err)
			os.Exit(1)
		}
	}

	// Charger ou créer la blockchain
	bc, err := blockchain.Load()
	if err != nil {
		fmt.Printf("[Node] Erreur chargement blockchain: %v\n", err)
		os.Exit(1)
	}

	node := network.NewNode(*port, bc)

	// Ajouter les pairs initiaux
	initialPeers := network.ParsePeers(*peersStr)
	for _, peer := range initialPeers {
		node.AddPeer(peer)
	}

	// S'enregistrer auprès de chaque pair et récupérer leur liste de pairs
	fmt.Println("[Node] Enregistrement auprès des pairs...")
	for _, peer := range initialPeers {
		if err := node.RegisterWithPeer(peer); err != nil {
			fmt.Printf("[Node] Avertissement: %v\n", err)
		}
	}

	// Synchroniser la chaîne avec les pairs
	fmt.Println("[Node] Synchronisation de la blockchain...")
	node.SyncWithPeers()

	// Afficher les stats toutes les 30 secondes
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			node.PrintStatus()
		}
	}()

	// Démarrer le nœud P2P
	fmt.Printf("[Node] Nœud démarré sur le port %s\n", *port)
	fmt.Printf("[Node] Paires connectés: %d | Blocs: %d | Supply: %d SCO\n",
		len(node.Peers), len(bc.Blocks), bc.TotalSupply)
	fmt.Println("[Node] En attente de connexions... (stats toutes les 30s)")

	if err := node.Start(); err != nil {
		fmt.Printf("[Node] Erreur fatale: %v\n", err)
		os.Exit(1)
	}
}
