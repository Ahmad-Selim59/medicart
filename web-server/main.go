package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
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
)

func main() {
	ensureStorageFile()

	http.HandleFunc("/api/ingest", handleIngest)

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
