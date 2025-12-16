# Medicart Device Bridge Server

This is a Go web server that acts as a WebSocket bridge for the `lepu_cli.exe` medical device tool. It spawns the CLI tool in various modes and streams the parsed data to connected WebSocket clients.

## Prerequisites

- **Go** installed.
- The `lepu_cli.exe` tool must be in your system PATH or in the same directory as the server executable.

## Installation

1.  Initialize the module (if not already done):
    ```bash
    go mod tidy
    ```

## Usage

1.  Start the server:
    ```bash
    go run main.go
    ```
    The server listens on port `8080`.

2.  Connect via WebSocket to the desired endpoint:

    -   **Heart Rate / SpO2**: `ws://localhost:8080/api/heartrate`
        -   Runs: `lepu_cli.exe -heartrate`
        -   Returns: `{"type":"data", "pr":75, "spo2":98}` or status messages.

    -   **NIBP (Blood Pressure)**: `ws://localhost:8080/api/nibp`
        -   Runs: `lepu_cli.exe -nibp`
        -   Returns: `{"type":"cuff_update", "cuff_pressure":120}` and final result.

    -   **Glucose**: `ws://localhost:8080/api/glucose`
        -   Runs: `lepu_cli.exe -glu`
        -   Returns: `{"type":"data", "glu":105}`

    -   **Temperature**: `ws://localhost:8080/api/temperature`
        -   Runs: `lepu_cli.exe -temperature`
        -   Returns: `{"type":"data", "temp":36.5}`

## Behavior

-   **Auto-Kill**: When the WebSocket connection is closed (client disconnects), the server automatically terminates the underlying `lepu_cli.exe` process.
-   **Parsing**: The server parses the raw stdout from the CLI tool and sends structured JSON messages.

built for windows32 using:
GOOS=windows GOARCH=386 /usr/local/go/bin/go build -o medicart-server.exe main.go