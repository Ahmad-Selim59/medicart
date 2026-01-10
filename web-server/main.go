package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type DataStorage struct {
	Records []Record `json:"records"`
}

type Record struct {
	Timestamp   time.Time              `json:"timestamp"`
	PatientName string                 `json:"patient_name"`
	ClinicName  string                 `json:"clinic_name"`
	RawData     map[string]interface{} `json:"data"`
}

var (
	storageFile = "data.json"
	fileMutex   sync.Mutex

	feedConn *websocket.Conn
	wsMutex  sync.Mutex

	streams   = make(map[string]map[*websocket.Conn]bool) // key: clinic|patient
	streamsMu sync.Mutex
)

func main() {
	ensureStorageFile()
	ensureDataDir()

	http.HandleFunc("/api/ingest", handleIngest)
	http.HandleFunc("/ws/feed", handleFeedWS)
	http.HandleFunc("/ws/stream", handleStreamWS) // clinic & patient query params
	http.HandleFunc("/api/feed/start", handleFeedStart)
	http.HandleFunc("/api/feed/stop", handleFeedStop)
	http.HandleFunc("/api/clinics", handleClinics)
	http.HandleFunc("/clinics", handleClinics) // simple alias
	http.HandleFunc("/api/clinic/", handleClinicRoutes)

	port := ":8081"
	fmt.Printf("Web Server starting on port %s...\n", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}

// --- CORS helpers ---
func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func preflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodOptions {
		setCORS(w)
		w.WriteHeader(http.StatusOK)
		return true
	}
	setCORS(w)
	return false
}

func handleIngest(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	patientName := "Unknown"
	if name, ok := data["patient_name"].(string); ok {
		patientName = name
	}
	clinicName := "Unknown"
	if name, ok := data["clinic_name"].(string); ok {
		clinicName = name
	}

	record := Record{
		Timestamp:   time.Now(),
		PatientName: patientName,
		ClinicName:  clinicName,
		RawData:     data,
	}

	if err := saveRecord(record); err != nil {
		log.Printf("Error saving record: %v", err)
		http.Error(w, "Failed to save data", http.StatusInternalServerError)
		return
	}

	log.Printf("Received data for patient: %s", patientName)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Data received successfully")
}

func ensureStorageFile() {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	// no-op legacy
}

func saveRecord(record Record) error {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	dir := filepath.Join("data", safe(record.ClinicName), safe(record.PatientName))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	filename := metricFile(record.RawData)
	if filename == "" {
		filename = "misc.json"
	}
	path := filepath.Join(dir, filename)

	var existing []Record
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &existing)
	}
	existing = append(existing, record)

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func ensureDataDir() {
	_ = os.MkdirAll("data", 0755)
}


var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleFeedWS(w http.ResponseWriter, r *http.Request) {
	var currentClinic = "Unknown"
	var currentPatient = "Unknown"

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	wsMutex.Lock()
	if feedConn != nil {
		feedConn.Close()
	}
	feedConn = conn
	wsMutex.Unlock()

	log.Printf("Feed WS connected")

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WS read error: %v", err)
			break
		}
		if mt == websocket.BinaryMessage {
			key := streamKey(currentClinic, currentPatient)
			broadcastFrame(key, msg)
		} else {
			// Expect JSON metadata: {"clinic_name": "...", "patient_name": "..."}
			var meta struct {
				Clinic string `json:"clinic_name"`
				Patient string `json:"patient_name"`
			}
			if err := json.Unmarshal(msg, &meta); err == nil {
				if meta.Clinic != "" {
					currentClinic = meta.Clinic
				}
				if meta.Patient != "" {
					currentPatient = meta.Patient
				}
			} else {
				log.Printf("WS text: %s", string(msg))
			}
		}
	}

	wsMutex.Lock()
	if feedConn == conn {
		feedConn = nil
	}
	wsMutex.Unlock()
}

func handleFeedStart(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	if err := sendControl("start"); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "started")
}

func handleFeedStop(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	if err := sendControl("stop"); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "stopped")
}

func sendControl(cmd string) error {
	wsMutex.Lock()
	defer wsMutex.Unlock()
	if feedConn == nil {
		return fmt.Errorf("no desktop connected")
	}
	if err := feedConn.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		feedConn = nil
		return fmt.Errorf("failed to send command: %w", err)
	}
	return nil
}

// --- Stream broker ---

func streamKey(clinic, patient string) string {
	return safe(clinic) + "|" + safe(patient)
}

func broadcastFrame(key string, frame []byte) {
	streamsMu.Lock()
	conns := streams[key]
	if len(conns) == 0 {
		streamsMu.Unlock()
		return // drop if no subscribers
	}
	for c := range conns {
		if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			c.Close()
			delete(conns, c)
		}
	}
	if len(conns) == 0 {
		delete(streams, key)
	}
	streamsMu.Unlock()
}

func handleStreamWS(w http.ResponseWriter, r *http.Request) {
	clinic := r.URL.Query().Get("clinic")
	patient := r.URL.Query().Get("patient")
	if clinic == "" || patient == "" {
		http.Error(w, "clinic and patient required", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS stream upgrade error: %v", err)
		return
	}
	key := streamKey(clinic, patient)

	streamsMu.Lock()
	if streams[key] == nil {
		streams[key] = make(map[*websocket.Conn]bool)
	}
	streams[key][conn] = true
	streamsMu.Unlock()

	log.Printf("Stream subscriber connected: %s", key)

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	streamsMu.Lock()
	if m := streams[key]; m != nil {
		delete(m, conn)
		if len(m) == 0 {
			delete(streams, key)
		}
	}
	streamsMu.Unlock()
	conn.Close()
	log.Printf("Stream subscriber disconnected: %s", key)
}

func metricFile(data map[string]interface{}) string {
	lowerKeys := map[string]bool{}
	for k := range data {
		lowerKeys[strings.ToLower(k)] = true
	}
	switch {
	case lowerKeys["pr"] || lowerKeys["spo2"]:
		return "heart_rate.json"
	case lowerKeys["sys"] || lowerKeys["dia"] || lowerKeys["cuff_pressure"]:
		return "bp.json"
	case lowerKeys["glu"]:
		return "glucose.json"
	case lowerKeys["temp"]:
		return "temperature.json"
	default:
		return "misc.json"
	}
}

func safe(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	return s
}

// --- Listing APIs ---
func handleClinics(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	entries, err := os.ReadDir("data")
	if err != nil {
		http.Error(w, "Failed to list clinics", http.StatusInternalServerError)
		return
	}
	var clinics []string
	for _, e := range entries {
		if e.IsDir() {
			clinics = append(clinics, e.Name())
		}
	}
	writeJSON(w, clinics)
}

// Routes under /api/clinic/{clinic}/...
func handleClinicRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/clinic/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	clinic := parts[0]
	if len(parts) == 1 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "patients":
		handlePatients(w, r, clinic)
	case "patient":
		if len(parts) >= 4 && parts[3] == "data" {
			patient := parts[2]
			handlePatientData(w, r, clinic, patient)
		} else if len(parts) >= 4 && parts[3] == "camera" {
			patient := parts[2]
			handlePatientCamera(w, r, clinic, patient)
		} else {
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func handlePatients(w http.ResponseWriter, r *http.Request, clinic string) {
	if preflight(w, r) {
		return
	}
	dir := filepath.Join("data", clinic)
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, "Failed to list patients", http.StatusInternalServerError)
		return
	}
	var patients []string
	for _, e := range entries {
		if e.IsDir() {
			patients = append(patients, e.Name())
		}
	}
	writeJSON(w, patients)
}

func handlePatientData(w http.ResponseWriter, r *http.Request, clinic, patient string) {
	if preflight(w, r) {
		return
	}
	dir := filepath.Join("data", clinic, patient)
	files, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, "Failed to read patient data", http.StatusInternalServerError)
		return
	}
	result := map[string]interface{}{}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if strings.HasSuffix(name, ".json") {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			var recs []Record
			if err := json.Unmarshal(b, &recs); err == nil {
				result[name] = recs
			}
		}
	}
	writeJSON(w, result)
}

func handlePatientCamera(w http.ResponseWriter, r *http.Request, clinic, patient string) {
	if preflight(w, r) {
		return
	}
	path := filepath.Join("data", clinic, patient, "camera.jpg")
	http.ServeFile(w, r, path)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
