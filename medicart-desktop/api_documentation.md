# Medicart API Documentation

This document outlines the API endpoints provided by the Medicart web server (port 8081) for the web developer.

## Base URL
`http://localhost:8081`

---

## Endpoints

### 1. List Clinics
Returns a list of all clinics available in the data storage.

- **URL:** `${API_BASE}/api/clinics` (Alias: `/clinics`)
- **Method:** `GET`
- **Response:** `string[]`
  - Example: `["ClinicA", "ClinicB"]`

### 2. List Patients in Clinic
Returns a list of all patients within a specific clinic.

- **URL:** `${API_BASE}/api/clinic/{clinic}/patients`
- **Method:** `GET`
- **Response:** `string[]`
  - Example: `["Patient123", "Patient456"]`

### 3. Get Patient Data
Returns all recorded sensor data for a specific patient, grouped by metric type.

- **URL:** `${API_BASE}/api/clinic/{clinic}/patient/{patient}/data`
- **Method:** `GET`
- **Response:** `Record<string, RecordEntry[]>`
  - Keys are filenames (e.g., `heart_rate.json`, `bp.json`).
  - Values are arrays of records.
  - **RecordEntry Base Schema:**
    ```typescript
    {
      "timestamp": string, // ISO 8601
      "patient_name": string,
      "clinic_name": string,
      "data": SensorData // See specific sensor schemas below
    }
    ```

---

## Sensor Data Schemas (`"data"` object)

The `"data"` field within a [RecordEntry](file:///Users/devipeepive/Desktop/projects/medicart/medicart-web/app/page.tsx#5-11) contains the raw measurements. Below are the structures for each sensor type.

### 1. Heart Rate / SpO2
Stored in `heart_rate.json`.
```json
{
  "type": "data",
  "pr": 75,   // Pulse Rate (integer)
  "spo2": 98  // Oxygen Saturation (percentage)
}
```
*Note: May also receive `{"type": "status", "msg": "..."}` for hardware alerts.*

### 2. Blood Pressure (NIBP)
Stored in `bp.json`.
**Final Result:**
```json
{
  "type": "result",
  "sys": 120, // Systolic (mmHg)
  "dia": 80,  // Diastolic (mmHg)
  "map": 93,  // Mean Arterial Pressure (mmHg)
  "pr": 72,   // Pulse Rate
  "irr": false // Irregular heartbeat detected
}
```
**Live Cuff Pressure (during measurement):**
```json
{
  "type": "cuff_update",
  "cuff_pressure": 150
}
```

### 3. Blood Glucose
Stored in `glucose.json`.
```json
{
  "type": "data",
  "glu": 105 // Glucose level (integer, mg/dL)
}
```

### 4. Body Temperature
Stored in `temperature.json`.
```json
{
  "type": "data",
  "temp": 36.6 // Temperature (float, Celsius)
}
```

### 5. Stethoscope
Stored in `stethoscope.json`.
**Stream Data:**
```json
{
  "type": "stream",
  "stream_type": "audio" | "heartrate",
  "data": [123, 456, ...], // Array of int16 (if audio)
  "value": 75              // Pulse rate (if heartrate)
}
```

---

### 4. Camera Control
Sends a movement or control command to the desktop application's camera.

- **URL:** `${API_BASE}/api/camera/control`
- **Method:** `POST`
- **Request Body:**
  ```json
  { "command": "string" }
  ```
  - Common commands: `move-left`, `move-right`, `move-up`, `move-down`, `start`, `stop`.
- **Response:** `{"status": "ok"}`

### 5. Camera Stream (WebSocket)
Provides a live binary stream of JPEG frames from the desktop application.

- **URL:** `ws://localhost:8081/ws/stream?clinic={clinic}&patient={patient}`
- **Protocol:** WebSocket
- **Data Format:** Binary (JPEG bytes)
- **Note:** Connecting to this stream automatically sends a "start" command to the desktop application if it's connected.

### 6. Get Latest Camera Snapshot
Fetches the last saved camera snapshot for a patient.

- **URL:** `${API_BASE}/api/clinic/{clinic}/patient/{patient}/camera`
- **Method:** `GET`
- **Response:** Image (`image/jpeg`)

---

## Data Ingestion (Internal/Desktop)

### Ingest Data
Used by the desktop application to upload sensor readings.

- **URL:** `${API_BASE}/api/ingest`
- **Method:** `POST`
- **Request Body:**
  ```json
  {
    "patient_name": "string",
    "clinic_name": "string",
    "data": { ... }
  }
  ```

---

## Missing Information & Limitations

> [!WARNING]
> **Authentication & Security**
> There is currently **no authentication** or **CORS restriction** beyond basic preflight handling. Any client can access clinic and patient data if they know the URL.


> [!NOTE]
> **Error Handling**
> API errors currently return generic text messages with standard HTTP status codes (400, 404, 500). There is no structured JSON error format.

> [!CAUTION]
> **Storage Structure**
> The server relies on a file-based directory structure (`data/{clinic}/{patient}/{metric}.json`). Any manual changes to this directory might affect the API responses.
> This will be changed to a cloud based solution later.