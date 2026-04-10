#!/bin/bash
# Script d'installation Scorbits sur VPS Ubuntu
set -e

echo "=== Installation Scorbits ==="

# 1. Installer Go
echo "[1/6] Installation de Go..."
wget -q https://go.dev/dl/go1.22.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.22.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc

# 2. Créer le dossier
echo "[2/6] Création du dossier /opt/scorbits..."
sudo mkdir -p /opt/scorbits
sudo chown ubuntu:ubuntu /opt/scorbits

# 3. Copier les fichiers
echo "[3/6] Copie des fichiers..."
cp -r . /opt/scorbits/
cd /opt/scorbits

# 4. Compiler
echo "[4/6] Compilation..."
go build -o scorbits .
chmod +x scorbits

# 5. Installer les services
echo "[5/6] Installation des services systemd..."
sudo cp scripts/scorbits.service /etc/systemd/system/
sudo cp scripts/scorbits-explorer.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable scorbits
sudo systemctl enable scorbits-explorer

# 6. Démarrer
echo "[6/6] Démarrage..."
sudo systemctl start scorbits
sleep 2
sudo systemctl start scorbits-explorer

echo ""
echo "=== Installation terminée ==="
echo "Nœud     : sudo systemctl status scorbits"
echo "Explorer : sudo systemctl status scorbits-explorer"
echo "Logs     : sudo journalctl -u scorbits -f"
echo "Explorer accessible sur : http://$(curl -s ifconfig.me):8080"