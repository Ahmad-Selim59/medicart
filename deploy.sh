#!/bin/bash
# Run ON the Ubuntu server at /home/ubuntu/medicart/deploy.sh

set -e

APP_DIR="/home/ubuntu/medicart"
WEB_DIR="$APP_DIR/web-server"
SERVICE="medicart"
GO_INSTALL_DIR="/usr/local/go"

export PATH="$GO_INSTALL_DIR/bin:$PATH"

if ! command -v go >/dev/null 2>&1; then
	echo "ERROR: Go is not installed. Run setup.sh first or install Go to $GO_INSTALL_DIR."
	exit 1
fi

echo "Starting deployment..."

echo "Pulling latest changes..."
cd "$APP_DIR"
git pull origin main

echo "Building web server..."
cd "$WEB_DIR"
go mod tidy
go build -o medicart-server-ubuntu .

sudo chown -R ubuntu:ubuntu "$APP_DIR"

echo "Cleaning old processes..."
sudo systemctl stop "$SERVICE" || true
sudo fuser -k 8081/tcp || true
sudo pkill -f medicart-server-ubuntu || true

echo "Restarting service..."
sudo systemctl daemon-reload
sudo systemctl restart "$SERVICE"

echo "Checking service status..."
sudo systemctl status "$SERVICE" --no-pager

echo "Deployment complete."
