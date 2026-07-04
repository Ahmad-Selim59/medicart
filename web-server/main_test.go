package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
			name: "Patient Nested in Data",
			payload: map[string]interface{}{
				"patient_name": "Test",
				"data": map[string]interface{}{
					"type":   "profile",
					"gender": "Male",
					"age":    24,
				},
			},
			expected: "profile",
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

func TestBuildPatientProfileFromMisc(t *testing.T) {
	os.RemoveAll("data")
	defer os.RemoveAll("data")

	record := Record{
		Timestamp:   time.Now(),
		PatientName: "Ahmad Selim",
		ClinicName:  "saab",
		RawData: map[string]interface{}{
			"patient_name": "Ahmad Selim",
			"clinic_name":  "saab",
			"data": map[string]interface{}{
				"type":   "profile",
				"gender": "Male",
				"age":    24,
				"weight": 83.0,
				"height": 178.0,
			},
		},
	}

	if err := saveRecord(record); err != nil {
		t.Fatalf("saveRecord failed: %v", err)
	}

	patient := buildPatient("saab", "Ahmad Selim")
	if patient.Gender != "Male" {
		t.Fatalf("expected gender Male, got %q", patient.Gender)
	}
	if patient.Age != 24 {
		t.Fatalf("expected age 24, got %d", patient.Age)
	}
	if patient.Weight != 83 {
		t.Fatalf("expected weight 83, got %v", patient.Weight)
	}
	if patient.Height != 178 {
		t.Fatalf("expected height 178, got %v", patient.Height)
	}
	if misc, ok := patient.Data["misc"]; ok {
		t.Fatalf("expected profile records to be excluded from misc data, got %v", misc)
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

func TestPruneMetricReadings(t *testing.T) {
	os.RemoveAll("data")
	defer os.RemoveAll("data")

	dir := filepath.Join("data", "TestClinic", "Ahmad")
	base := time.Unix(1700000000, 0)

	for i := 0; i < 5; i++ {
		record := Record{
			Timestamp:   base.Add(time.Duration(i) * time.Minute),
			PatientName: "Ahmad",
			ClinicName:  "TestClinic",
			RawData: map[string]interface{}{
				"pr":   70 + i,
				"spo2": 98,
			},
		}
		if err := saveRecord(record); err != nil {
			t.Fatalf("saveRecord %d failed: %v", i, err)
		}
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	var hrFiles []string
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "heart_rate_") {
			hrFiles = append(hrFiles, f.Name())
		}
	}
	if len(hrFiles) != 3 {
		t.Fatalf("expected 3 heart_rate files after pruning, got %d: %v", len(hrFiles), hrFiles)
	}

	patient := buildPatient("TestClinic", "Ahmad")
	hr, ok := patient.Data["heartRate"].([]interface{})
	if !ok {
		t.Fatalf("expected heartRate data")
	}
	if len(hr) != 3 {
		t.Fatalf("expected 3 heartRate readings in API response, got %d", len(hr))
	}
}

func TestPruneMetricReadingsKeepsFewerThanLimit(t *testing.T) {
	os.RemoveAll("data")
	defer os.RemoveAll("data")

	dir := filepath.Join("data", "TestClinic", "Ahmad")
	base := time.Unix(1700000000, 0)

	for i := 0; i < 2; i++ {
		record := Record{
			Timestamp:   base.Add(time.Duration(i) * time.Minute),
			PatientName: "Ahmad",
			ClinicName:  "TestClinic",
			RawData: map[string]interface{}{
				"pr":   70 + i,
				"spo2": 98,
			},
		}
		if err := saveRecord(record); err != nil {
			t.Fatalf("saveRecord %d failed: %v", i, err)
		}
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	var hrFiles []string
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "heart_rate_") {
			hrFiles = append(hrFiles, f.Name())
		}
	}
	if len(hrFiles) != 2 {
		t.Fatalf("expected 2 heart_rate files when under limit, got %d", len(hrFiles))
	}

	patient := buildPatient("TestClinic", "Ahmad")
	hr, ok := patient.Data["heartRate"].([]interface{})
	if !ok || len(hr) != 2 {
		t.Fatalf("expected 2 heartRate readings in API response, got %v", patient.Data["heartRate"])
	}
}
