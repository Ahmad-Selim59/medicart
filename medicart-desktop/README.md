# Medicart Uploader

A desktop application built with [Fyne](https://fyne.io/) to monitor medical sensors and upload patient data to a remote web server.

This application acts as a bridge between local medical devices (via `lepu_cli.exe`) and your central health record system.

## Features

- **Graphical User Interface**: Easy-to-use desktop interface with Light/Dark mode support.
- **Device Support**: Interfaces with Heart Rate/SpO2, NIBP (Blood Pressure), Glucose, and Temperature sensors.
- **Data Ingestion**: Parses raw device data and sends structured JSON to a specified HTTP endpoint.
- **Patient Association**: Allows tagging readings with a specific Patient Name.
- **Real-time Status**: Visual feedback and error highlighting (red for errors).
- **ZUG TR8 monitor (HL7 UDP)**: Listens on UDP port **5500** by default (configurable in Settings); live values on the Readings tab; tap a vital button to save one snapshot.

### TR8 HL7 setup (Windows)

1. On the monitor, set the HL7 **destination** to this PC’s LAN IP and **UDP port 5500** (must match Settings → TR8 Monitor → UDP port).
2. In Medicart **Settings**, set **Clinic Name** and **Server Base URL**. The TR8 usually does **not** send a clinic name; vitals uploads use this clinic when HL7 has none.
3. Leave **Allow source IP** empty unless you intentionally filter one monitor IP.
4. Allow inbound **UDP** (your chosen port, default **5500**) in Windows Firewall for **this app’s .exe**. Example (Admin PowerShell, adjust the path):

   ```powershell
   New-NetFirewallRule -DisplayName "Medicart HL7 UDP" -Direction Inbound -Protocol UDP -LocalPort 5500 -Action Allow -Program "C:\path\to\medicart-desktop-windows-386.exe"
   ```

5. **Allow source IP** in Settings must be **empty** unless you intentionally filter one monitor address. You do **not** need to know the monitor IP for normal operation.
6. On Readings, after a few seconds you should see a **UDP self-test** line in Live Console. If **packet count** stays 0 but self-test ran, it is almost always firewall or another program still bound to the same UDP port (close PowerShell test listeners).

### “Access forbidden” when binding UDP (Windows)

Windows often **reserves** UDP port **5000** (Hyper-V, WSL2, Docker). The app defaults to **5500** for that reason. If bind still fails, pick another port in Settings and on the TR8 (must match).

**If you need a different port:**

1. Medicart **Settings → TR8 Monitor → UDP port** → e.g. `9000` → **Save Settings**
2. On the TR8, set the HL7 **destination port** to the same value.
3. Add a firewall rule for that UDP port if needed.

To inspect Windows reserved ranges (Admin PowerShell):

```powershell
netsh interface ipv4 show excludedportrange protocol=udp
```

Pick a port **not** inside any `Start Port`–`End Port` range listed there.

## Prerequisites

1.  **Go** (1.20 or later recommended).
2.  **C Compiler**: Required by Fyne.
    *   **Windows**: MSYS2 with Mingw-w64 or TDM-GCC.
    *   **macOS**: Xcode Command Line Tools (`xcode-select --install`).
    *   **Linux**: GCC (`sudo apt install gcc`).
3.  **Device CLIs** (Windows): Place executables and their full publish output in `dependencies/` next to the desktop app:

    ```
    medicart-desktop-windows-386/
    ├── medicart-desktop-windows-386.exe
    └── dependencies/
        ├── lepu_cli.exe
        ├── lepu_cli.dll
        ├── camera_cli.exe
        ├── MinttiCLI.exe          (optional — stethoscope only)
        └── runtimes/              (required by lepu_cli — keep inside dependencies/)
    ```

    Paths are resolved from the folder containing the desktop executable. Each CLI runs with `dependencies/` as its working directory so DLLs and `runtimes/` load correctly. CLIs can also be placed on the system PATH.

## Installation

1.  Clone the repository.
2.  Install dependencies:
    ```bash
    go mod tidy
    ```

## Running the App

To run the application directly:

```bash
go run .
```

To build a standalone executable:

```bash
go build -o MedicartUploader .
```

to build in windows 32 bit:
```bash
GOOS=windows GOARCH=386 CGO_ENABLED=1 \
  CC=i686-w64-mingw32-gcc \
  CXX=i686-w64-mingw32-g++ \
  go build -o medicart-desktop-windows-386.exe .
```

## Usage

1.  **Web Server URL**: Enter the full URL of your backend API endpoint (e.g., `http://myserver.com/api/readings`).
2.  **Patient Name**: Enter the name of the patient currently being examined. This field is required.
3.  **Start Monitoring**: Click the button corresponding to the sensor you want to use (e.g., "Start Heart Rate / SpO2").
4.  **Stop**: Click the "Stop" button to end the current session.

## Data Format

The application sends HTTP POST requests with a JSON body. All payloads include a `patient_name` field.

### Heart Rate / SpO2
```json
{
  "type": "data",
  "pr": 75,
  "spo2": 98,
  "patient_name": "John Doe"
}
```

### NIBP (Blood Pressure)
**Intermediate Updates (Cuff Pressure):**
```json
{
  "type": "cuff_update",
  "cuff_pressure": 120,
  "patient_name": "John Doe"
}
```

**Final Result:**
```json
{
  "type": "result",
  "sys": 120,
  "dia": 80,
  "map": 93,
  "pr": 70,
  "irr": false,
  "patient_name": "John Doe"
}
```

### Glucose
```json
{
  "type": "data",
  "glu": 105,
  "patient_name": "John Doe"
}
```

### Temperature
```json
{
  "type": "data",
  "temp": 36.5,
  "patient_name": "John Doe"
}
```

## Legacy Code

The original WebSocket-based server implementation has been moved to the `legacy/` directory.

