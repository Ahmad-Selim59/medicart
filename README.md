# Medicart

## medicart-desktop

Fyne desktop app for monitoring medical sensors and uploading data to the web server.

**Prerequisites:** Go 1.20+, C compiler (Xcode Command Line Tools on Mac), and `lepu_cli.exe` on the target machine.

### Build locally on macOS

```bash
cd medicart-desktop
go mod tidy
go build -o MedicartUploader .
```

Run without building:

```bash
cd medicart-desktop
go run .
```

### Cross-compile for Windows 32-bit (from macOS)

Install the MinGW cross-compiler first:

```bash
brew install mingw-w64
```

Then build:

```bash
cd medicart-desktop
go mod tidy
GOOS=windows GOARCH=386 CGO_ENABLED=1 \
  CC=i686-w64-mingw32-gcc \
  CXX=i686-w64-mingw32-g++ \
  go build -o medicart-desktop-windows-386.exe .
```

---

## web-server

Go HTTP/WebSocket API that receives and stores patient readings.

**Prerequisites:** Go 1.24+. Copy `.env` into `web-server/` before running (see `web-server/.env`).

### Build locally on macOS

```bash
cd web-server
go mod tidy
go build -o medicart-server .
```

Run without building:

```bash
cd web-server
go run .
```

### Cross-compile for Ubuntu (from macOS)

```bash
cd web-server
go mod tidy
GOOS=linux GOARCH=amd64 go build -o medicart-server-ubuntu .
```

Copy `medicart-server-ubuntu` to the Ubuntu server (e.g. `/home/ubuntu/medicart/web-server/`) and restart the service:

```bash
sudo systemctl restart medicart
```
