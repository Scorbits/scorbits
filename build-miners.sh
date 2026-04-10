#!/bin/bash
set -e

mkdir -p dist

echo "Building Scorbits Miner for all platforms..."

GOOS=windows GOARCH=amd64 go build -o dist/scorbits-miner-windows.exe ./cmd/miner
echo "  [OK] dist/scorbits-miner-windows.exe"

GOOS=darwin GOARCH=amd64 go build -o dist/scorbits-miner-macos ./cmd/miner
echo "  [OK] dist/scorbits-miner-macos"

GOOS=linux GOARCH=amd64 go build -o dist/scorbits-miner-linux ./cmd/miner
echo "  [OK] dist/scorbits-miner-linux"

echo ""
echo "Done! Binaries are in dist/"
ls -lh dist/
