package blockchain

import (
	"encoding/json"
	"fmt"
	"os"
)

const ChainFile = "scorbits_chain.json"

type ChainData struct {
	Blocks     []*Block `json:"blocks"`
	Difficulty int      `json:"difficulty"`
	TotalSupply int     `json:"total_supply"`
}

// Save sauvegarde la blockchain sur disque
func (bc *Blockchain) Save() error {
	data := ChainData{
		Blocks:      bc.Blocks,
		Difficulty:  bc.Difficulty,
		TotalSupply: bc.TotalSupply,
	}
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("erreur sérialisation: %w", err)
	}
	err = os.WriteFile(ChainFile, bytes, 0644)
	if err != nil {
		return fmt.Errorf("erreur écriture fichier: %w", err)
	}
	fmt.Printf("[Storage] Blockchain sauvegardée (%d blocs)\n", len(bc.Blocks))
	return nil
}

// Load charge la blockchain depuis le disque
func Load() (*Blockchain, error) {
	if _, err := os.Stat(ChainFile); os.IsNotExist(err) {
		fmt.Println("[Storage] Aucune blockchain trouvée, création depuis le genesis...")
		bc := NewBlockchain()
		if err := bc.Save(); err != nil {
			return nil, fmt.Errorf("erreur sauvegarde genesis: %w", err)
		}
		return bc, nil
	}

	bytes, err := os.ReadFile(ChainFile)
	if err != nil {
		return nil, fmt.Errorf("erreur lecture fichier: %w", err)
	}

	var data ChainData
	err = json.Unmarshal(bytes, &data)
	if err != nil {
		return nil, fmt.Errorf("erreur désérialisation: %w", err)
	}

	bc := &Blockchain{
		Blocks:      data.Blocks,
		Difficulty:  data.Difficulty,
		TotalSupply: data.TotalSupply,
	}

	if !bc.IsValid() {
		return nil, fmt.Errorf("blockchain corrompue — intégrité invalide")
	}

	fmt.Printf("[Storage] Blockchain chargée (%d blocs, supply: %d SCO)\n",
		len(bc.Blocks), bc.TotalSupply)
	return bc, nil
}