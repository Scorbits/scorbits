package main

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"scorbits/blockchain"
	"scorbits/cli"
	"scorbits/db"
	"scorbits/explorer"
	"scorbits/mempool"
)

func main() {
	fmt.Println("=== SCORBITS — SCO ===")

	// Charger le .env
	if err := godotenv.Load(); err != nil {
		fmt.Println("⚠ .env non trouvé, utilisation des variables d'environnement système")
	}

	// Charger la blockchain
	bc, err := blockchain.Load()
	if err != nil {
		fmt.Printf("Erreur blockchain: %v\n", err)
		os.Exit(1)
	}

	mp := mempool.NewMempool()
	args := os.Args[1:]

	if len(args) > 0 && args[0] == "explorer" {
		// Connecter MongoDB
		if os.Getenv("MONGODB_URI") != "" {
			if err := db.Connect(); err != nil {
				fmt.Printf("⚠ MongoDB non connecté: %v\n", err)
			} else {
				defer db.Disconnect()
			}
		}

		port := os.Getenv("EXPLORER_PORT")
		if port == "" {
			port = "8080"
		}
		if len(args) > 1 {
			port = args[1]
		}

		exp := explorer.NewExplorer(bc, mp, port)
		if err := exp.Start(); err != nil {
			fmt.Printf("Erreur explorer: %v\n", err)
		}
		return
	}

	c := cli.NewCLI(bc, mp)
	c.Run(args)
}