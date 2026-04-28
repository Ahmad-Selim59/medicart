# Medicart Uploader

A desktop application built with [Fyne](https://fyne.io/) to monitor medical sensors and upload patient data to a remote web server.

This application acts as a bridge between local medical devices (via `lepu_cli.exe`) and your central health record system.

## Features

- **Graphical User Interface**: Easy-to-use desktop interface with Light/Dark mode support.
- **Device Support**: Interfaces with Heart Rate/SpO2, NIBP (Blood Pressure), Glucose, and Temperature sensors.
- **Data Ingestion**: Parses raw device data and sends structured JSON to a specified HTTP endpoint.
- **Patient Association**: Allows tagging readings with a specific Patient Name.
- **Real-time Status**: Visual feedback and error highlighting (red for errors).

## Prerequisites

1.  **Go** (1.20 or later recommended).
2.  **C Compiler**: Required by Fyne.
    *   **Windows**: MSYS2 with Mingw-w64 or TDM-GCC.
    *   **macOS**: Xcode Command Line Tools (`xcode-select --install`).
    *   **Linux**: GCC (`sudo apt install gcc`).
3.  **lepu_cli.exe**: The driver executable for the medical devices. This file must be present in the system PATH or in the same directory as the application.

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

