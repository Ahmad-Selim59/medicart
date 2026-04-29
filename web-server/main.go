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
	"github.com/joho/godotenv"
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

	// feedConns maps clinicName -> websocket.Conn
	feedConns = make(map[string]*websocket.Conn)
	wsMutex   sync.Mutex

	streams   = make(map[string]map[*websocket.Conn]bool) // key: clinic|patient
	streamsMu sync.Mutex
)

func main() {
	// Try to find the .env file in the executable's directory
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		_ = godotenv.Load(filepath.Join(exeDir, ".env"))
	}

	// Also try loading from current working directory as fallback
	_ = godotenv.Load()

	ensureStorageFile()
	ensureDataDir()

	http.HandleFunc("/api/ingest", handleIngest)
	http.HandleFunc("/ws/feed", handleFeedWS)
	http.HandleFunc("/ws/stream", handleStreamWS) // clinic & patient query params
	http.HandleFunc("/api/feed/start", handleFeedStart)
	http.HandleFunc("/api/feed/stop", handleFeedStop)
	http.HandleFunc("/api/clinics", authMiddleware(handleClinics))
	http.HandleFunc("/clinics", authMiddleware(handleClinics))  // simple alias
	http.HandleFunc("/api/camera/control", handleCameraControl) // optionally protect this too? Let's leave it for now
	http.HandleFunc("/api/clinic/", authMiddleware(handleClinicRoutes))
	http.HandleFunc("/api/patient/", authMiddleware(handlePatientRoutes))
	http.HandleFunc("/api/patients", authMiddleware(handleAllPatients))
	http.HandleFunc("/api/dashboard", authMiddleware(handleDashboard))

	port := ":8081"
	fmt.Printf("Web Server starting on port %s...\n", port)

	// Wrap the default mux with CORS middleware
	handler := corsMiddleware(http.DefaultServeMux)

	if err := http.ListenAndServe(port, handler); err != nil {
		log.Fatal(err)
	}
}

// corsMiddleware handles CORS for all routes
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, apikey")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func preflight(w http.ResponseWriter, r *http.Request) bool {
	// Already handled by middleware, return false to continue to actual handler
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

	baseFilename := metricFile(record.RawData)
	if baseFilename == "" {
		baseFilename = "misc"
	}

	filename := fmt.Sprintf("%s_%d.json", baseFilename, record.Timestamp.Unix())
	path := filepath.Join(dir, filename)

	recordsToSave := []Record{record}

	data, err := json.MarshalIndent(recordsToSave, "", "  ")
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
	var currentClinic = ""

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	// Registration is deferred until the first text message identifying the clinic.

	log.Printf("Feed WS connected")

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WS read error: %v", err)
			break
		}
		if mt == websocket.BinaryMessage {
			broadcastFrame(safe(currentClinic), msg)
		} else {
			// Expect JSON metadata: {"clinic_name": "..."}
			var meta struct {
				Clinic string `json:"clinic_name"`
			}
			if err := json.Unmarshal(msg, &meta); err == nil {
				if meta.Clinic != "" {
					oldClinic := currentClinic
					currentClinic = meta.Clinic

					wsMutex.Lock()
					if oldClinic != "" && oldClinic != currentClinic {
						delete(feedConns, oldClinic)
					}
					feedConns[currentClinic] = conn
					wsMutex.Unlock()

					log.Printf("Feed WS registered for clinic: %s", currentClinic)
				}
			} else {
				log.Printf("WS text: %s", string(msg))
			}
		}
	}

	if currentClinic != "" {
		wsMutex.Lock()
		if feedConns[currentClinic] == conn {
			delete(feedConns, currentClinic)
			log.Printf("Feed WS disconnected for clinic: %s", currentClinic)
		}
		wsMutex.Unlock()
	}
}

func handleFeedStart(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	clinic := r.URL.Query().Get("clinic")
	if clinic == "" {
		http.Error(w, "clinic required", http.StatusBadRequest)
		return
	}
	if err := sendControl(clinic, "start"); err != nil {
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
	clinic := r.URL.Query().Get("clinic")
	if clinic == "" {
		http.Error(w, "clinic required", http.StatusBadRequest)
		return
	}
	if err := sendControl(clinic, "stop"); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "stopped")
}

func sendControl(clinic, cmd string) error {
	wsMutex.Lock()
	defer wsMutex.Unlock()
	conn, ok := feedConns[clinic]
	if !ok {
		return fmt.Errorf("no desktop connected for clinic: %s", clinic)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		delete(feedConns, clinic)
		return fmt.Errorf("failed to send command to %s: %w", clinic, err)
	}
	return nil
}

// Camera control endpoint: expects {"command":"move-left"} etc.
func handleCameraControl(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	var req struct {
		Clinic  string `json:"clinic_name"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Command == "" {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if err := sendControl(req.Clinic, req.Command); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Stream broker ---

func streamKey(clinic, patient string) string {
	c := safe(clinic)
	p := safe(patient)
	if p == "unknown" || p == "" {
		return c
	}
	return c + "|" + p
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
	if clinic == "" {
		http.Error(w, "clinic required", http.StatusBadRequest)
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

	// Attempt to start feed when a subscriber connects
	if err := sendControl(clinic, "start"); err != nil {
		log.Printf("feed start error (ignored): %v", err)
	}

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

	// If no subscribers remain for THIS clinic, try stopping feed
	streamsMu.Lock()
	remaining := len(streams[key])
	streamsMu.Unlock()
	if remaining == 0 {
		if err := sendControl(clinic, "stop"); err != nil {
			log.Printf("feed stop error (ignored): %v", err)
		}
	}

	log.Printf("Stream subscriber disconnected: %s", key)
}

func metricFile(data map[string]interface{}) string {
	payload := data
	if d, ok := data["data"].(map[string]interface{}); ok {
		payload = d
	}

	lowerKeys := map[string]bool{}
	for k := range payload {
		lowerKeys[strings.ToLower(k)] = true
	}
	switch {
	case lowerKeys["pr"] || lowerKeys["spo2"]:
		return "heart_rate"
	case lowerKeys["sys"] || lowerKeys["dia"] || lowerKeys["cuff_pressure"]:
		return "bp"
	case lowerKeys["glu"]:
		return "glucose"
	case lowerKeys["temp"]:
		return "temperature"
	case lowerKeys["type"] && payload["type"] == "stream" && (payload["stream_type"] == "audio" || payload["stream_type"] == "heartrate"):
		return "stethoscope"
	default:
		return "misc"
	}
}

func safe(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	return s
}

// Handlers are now in api.go

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
