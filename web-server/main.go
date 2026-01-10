package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	RawData     map[string]interface{} `json:"data"`
}

var (
	storageFile = "data.json"
	fileMutex   sync.Mutex

	feedConn *websocket.Conn
	wsMutex  sync.Mutex
)

func main() {
	ensureStorageFile()
	ensureDataDir()

	http.HandleFunc("/api/ingest", handleIngest)
	http.HandleFunc("/ws/feed", handleFeedWS)
	http.HandleFunc("/api/feed/start", handleFeedStart)
	http.HandleFunc("/api/feed/stop", handleFeedStop)

	port := ":8081"
	fmt.Printf("Web Server starting on port %s...\n", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}

func handleIngest(w http.ResponseWriter, r *http.Request) {
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

	record := Record{
		Timestamp:   time.Now(),
		PatientName: patientName,
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

	if _, err := os.Stat(storageFile); os.IsNotExist(err) {
		initial := DataStorage{Records: []Record{}}
		saveStorage(initial)
	}
}

func saveRecord(record Record) error {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	storage, err := loadStorage()
	if err != nil {
		return err
	}

	storage.Records = append(storage.Records, record)

	return saveStorage(storage)
}

func loadStorage() (DataStorage, error) {
	var storage DataStorage
	
	fileBytes, err := os.ReadFile(storageFile)
	if err != nil {
		return storage, err
	}

	if len(fileBytes) == 0 {
		return DataStorage{Records: []Record{}}, nil
	}

	if err := json.Unmarshal(fileBytes, &storage); err != nil {
		return storage, err
	}

	return storage, nil
}

func saveStorage(storage DataStorage) error {
	data, err := json.MarshalIndent(storage, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(storageFile, data, 0644)
}

func ensureDataDir() {
	_ = os.MkdirAll("data", 0755)
}


var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleFeedWS(w http.ResponseWriter, r *http.Request) {
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
			path := filepath.Join("data", "last_frame.jpg")
			if err := os.WriteFile(path, msg, 0644); err != nil {
				log.Printf("Failed to write frame: %v", err)
			}
		} else {
			log.Printf("WS text: %s", string(msg))
		}
	}

	wsMutex.Lock()
	if feedConn == conn {
		feedConn = nil
	}
	wsMutex.Unlock()
}

func handleFeedStart(w http.ResponseWriter, r *http.Request) {
	if err := sendControl("start"); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "started")
}

func handleFeedStop(w http.ResponseWriter, r *http.Request) {
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
