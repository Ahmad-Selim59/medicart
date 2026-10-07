#!/bin/bash

set -e  # Exit on error

### CONFIG (EDIT THESE)
GITHUB_TOKEN="your_github_token_here"
REPO_URL="https://github.com/Ahmad-Selim59/medicart.git" #this should be whatever repo you keep the web server in
DOMAIN="yourdomain.com"

APP_DIR="/home/ubuntu/medicart"
WEB_DIR="$APP_DIR/web-server"
GO_VERSION="1.24.2"
GO_INSTALL_DIR="/usr/local/go"

install_go() {
	if [ -x "$GO_INSTALL_DIR/bin/go" ] && "$GO_INSTALL_DIR/bin/go" version | grep -q "go${GO_VERSION}"; then
		echo "Go ${GO_VERSION} already installed."
		return
	fi

	echo "Installing Go ${GO_VERSION}..."
	curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tar.gz
	sudo rm -rf "$GO_INSTALL_DIR"
	sudo tar -C /usr/local -xzf /tmp/go.tar.gz
	rm /tmp/go.tar.gz

	sudo tee /etc/profile.d/go-path.sh > /dev/null <<'EOF'
export PATH="/usr/local/go/bin:$PATH"
EOF
}

ensure_go_path() {
	export PATH="$GO_INSTALL_DIR/bin:$PATH"
}

### Update + install packages
echo "Updating system and installing dependencies..."
sudo apt update -y
sudo DEBIAN_FRONTEND=noninteractive apt install -y \
    git gh nginx certbot python3-certbot-nginx curl

install_go
ensure_go_path

### GitHub auth (non-interactive)
echo "Authenticating GitHub CLI..."
echo "$GITHUB_TOKEN" | gh auth login --with-token || true

### Clone repo
echo "Cloning repository..."
if [ ! -d "$APP_DIR" ]; then
    git clone "$REPO_URL" "$APP_DIR"
else
    echo "Repo exists, pulling latest..."
    cd "$APP_DIR"
    git pull
fi

### Set permissions
sudo chown -R ubuntu:ubuntu "$APP_DIR"

### Create .env file
echo "Creating .env file..."
cat <<EOF > "$WEB_DIR/.env"
NEXT_PUBLIC_SUPABASE_URL=https://jntutbhgejmeejgxnzqb.supabase.co
NEXT_PUBLIC_SUPABASE_ANON_KEY=sb_publishable_HAIEHgFAePvfHsIHNOQpvw_mc0zrd0q
EOF

### CLEANUP: free port 8081 BEFORE starting service
echo "Cleaning up port 8081..."
sudo fuser -k 8081/tcp || true
sudo pkill -f medicart-server-ubuntu || true

### Nginx setup
echo "Configuring Nginx..."

NGINX_CONF="/etc/nginx/sites-available/medicart"

sudo tee "$NGINX_CONF" > /dev/null <<EOF
server {
    listen 80;
    server_name $DOMAIN www.$DOMAIN;

    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_http_version 1.1;

        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection 'upgrade';

        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
}
EOF

sudo ln -sf "$NGINX_CONF" /etc/nginx/sites-enabled/
sudo rm -f /etc/nginx/sites-enabled/default

sudo systemctl restart nginx
sudo systemctl enable nginx

### SSL (non-interactive certbot)
echo "Setting up SSL..."
sudo certbot --nginx \
    -d "$DOMAIN" -d "www.$DOMAIN" \
    --non-interactive \
    --agree-tos \
    -m "admin@$DOMAIN" \
    --redirect || true

### systemd service
echo "Creating systemd service..."

SERVICE_FILE="/etc/systemd/system/medicart.service"

sudo tee "$SERVICE_FILE" > /dev/null <<EOF
[Unit]
Description=Medicart Server
After=network.target

[Service]
User=ubuntu
WorkingDirectory=$WEB_DIR
ExecStart=$WEB_DIR/medicart-server-ubuntu
Restart=always

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reexec
sudo systemctl daemon-reload
sudo systemctl enable medicart

### Build and start service
echo "Building web server..."
ensure_go_path
cd "$WEB_DIR"
go mod tidy
go build -o medicart-server-ubuntu .

echo "Starting service..."
sudo systemctl restart medicart

### Done
echo "Setup complete!!!"
echo "Your app should be running and accessible at https://$DOMAIN"
