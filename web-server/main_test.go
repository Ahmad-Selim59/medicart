package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetricFile(t *testing.T) {
	tests := []struct {
		name     string
		payload  map[string]interface{}
		expected string
	}{
		{
			name: "Heart Rate Top Level",
			payload: map[string]interface{}{
				"pr":   75,
				"spo2": 95,
			},
			expected: "heart_rate",
		},
		{
			name: "Heart Rate Nested in Data",
			payload: map[string]interface{}{
				"patient_name": "Test",
				"data": map[string]interface{}{
					"pr":   75,
					"spo2": 95,
				},
			},
			expected: "heart_rate",
		},
		{
			name: "Blood Pressure Nested",
			payload: map[string]interface{}{
				"patient_name": "Test",
				"data": map[string]interface{}{
					"sys": 120,
					"dia": 80,
				},
			},
			expected: "bp",
		},
		{
			name: "Stethoscope Audio Stream",
			payload: map[string]interface{}{
				"patient_name": "Test",
				"data": map[string]interface{}{
					"type":        "stream",
					"stream_type": "audio",
					"value":       []int{1, 2, 3},
				},
			},
			expected: "stethoscope",
		},
		{
			name: "ECG Image",
			payload: map[string]interface{}{
				"patient_name": "Test",
				"type":         "ecg",
				"image":        "abc",
				"image_mime":   "image/jpeg",
			},
			expected: "ecg",
		},
		{
			name: "Misc Data",
			payload: map[string]interface{}{
				"patient_name": "Test",
				"data": map[string]interface{}{
					"weight": 80,
					"height": 180,
				},
			},
			expected: "misc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := metricFile(tt.payload)
			if result != tt.expected {
				t.Errorf("expected %q but got %q", tt.expected, result)
			}
		})
	}
}

func TestSavePatientProfile(t *testing.T) {
	os.RemoveAll("data")
	defer os.RemoveAll("data")

	record := Record{
		Timestamp:   time.Now(),
		PatientName: "Ahmad",
		ClinicName:  "TestClinic",
		RawData: map[string]interface{}{
			"type":         "profile",
			"patient_name": "Ahmad",
			"clinic_name":  "TestClinic",
			"gender":       "Male",
			"age":          62,
			"weight":       85.0,
			"height":       180.0,
		},
	}

	if err := savePatientProfile(record); err != nil {
		t.Fatalf("savePatientProfile failed: %v", err)
	}

	profile := loadPatientProfile("TestClinic", "Ahmad")
	if profile.Gender != "Male" {
		t.Fatalf("expected gender Male, got %q", profile.Gender)
	}
	if profile.Age != 62 {
		t.Fatalf("expected age 62, got %d", profile.Age)
	}
	if profile.Weight != 85 {
		t.Fatalf("expected weight 85, got %v", profile.Weight)
	}
	if profile.Height != 180 {
		t.Fatalf("expected height 180, got %v", profile.Height)
	}

	patient := buildPatient("TestClinic", "Ahmad")
	if patient.Gender != "Male" || patient.Age != 62 || patient.Weight != 85 || patient.Height != 180 {
		t.Fatalf("unexpected patient profile: %+v", patient)
	}
}

func TestSaveRecord(t *testing.T) {
	// Create a temporary data directory
	os.RemoveAll("data")
	defer os.RemoveAll("data")

	now := time.Now()

	record := Record{
		Timestamp:   now,
		PatientName: "Ahmad",
		ClinicName:  "TestClinic",
		RawData: map[string]interface{}{
			"patient_name": "Ahmad",
			"clinic_name":  "TestClinic",
			"data": map[string]interface{}{
				"pr":   75,
				"spo2": 95,
			},
		},
	}

	err := saveRecord(record)
	if err != nil {
		t.Fatalf("saveRecord failed: %v", err)
	}

	dir := filepath.Join("data", "TestClinic", "Ahmad")

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	filename := files[0].Name()
	
	// Open file to verify contents
	b, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	
	var records []Record
	if err := json.Unmarshal(b, &records); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if len(records) != 1 {
		t.Errorf("expected 1 record in array, got %d", len(records))
	}
	
	// Read a second reading to ensure it writes to a separate file (provided time changes)
	secondTimeStr := "2026-03-30T20:00:00Z"
	secondTime, _ := time.Parse(time.RFC3339, secondTimeStr)
	record2 := record
	record2.Timestamp = secondTime
	
	_ = saveRecord(record2)
	files, _ = os.ReadDir(dir)
	
	if len(files) != 2 {
		t.Errorf("expected 2 files after second reading with different timestamp, got %d", len(files))
	}
}
