#!/bin/sh
# Деплой: собираем образ здесь (сервер не всегда достаёт Docker Hub и мал по памяти),
# передаём по SSH и перезапускаем. Использование: ./deploy.sh [root@сервер]
set -e
HOST=${1:-root@212.193.15.189}
git push -q origin main
docker buildx build --platform linux/amd64 -t nfc-control-app:latest --load .
docker save nfc-control-app:latest | gzip | ssh "$HOST" 'gunzip | docker load'
ssh "$HOST" 'cd /opt/nfc && git pull -q && docker compose --profile prod up -d --no-build && docker compose --profile prod restart caddy'
