#!/bin/bash
# Compile le mineur Go en WebAssembly
set -e

echo "=== Compilation WebAssembly ==="

WASM_OUT="./static/miner.wasm"

echo "[1/2] Compilation Go → WebAssembly..."
GOOS=js GOARCH=wasm go build -o $WASM_OUT ./wasm/

echo "[2/2] Vérification..."
if [ -f "$WASM_OUT" ]; then
  SIZE=$(du -h $WASM_OUT | cut -f1)
  echo "OK : $WASM_OUT ($SIZE)"
else
  echo "ERREUR : fichier non généré"
  exit 1
fi

echo ""
echo "=== WebAssembly prêt ==="
echo "Fichier : $WASM_OUT"
echo "Lancez le serveur : go run main.go explorer"
EOF