//go:build js && wasm

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"syscall/js"
	"time"
)

// calculateHash computes SHA256(index+timestamp+txData+previousHash+nonce+minerAddress)
func calculateHash(index int, timestamp int64, txData string, previousHash string, nonce int, minerAddress string) string {
	input := fmt.Sprintf("%d%d%s%s%d%s", index, timestamp, txData, previousHash, nonce, minerAddress)
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

// jsArrayToTxData converts a JS array of transaction strings to ";" joined string.
func jsArrayToTxData(arr js.Value) string {
	if arr.Type() != js.TypeObject || arr.IsNull() || arr.IsUndefined() {
		return "wasm-browser-mine"
	}
	n := arr.Length()
	if n == 0 {
		return "wasm-browser-mine"
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = arr.Index(i).String()
	}
	return strings.Join(parts, ";")
}

func mineBlock(this js.Value, args []js.Value) interface{} {
	if len(args) < 6 {
		return js.ValueOf("error: arguments manquants")
	}
	index := args[0].Int()
	txData := jsArrayToTxData(args[1])
	previousHash := args[2].String()
	minerAddress := args[3].String()
	difficulty := args[4].Int()
	foundCallback := args[5]
	target := strings.Repeat("0", difficulty)
	timestamp := time.Now().Unix()
	nonce := 0

	go func() {
		for {
			hash := calculateHash(index, timestamp, txData, previousHash, nonce, minerAddress)
			if strings.HasPrefix(hash, target) {
				result := map[string]interface{}{
					"index":        index,
					"timestamp":    timestamp,
					"previousHash": previousHash,
					"hash":         hash,
					"nonce":        nonce,
					"minerAddress": minerAddress,
					"difficulty":   difficulty,
				}
				foundCallback.Invoke(js.ValueOf(result))
				return
			}
			nonce++
			if nonce%100 == 0 {
				js.Global().Call("updateMinerProgress", js.ValueOf(nonce))
			}
		}
	}()
	return nil
}

func mineBlockFrom(this js.Value, args []js.Value) interface{} {
	if len(args) < 8 {
		return js.ValueOf("error: arguments manquants")
	}
	index := args[0].Int()
	txData := jsArrayToTxData(args[1])
	previousHash := args[2].String()
	minerAddress := args[3].String()
	difficulty := args[4].Int()
	nonceStart := args[5].Int()
	nonceStep := args[6].Int()
	foundCallback := args[7]
	target := strings.Repeat("0", difficulty)
	timestamp := time.Now().Unix()
	nonce := nonceStart

	go func() {
		for {
			hash := calculateHash(index, timestamp, txData, previousHash, nonce, minerAddress)
			if strings.HasPrefix(hash, target) {
				result := map[string]interface{}{
					"index":        index,
					"timestamp":    timestamp,
					"previousHash": previousHash,
					"hash":         hash,
					"nonce":        nonce,
					"minerAddress": minerAddress,
					"difficulty":   difficulty,
				}
				foundCallback.Invoke(js.ValueOf(result))
				return
			}
			nonce += nonceStep
			if nonce%100 == 0 {
				js.Global().Call("updateMinerProgress", js.ValueOf(nonce))
			}
		}
	}()
	return nil
}

func main() {
	c := js.Global()
	c.Set("goMineBlock", js.FuncOf(mineBlock))
	c.Set("goMineBlockFrom", js.FuncOf(mineBlockFrom))
	c.Set("wasmGoReady", js.ValueOf(true))
	fmt.Println("[WASM] Mineur Scorbits (SHA256) chargé")
	select {}
}
