package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	Weight      float64                `json:"weight"`
	Height      float64                `json:"height"`
	Status      string                 `json:"status"`
	Action      string                 `json:"action"`
	Data        map[string]interface{} `json:"data"`
}

type PatientProfile struct {
	PatientName string  `json:"patient_name"`
	ClinicName  string  `json:"clinic_name"`
	Gender      string  `json:"gender"`
	Age         int     `json:"age"`
	Weight      float64 `json:"weight"`
	Height      float64 `json:"height"`
	UpdatedAt   string  `json:"updated_at"`
}

func loadPatientProfile(clinicName, patientName string) PatientProfile {
	path := filepath.Join("data", clinicName, patientName, "profile.json")
	b, err := readFile(path)
	if err != nil {
		return PatientProfile{}
	}
	var profile PatientProfile
	if err := json.Unmarshal(b, &profile); err != nil {
		return PatientProfile{}
	}
	return profile
}

func profilePayloadFromRecord(raw map[string]interface{}) (map[string]interface{}, bool) {
	if t, ok := raw["type"].(string); ok && t == "profile" {
		return raw, true
	}
	if nested, ok := raw["data"].(map[string]interface{}); ok {
		if t, ok := nested["type"].(string); ok && t == "profile" {
			return nested, true
		}
	}
	return nil, false
}

func profileFromPayload(payload map[string]interface{}, patientName, clinicName, updatedAt string) PatientProfile {
	return PatientProfile{
		PatientName: patientName,
		ClinicName:  clinicName,
		Gender:      profileString(payload, "gender"),
		Age:         profileInt(payload, "age"),
		Weight:      profileFloat(payload, "weight"),
		Height:      profileFloat(payload, "height"),
		UpdatedAt:   updatedAt,
	}
}

func mergePatientProfile(primary, fallback PatientProfile) PatientProfile {
	merged := fallback
	if primary.Gender != "" {
		merged.Gender = primary.Gender
	}
	if primary.Age > 0 {
		merged.Age = primary.Age
	}
	if primary.Weight > 0 {
		merged.Weight = primary.Weight
	}
	if primary.Height > 0 {
		merged.Height = primary.Height
	}
	if primary.UpdatedAt != "" {
		merged.UpdatedAt = primary.UpdatedAt
	}
	return merged
}

func readingTimestamp(item interface{}) int64 {
	m, ok := item.(map[string]interface{})
	if !ok {
		return 0
	}
	switch ts := m["timestamp"].(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			return parsed.Unix()
		}
	case float64:
		return int64(ts)
	}
	return 0
}

func trimToLastReadings(items []interface{}, keep int) []interface{} {
	if keep <= 0 || len(items) <= keep {
		return items
	}
	sort.Slice(items, func(i, j int) bool {
		return readingTimestamp(items[i]) < readingTimestamp(items[j])
	})
	return items[len(items)-keep:]
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
	entries, _ := listDirs("data")
	for _, name := range entries {
		// count patients in folder
		patientEntries, _ := listDirs(filepath.Join("data", name))
		patientCount := len(patientEntries)

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

	return clinics
}



func buildPatient(clinicName string, patientName string) Patient {
	dir := filepath.Join("data", clinicName, patientName)
	files, err := listFiles(dir)

	realData := make(map[string]interface{})
	var latestMiscProfile PatientProfile
	var latestMiscProfileTS int64

	if err == nil {
		for _, name := range files {
			if name == "profile.json" {
				continue
			}
			if strings.HasSuffix(name, ".json") {
				b, err := readFile(filepath.Join(dir, name))
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
						if payload, ok := profilePayloadFromRecord(r.RawData); ok {
							if ts := r.Timestamp.Unix(); ts >= latestMiscProfileTS {
								latestMiscProfileTS = ts
								latestMiscProfile = profileFromPayload(
									payload,
									patientName,
									clinicName,
									r.Timestamp.UTC().Format(time.RFC3339),
								)
							}
							continue
						}

						item := make(map[string]interface{}, len(r.RawData)+1)
						for k, v := range r.RawData {
							item[k] = v
						}
						if _, hasTS := item["timestamp"]; !hasTS {
							item["timestamp"] = r.Timestamp.UTC().Format(time.RFC3339)
						}
						innerData = append(innerData, item)
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

	for key, val := range realData {
		if items, ok := val.([]interface{}); ok {
			realData[key] = trimToLastReadings(items, maxReadingsPerMetric)
		}
	}

	profile := mergePatientProfile(loadPatientProfile(clinicName, patientName), latestMiscProfile)
	gender := "Unknown"
	age := 0
	weight := 0.0
	height := 0.0
	if profile.Gender != "" {
		gender = profile.Gender
	}
	if profile.Age > 0 {
		age = profile.Age
	}
	if profile.Weight > 0 {
		weight = profile.Weight
	}
	if profile.Height > 0 {
		height = profile.Height
	}

	return Patient{
		ID:          patientName, // use name as ID
		ClinicID:    clinicName,
		Name:        patientName,
		Gender:      gender,
		Age:         age,
		Weight:      weight,
		Height:      height,
		Status:   "stable",
		Action:   "View",
		Data:     realData,
	}
}

func getAllPatients(allowedClinics []string) []Patient {
	var patients []Patient
	clinics := getAllClinics(allowedClinics)
	for _, c := range clinics {
		entries, err := listDirs(filepath.Join("data", c.Name))
		if err != nil {
			continue
		}
		for _, name := range entries {
			patients = append(patients, buildPatient(c.Name, name))
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
	files, err := listFiles(dir)
	if err != nil {
		http.Error(w, "Failed to read patient data", http.StatusInternalServerError)
		return
	}
	result := map[string]interface{}{}
	for _, name := range files {
		if strings.HasSuffix(name, ".json") {
			b, err := readFile(filepath.Join(dir, name))
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
	if CloudStoreEnabled {
		b, err := readFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(b)
		return
	}
	http.ServeFile(w, r, path)
}
