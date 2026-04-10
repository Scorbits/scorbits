#!/bin/bash
# Compile les binaires de minage pour Windows, Linux et Mac
set -e

VERSION="v1.0.0"
OUTPUT_DIR="./releases"
mkdir -p $OUTPUT_DIR

echo "=== Build Scorbits $VERSION ==="

echo "[1/3] Windows (amd64)..."
GOOS=windows GOARCH=amd64 go build -o $OUTPUT_DIR/scorbits-windows-amd64.exe .
echo "OK : $OUTPUT_DIR/scorbits-windows-amd64.exe"

echo "[2/3] Linux (amd64)..."
GOOS=linux GOARCH=amd64 go build -o $OUTPUT_DIR/scorbits-linux-amd64 .
chmod +x $OUTPUT_DIR/scorbits-linux-amd64
echo "OK : $OUTPUT_DIR/scorbits-linux-amd64"

echo "[3/3] Mac (amd64 + arm64)..."
GOOS=darwin GOARCH=amd64 go build -o $OUTPUT_DIR/scorbits-mac-amd64 .
GOOS=darwin GOARCH=arm64 go build -o $OUTPUT_DIR/scorbits-mac-arm64 .
chmod +x $OUTPUT_DIR/scorbits-mac-amd64
chmod +x $OUTPUT_DIR/scorbits-mac-arm64
echo "OK : $OUTPUT_DIR/scorbits-mac-amd64 (Intel)"
echo "OK : $OUTPUT_DIR/scorbits-mac-arm64 (Apple Silicon)"

echo ""
echo "=== Releases générées dans $OUTPUT_DIR ==="
ls -lh $OUTPUT_DIR