#!/bin/bash
# Run ON the Ubuntu server after cloning the repo to /home/ubuntu/medicart.
# This server image does not include Go — use ../release.sh from your Mac instead.

set -e

APP_DIR="/home/ubuntu/medicart"
WEB_DIR="$APP_DIR/web-server"
SERVICE="medicart"
BINARY="$WEB_DIR/medicart-server-ubuntu"

echo "Starting deployment..."

### Pull latest code
echo "Pulling latest changes..."
cd "$APP_DIR"
git pull origin main

if command -v go >/dev/null 2>&1; then
	echo "Building web server..."
	cd "$WEB_DIR"
	go build -o medicart-server-ubuntu .
else
	echo "Go is not installed on this server (expected on EC2)."
	if [ ! -x "$BINARY" ]; then
		echo "ERROR: No binary at $BINARY"
		echo "Deploy from your Mac instead:"
		echo "  MEDICART_SERVER=ubuntu@YOUR_HOST ./release.sh"
		exit 1
	fi
	echo "Using existing binary. To ship a new build, run release.sh from your Mac."
fi

### Fix permissions (safe)
sudo chown -R ubuntu:ubuntu "$APP_DIR"

### Clean old process + port
echo "Cleaning old processes..."
sudo systemctl stop "$SERVICE" || true
sudo fuser -k 8081/tcp || true
sudo pkill -f medicart-server-ubuntu || true

### Restart service
echo "Restarting service..."
sudo systemctl daemon-reload
sudo systemctl restart "$SERVICE"

### Status check
echo "Checking service status..."
sudo systemctl status "$SERVICE" --no-pager

echo "Deployment complete."
