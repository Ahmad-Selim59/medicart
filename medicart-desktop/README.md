# Medicart Uploader

A desktop application built with [Fyne](https://fyne.io/) to monitor medical sensors and upload patient data to a remote web server.

This application bridges the **ZUG TR8 patient monitor** (HL7 over UDP) and your central health record system. Manual **glucose** entry and **ECG image upload** (when the monitor is off) are also supported.

## Features

- **Graphical User Interface**: Easy-to-use desktop interface with Light/Dark mode support.
- **TR8 vitals**: Heart rate, SpO2, NIBP, and **temperature** from the monitor HL7 stream (`ORU^R01`); tap a button to save one snapshot to the server.
- **Manual glucose** on the Readings tab.
- **Data Ingestion**: Structured JSON to a configurable HTTP `/api/ingest` endpoint.
- **Patient Association**: Patient/clinic from HL7 with Settings fallbacks.
- **Real-time Status**: Live vitals cache, connection status, Live Console.
- **ZUG TR8 monitor (HL7 UDP)**: Listens on UDP port **5500** by default (configurable in Settings). **ECG** is buffered from **ORU^W01** waveforms; use **Save ECG from monitor** for a PNG strip. Live W01 preview and debug stats appear on the Readings tab when the monitor is enabled.

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

**Temperature** on the Readings card comes from TR8 `MDC_TEMP` (150344) in HL7. Values are normalized to **°C** using OBX-6 units when present (e.g. `MDC_DIM_DEGC`, `MDC_DIM_DEGF`); missing units are treated as Celsius. **Weight** and **height** from patient OBX are stored as **kg** and **cm** with the same unit handling. If temp shows **—**, the stream is sending `-99.9` / no probe value—use the probe on the monitor so R01 includes a valid reading.

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
3.  **Optional dependencies** (Windows): For Comms tab camera control/preview, place `camera_cli.exe` in `dependencies/` next to the desktop executable (or on the system PATH).

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

Or build:

```bash
go build -o medicart-desktop .
```

## Configuration

Settings are stored in `~/.medicart/config.json` (or `%USERPROFILE%\.medicart\config.json` on Windows).

Key fields:

- `server_base`: Base URL of the web server (e.g. `http://localhost:8080`)
- `monitor_enabled`: TR8 UDP listener on/off
- `monitor_udp_port`: Default **5500**
- `clinic_name`, `patient_name`: Fallbacks when HL7 omits them

## API Payload Examples

See `api_documentation.md` for ingest JSON shapes (vitals, ECG, glucose, profile).

## Legacy Code

The original WebSocket-based server implementation has been moved to the `legacy/` directory.
