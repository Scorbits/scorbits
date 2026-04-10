#!/bin/bash
chmod +x scorbits-miner-linux

read -p "Enter your SCO address (SCO...): " ADDR
if [[ "$ADDR" != SCO* ]]; then echo "Invalid address - must start with SCO"; exit 1; fi

echo ""
echo "Starting Scorbits Miner..."
echo "Address: $ADDR"
echo ""
./scorbits-miner-linux --address "$ADDR" --node https://scorbits.com --threads 2
echo ""
read -p "Miner stopped. Press Enter to close..." _
