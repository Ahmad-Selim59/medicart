#!/bin/bash

set -e

APP_DIR="/home/ubuntu/medicart"
WEB_DIR="$APP_DIR/web-server"
SERVICE="medicart"

echo "Starting deployment..."

### Pull latest code
echo "Pulling latest changes..."
cd "$APP_DIR"
git pull origin main

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

echo "Deployment complete!!!"
