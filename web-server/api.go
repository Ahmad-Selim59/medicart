package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Clinic struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Address      string `json:"address"`
	Phone        string `json:"phone"`
	Email        string `json:"email"`
	Website      string `json:"website"`
	Action       string `json:"action"`
	PatientCount int    `json:"patientCount"`
	Status       string `json:"status"`
}

type Patient struct {
	ID          string                 `json:"id"`
	ClinicID    string                 `json:"clinicId"`
	Name        string                 `json:"name"`
	Gender      string                 `json:"gender"`
	Age         int                    `json:"age"`
	Status      string                 `json:"status"`
	Action      string                 `json:"action"`
	LastChecked string                 `json:"lastChecked"`
	Data        map[string]interface{} `json:"data"`
}

func getAllClinics(allowedClinics []string) []Clinic {
	clinics := []Clinic{}
	clinicsMap := make(map[string]bool)

	// Filter out internal markers like __none__
	cleanAllowed := []string{}
	for _, name := range allowedClinics {
		if name != "" && name != "__none__" {
			cleanAllowed = append(cleanAllowed, name)
		}
	}

	// 1. Add all authorized clinics from Supabase (Source of Truth)
	for _, name := range cleanAllowed {
		if !clinicsMap[name] {
			clinics = append(clinics, Clinic{
				ID:           name,
				Name:         name,
				Address:      "Address N/A",
				Phone:        "N/A",
				Email:        "N/A",
				Website:      "N/A",
				Action:       "View",
				PatientCount: 0,
				Status:       "active",
			})
			clinicsMap[name] = true
		}
	}

	// 2. Add local folders (only if no authorized list provided at all)
	// We check len(allowedClinics) == 0 to see if the query param was missing
	entries, _ := os.ReadDir("data")
	for _, e := range entries {
		if e.IsDir() {
			name := e.Name()
			
			// count patients in folder
			patientEntries, _ := os.ReadDir(filepath.Join("data", name))
			patientCount := 0
			for _, pe := range patientEntries {
				if pe.IsDir() {
					patientCount++
				}
			}

			if !clinicsMap[name] {
				// ONLY fallback to local folders if NO allowed list was provided
				// This prevents unauthorized users from seeing everything
				if len(allowedClinics) == 0 {
					clinics = append(clinics, Clinic{
						ID:           name,
						Name:         name,
						Address:      "Address N/A",
						Phone:        "N/A",
						Email:        "N/A",
						Website:      "N/A",
						Action:       "View",
						PatientCount: patientCount,
						Status:       "active",
					})
					clinicsMap[name] = true
				}
			} else {
				// Update patient count for the already added clinic
				for i := range clinics {
					if clinics[i].Name == name {
						clinics[i].PatientCount = patientCount
						break
					}
				}
			}
		}
	}

	return clinics
}



func buildPatient(clinicName string, patientName string) Patient {
	dir := filepath.Join("data", clinicName, patientName)
	files, err := os.ReadDir(dir)

	realData := make(map[string]interface{})

	if err == nil {
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
					nameNoExt := strings.TrimSuffix(name, ".json")
					lastIdx := strings.LastIndex(nameNoExt, "_")
					key := nameNoExt
					if lastIdx != -1 {
						key = nameNoExt[:lastIdx]
					}
					if key == "heart_rate" {
						key = "heartRate"
					}
					if key == "bp" {
						key = "bloodPressure"
					}

					var innerData []interface{}
					for _, r := range recs {
						innerData = append(innerData, r.RawData)
					}
					if len(innerData) > 0 {
						if existing, ok := realData[key]; ok {
							realData[key] = append(existing.([]interface{}), innerData...)
						} else {
							realData[key] = innerData
						}
					}
				}
			}
		}
	}

	return Patient{
		ID:          patientName, // use name as ID
		ClinicID:    clinicName,
		Name:        patientName,
		Gender:      "Unknown",
		Age:         0,
		Status:      "stable",
		Action:      "View",
		LastChecked: "",
		Data:        realData,
	}
}

func getAllPatients(allowedClinics []string) []Patient {
	var patients []Patient
	clinics := getAllClinics(allowedClinics)
	for _, c := range clinics {
		entries, err := os.ReadDir(filepath.Join("data", c.Name))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				patients = append(patients, buildPatient(c.Name, e.Name()))
			}
		}
	}
	return patients
}

func handleClinics(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	allowedStr := r.URL.Query().Get("clinics")
	var allowed []string
	if allowedStr != "" {
		allowed = strings.Split(allowedStr, ",")
	}
	writeJSON(w, getAllClinics(allowed))
}

func handleAllPatients(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	allowedStr := r.URL.Query().Get("clinics")
	var allowed []string
	if allowedStr != "" {
		allowed = strings.Split(allowedStr, ",")
	}
	writeJSON(w, getAllPatients(allowed))
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}

	allowedStr := r.URL.Query().Get("clinics")
	var allowed []string
	if allowedStr != "" {
		allowed = strings.Split(allowedStr, ",")
	}

	clinics := getAllClinics(allowed)
	patients := getAllPatients(allowed)

	dashboard := map[string]interface{}{
		"quickStats": map[string]interface{}{
			"totalPatients": len(patients),
			"activeClinics": len(clinics),
			"activeDevices": len(patients), // Assume 1 device per patient
			"alertsCount":   0,
		},
	}

	writeJSON(w, dashboard)
}

func handlePatientRoutes(w http.ResponseWriter, r *http.Request) {
	if preflight(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/patient/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	allowedStr := r.URL.Query().Get("clinics")
	var allowed []string
	if allowedStr != "" {
		allowed = strings.Split(allowedStr, ",")
	}

	// path is patientName
	patients := getAllPatients(allowed)
	for _, p := range patients {
		if p.ID == path {
			writeJSON(w, p)
			return
		}
	}
	http.NotFound(w, r)
}

func handleClinicRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/clinic/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	clinicID := parts[0]

	if len(parts) == 1 {
		if preflight(w, r) {
			return
		}
		allowedStr := r.URL.Query().Get("clinics")
		var allowed []string
		if allowedStr != "" {
			allowed = strings.Split(allowedStr, ",")
		}

		clinics := getAllClinics(allowed)
		for _, c := range clinics {
			if c.ID == clinicID {
				writeJSON(w, c)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	switch parts[1] {
	case "patients":
		if preflight(w, r) {
			return
		}
		allowedStr := r.URL.Query().Get("clinics")
		var allowed []string
		if allowedStr != "" {
			allowed = strings.Split(allowedStr, ",")
		}

		result := []Patient{}
		patients := getAllPatients(allowed)
		for _, p := range patients {
			if p.ClinicID == clinicID {
				result = append(result, p)
			}
		}
		writeJSON(w, result)
	case "patient":
		if len(parts) >= 4 && parts[3] == "data" {
			patientID := parts[2]
			handlePatientData(w, r, clinicID, patientID)
		} else if len(parts) >= 4 && parts[3] == "camera" {
			patientID := parts[2]
			handlePatientCamera(w, r, clinicID, patientID)
		} else {
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func handlePatientData(w http.ResponseWriter, r *http.Request, clinicID, patientID string) {
	if preflight(w, r) {
		return
	}
	dir := filepath.Join("data", clinicID, patientID)
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

func handlePatientCamera(w http.ResponseWriter, r *http.Request, clinicID, patientID string) {
	if preflight(w, r) {
		return
	}
	path := filepath.Join("data", clinicID, patientID, "camera.jpg")
	http.ServeFile(w, r, path)
}
