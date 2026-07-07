package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

const maxReadingsPerMetric = 3

// Readings arriving within this window are treated as one measurement session.
const readingSessionGap = 45 * time.Second

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

	// feedConns maps clinicName -> websocket.Conn (camera)
	feedConns = make(map[string]*websocket.Conn)
	wsMutex   sync.Mutex

	streams   = make(map[string]map[*streamSubscriber]bool) // key: clinic|patient (camera)
	streamsMu sync.Mutex

	// audioFeedConns maps clinicName -> websocket.Conn (mic feed from desktop)
	audioFeedConns = make(map[string]*websocket.Conn)
	audioFeedMu    sync.Mutex

	audioStreams   = make(map[string]map[*websocket.Conn]bool) // key: clinic (audio subscribers)
	audioStreamsMu sync.Mutex
)

func main() {
	// Try to find the .env file in the executable's directory
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		_ = godotenv.Load(filepath.Join(exeDir, ".env"))
	}

	// Also try loading from current working directory as fallback
	_ = godotenv.Load()

	loadConfig()

	ensureStorageFile()
	initStorage()

	http.HandleFunc("/api/ingest", handleIngest)
	http.HandleFunc("/ws/feed", handleFeedWS)
	http.HandleFunc("/ws/stream", handleStreamWS)
	http.HandleFunc("/ws/audio-feed", handleAudioFeedWS)
	http.HandleFunc("/ws/audio-stream", handleAudioStreamWS)
	http.HandleFunc("/ws/chat", handleChatWS)
	http.HandleFunc("/api/feed/start", handleFeedStart)
	http.HandleFunc("/api/feed/stop", handleFeedStop)
	http.HandleFunc("/api/clinics", authMiddleware(handleClinics))
	http.HandleFunc("/clinics", authMiddleware(handleClinics))
	http.HandleFunc("/api/camera/control", handleCameraControl)
	http.HandleFunc("/api/mic/control", handleMicControl)
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

	if t, ok := data["type"].(string); ok && t == "ecg" {
		imageB64, _ := data["image"].(string)
		mime, _ := data["image_mime"].(string)
		if strings.TrimSpace(imageB64) == "" {
			http.Error(w, "ECG image required", http.StatusBadRequest)
			return
		}
		if err := validateChatImage(imageB64, mime); err != nil {
			http.Error(w, "Invalid ECG image: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if t, ok := data["type"].(string); ok && t == "profile" {
		saveProfileFromIngest(w, patientName, clinicName, data)
		return
	}
	if nested, ok := data["data"].(map[string]interface{}); ok {
		if t, ok := nested["type"].(string); ok && t == "profile" {
			saveProfileFromIngest(w, patientName, clinicName, nested)
			return
		}
	}

	record := Record{
		Timestamp:   time.Now(),
		PatientName: patientName,
		ClinicName:  clinicName,
		RawData:     data,
	}

	if !shouldPersistReading(data) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Ignored non-persistent reading")
		return
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

func saveProfileFromIngest(w http.ResponseWriter, patientName, clinicName string, profileData map[string]interface{}) {
	if strings.TrimSpace(patientName) == "" || patientName == "Unknown" {
		http.Error(w, "patient_name required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(clinicName) == "" || clinicName == "Unknown" {
		http.Error(w, "clinic_name required", http.StatusBadRequest)
		return
	}
	record := Record{
		Timestamp:   time.Now(),
		PatientName: patientName,
		ClinicName:  clinicName,
		RawData:     profileData,
	}
	if err := savePatientProfile(record); err != nil {
		log.Printf("Error saving patient profile: %v", err)
		http.Error(w, "Failed to save profile", http.StatusInternalServerError)
		return
	}
	log.Printf("Saved profile for patient: %s", patientName)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Profile saved successfully")
}

func saveRecord(record Record) error {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	dir := filepath.Join("data", safe(record.ClinicName), safe(record.PatientName))

	baseFilename := metricFile(record.RawData)
	if baseFilename == "" {
		baseFilename = "misc"
	}

	if latestFile, latestRecord, ok := findLatestMetricRecord(dir, baseFilename); ok {
		gap := record.Timestamp.Sub(latestRecord.Timestamp)
		if gap >= 0 && gap < readingSessionGap {
			path := filepath.Join(dir, latestFile)
			return writeMetricRecordFile(path, record)
		}
	}

	filename := fmt.Sprintf("%s_%d.json", baseFilename, record.Timestamp.Unix())
	path := filepath.Join(dir, filename)

	if err := writeMetricRecordFile(path, record); err != nil {
		return err
	}
	return pruneMetricReadings(dir, baseFilename, maxReadingsPerMetric)
}

func metricFilenameTS(filename string) int64 {
	name := strings.TrimSuffix(filename, ".json")
	lastIdx := strings.LastIndex(name, "_")
	if lastIdx == -1 {
		return 0
	}
	ts, err := strconv.ParseInt(name[lastIdx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

func pruneMetricReadings(dir, metric string, keep int) error {
	if keep <= 0 {
		return nil
	}

	files, err := listFiles(dir)
	if err != nil {
		return err
	}

	prefix := metric + "_"
	var metricFiles []string
	for _, name := range files {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") {
			metricFiles = append(metricFiles, name)
		}
	}
	if len(metricFiles) <= keep {
		return nil
	}

	sort.Slice(metricFiles, func(i, j int) bool {
		return metricFilenameTS(metricFiles[i]) < metricFilenameTS(metricFiles[j])
	})

	for _, name := range metricFiles[:len(metricFiles)-keep] {
		if err := deleteFile(filepath.Join(dir, name)); err != nil {
			log.Printf("prune: failed to delete %s: %v", name, err)
		}
	}
	return nil
}

func savePatientProfile(record Record) error {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	dir := filepath.Join("data", safe(record.ClinicName), safe(record.PatientName))
	path := filepath.Join(dir, "profile.json")

	profile := PatientProfile{
		PatientName: record.PatientName,
		ClinicName:  record.ClinicName,
		Gender:      profileString(record.RawData, "gender"),
		Age:         profileInt(record.RawData, "age"),
		Weight:      profileFloat(record.RawData, "weight"),
		Height:      profileFloat(record.RawData, "height"),
		UpdatedAt:   record.Timestamp.UTC().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	return saveFile(path, data)
}

func profileString(data map[string]interface{}, key string) string {
	if v, ok := data[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func profileInt(data map[string]interface{}, key string) int {
	switch v := data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}

func profileFloat(data map[string]interface{}, key string) float64 {
	switch v := data[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	default:
		return 0
	}
}

func ensureDataDir() {
	_ = os.MkdirAll("data", 0755)
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 256 * 1024,
}

type streamSubscriber struct {
	conn *websocket.Conn
	mu   sync.Mutex
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
					currentClinic = safe(meta.Clinic)

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

func removeStreamSubscriber(key string, sub *streamSubscriber) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	if m := streams[key]; m != nil {
		delete(m, sub)
		if len(m) == 0 {
			delete(streams, key)
		}
	}
}

func broadcastFrame(key string, frame []byte) {
	streamsMu.Lock()
	room := streams[key]
	if len(room) == 0 {
		streamsMu.Unlock()
		return // drop if no subscribers
	}
	targets := make([]*streamSubscriber, 0, len(room))
	for sub := range room {
		targets = append(targets, sub)
	}
	streamsMu.Unlock()

	// Copy once; fan out without blocking the feed read loop on slow clients.
	payload := append([]byte(nil), frame...)
	for _, sub := range targets {
		go func(s *streamSubscriber, data []byte) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := s.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				log.Printf("Stream WS write error for %s: %v", key, err)
				s.conn.Close()
				removeStreamSubscriber(key, s)
			}
		}(sub, payload)
	}
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
	sub := &streamSubscriber{conn: conn}

	streamsMu.Lock()
	if streams[key] == nil {
		streams[key] = make(map[*streamSubscriber]bool)
	}
	streams[key][sub] = true
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

	removeStreamSubscriber(key, sub)
	conn.Close()

	// Nurse controls broadcast lifecycle via End Stream on the desktop app.
	// Do not send "stop" when a viewer disconnects — a brief WS hiccup was
	// killing the entire clinic feed after the first frame.

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
	case lowerKeys["type"] && payload["type"] == "profile":
		return "profile"
	case lowerKeys["type"] && payload["type"] == "ecg":
		return "ecg"
	case lowerKeys["sys"] || lowerKeys["dia"]:
		return "bp"
	case lowerKeys["pr"] || lowerKeys["spo2"]:
		return "heart_rate"
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

// --- Audio streaming ---

// broadcastAudio forwards a raw PCM chunk to all audio-stream subscribers for a clinic.
func broadcastAudio(clinic string, data []byte) {
	audioStreamsMu.Lock()
	conns := audioStreams[clinic]
	if len(conns) == 0 {
		audioStreamsMu.Unlock()
		return
	}
	for c := range conns {
		if err := c.WriteMessage(websocket.BinaryMessage, data); err != nil {
			c.Close()
			delete(conns, c)
		}
	}
	if len(conns) == 0 {
		delete(audioStreams, clinic)
	}
	audioStreamsMu.Unlock()
}

// handleAudioFeedWS is the desktop-side audio producer endpoint (/ws/audio-feed).
// The desktop registers its clinic via a JSON text message, then streams raw PCM
// as binary frames. Binary frames from the browser (doctor mic) are also forwarded here.
func handleAudioFeedWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Audio feed WS upgrade error: %v", err)
		return
	}
	var currentClinic string
	log.Printf("Audio feed WS connected")

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Audio feed WS read error: %v", err)
			break
		}
		if mt == websocket.BinaryMessage {
			// Patient mic audio → broadcast to browser subscribers
			broadcastAudio(safe(currentClinic), msg)
		} else {
			var meta struct {
				Clinic string `json:"clinic_name"`
			}
			if err := json.Unmarshal(msg, &meta); err == nil && meta.Clinic != "" {
				old := currentClinic
				currentClinic = meta.Clinic
				audioFeedMu.Lock()
				if old != "" && old != currentClinic {
					delete(audioFeedConns, old)
				}
				audioFeedConns[currentClinic] = conn
				audioFeedMu.Unlock()
				log.Printf("Audio feed WS registered for clinic: %s", currentClinic)
			}
		}
	}

	if currentClinic != "" {
		audioFeedMu.Lock()
		if audioFeedConns[currentClinic] == conn {
			delete(audioFeedConns, currentClinic)
			log.Printf("Audio feed WS disconnected for clinic: %s", currentClinic)
		}
		audioFeedMu.Unlock()
	}
	conn.Close()
}

// handleAudioStreamWS is the browser subscriber endpoint (/ws/audio-stream?clinic=...).
// It receives patient mic audio as binary and can also send doctor mic audio back.
func handleAudioStreamWS(w http.ResponseWriter, r *http.Request) {
	clinic := r.URL.Query().Get("clinic")
	if clinic == "" {
		http.Error(w, "clinic required", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Audio stream WS upgrade error: %v", err)
		return
	}

	audioStreamsMu.Lock()
	if audioStreams[clinic] == nil {
		audioStreams[clinic] = make(map[*websocket.Conn]bool)
	}
	audioStreams[clinic][conn] = true
	audioStreamsMu.Unlock()

	log.Printf("Audio stream subscriber connected: %s", clinic)

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			// Doctor mic audio → forward to desktop via audio-feed connection
			audioFeedMu.Lock()
			desktop, ok := audioFeedConns[clinic]
			audioFeedMu.Unlock()
			if ok {
				_ = desktop.WriteMessage(websocket.BinaryMessage, msg)
			}
		}
	}

	audioStreamsMu.Lock()
	if m := audioStreams[clinic]; m != nil {
		delete(m, conn)
		if len(m) == 0 {
			delete(audioStreams, clinic)
		}
	}
	audioStreamsMu.Unlock()
	conn.Close()
	log.Printf("Audio stream subscriber disconnected: %s", clinic)
}

// handleMicControl sends mic-on / mic-off commands to the desktop via the existing feed WS.
func handleMicControl(w http.ResponseWriter, r *http.Request) {
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

// Handlers are now in api.go

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
