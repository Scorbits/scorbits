#!/bin/bash
# Sauvegarde automatique de scorbits_chain.json
BACKUP_DIR="/home/ubuntu/scorbits/backups"
CHAIN_FILE="/home/ubuntu/scorbits/scorbits_chain.json"
MAX_BACKUPS=48

mkdir -p $BACKUP_DIR
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
cp $CHAIN_FILE $BACKUP_DIR/scorbits_chain_$TIMESTAMP.json

# Garder seulement les MAX_BACKUPS dernières sauvegardes
ls -t $BACKUP_DIR/scorbits_chain_*.json | tail -n +$((MAX_BACKUPS+1)) | xargs -r rm

echo "[Backup] Sauvegarde effectuée : scorbits_chain_$TIMESTAMP.json"
