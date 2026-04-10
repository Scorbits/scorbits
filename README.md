# Scorbits (SCO)

**Proof of Work — SHA-256 — Resistance of time as proof of trust**

Scorbits is an independent SHA-256 Proof of Work blockchain, open source and accessible to everyone.

Explorer & Wallet : https://scorbits.com

## Technical Specifications

| Parameter | Value |
|---|---|
| Algorithm | SHA-256 (Proof of Work) |
| Max supply | 99,000,000 SCO |
| Block reward | 11 SCO |
| Block time | ~3 minutes |
| Halving | Every 840,000 blocks |
| Difficulty adjustment | Every 5 blocks |
| Transaction fee | 0.1% to Treasury |

## Mine SCO
Download the miner at https://scorbits.com/mine (Windows, Linux, macOS)

## Run a Node
Download the node binary at https://scorbits.com/mine

Linux : ./scorbits-node-linux --peers 51.91.122.48:3000
Windows : scorbits-node-windows.exe --peers 51.91.122.48:3000

## Build from Source
git clone https://github.com/Scorbits/scorbits.git
cd scorbits
go mod tidy
cp .env.example .env
go run main.go explorer

Requirements : Go 1.21+, MongoDB Atlas, Gmail SMTP

## Whitepaper
EN : https://scorbits.com/static/Scorbits_Whitepaper_EN.docx
FR : https://scorbits.com/static/Scorbits_Whitepaper_FR.docx

Scorbits (SCO) — Proof of Work — 2026
