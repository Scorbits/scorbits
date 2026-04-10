#!/bin/bash
# Gestion du nœud Scorbits
CMD=$1

case $CMD in
  start)
    sudo systemctl start scorbits scorbits-explorer
    echo "Nœud démarré"
    ;;
  stop)
    sudo systemctl stop scorbits scorbits-explorer
    echo "Nœud arrêté"
    ;;
  restart)
    sudo systemctl restart scorbits scorbits-explorer
    echo "Nœud redémarré"
    ;;
  status)
    sudo systemctl status scorbits scorbits-explorer
    ;;
  logs)
    sudo journalctl -u scorbits -f
    ;;
  logs-explorer)
    sudo journalctl -u scorbits-explorer -f
    ;;
  update)
    echo "Mise à jour du binaire..."
    cd /opt/scorbits
    go build -o scorbits .
    sudo systemctl restart scorbits scorbits-explorer
    echo "Mis à jour et redémarré"
    ;;
  *)
    echo "Usage: ./manage.sh [start|stop|restart|status|logs|logs-explorer|update]"
    ;;
esac