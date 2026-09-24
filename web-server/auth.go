package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const clinicCacheTTL = 60 * time.Second

type clinicCacheEntry struct {
	clinics []string
	expires time.Time
}

var (
	clinicCacheMu sync.RWMutex
	clinicCache   = make(map[string]clinicCacheEntry)
)

func tokenCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

func getCachedUserClinics(token string) ([]string, bool) {
	key := tokenCacheKey(token)

	clinicCacheMu.RLock()
	entry, ok := clinicCache[key]
	clinicCacheMu.RUnlock()

	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}

	return entry.clinics, true
}

func setCachedUserClinics(token string, clinics []string) {
	key := tokenCacheKey(token)
	clinicCopy := append([]string(nil), clinics...)

	clinicCacheMu.Lock()
	clinicCache[key] = clinicCacheEntry{
		clinics: clinicCopy,
		expires: time.Now().Add(clinicCacheTTL),
	}
	clinicCacheMu.Unlock()
}

func getUserClinicsFromSupabase(token string) ([]string, error) {
	url := os.Getenv("NEXT_PUBLIC_SUPABASE_URL")
	key := os.Getenv("NEXT_PUBLIC_SUPABASE_ANON_KEY")

	if url == "" || key == "" {
		return nil, fmt.Errorf("missing Supabase environment variables")
	}

	req, err := http.NewRequest("GET", url+"/rest/v1/clinic_members?select=clinics(name)", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("apikey", key)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("supabase returned status %d: %s - Body: %s", resp.StatusCode, resp.Status, string(body))
	}

	var memberships []struct {
		Clinics struct {
			Name string `json:"name"`
		} `json:"clinics"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&memberships); err != nil {
		return nil, err
	}

	var allowed []string
	for _, m := range memberships {
		if m.Clinics.Name != "" {
			allowed = append(allowed, m.Clinics.Name)
		}
	}

	return allowed, nil
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if preflight(w, r) {
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "Unauthorized: missing or invalid Bearer token", http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")

		allowedClinics, ok := getCachedUserClinics(token)
		if !ok {
			var err error
			allowedClinics, err = getUserClinicsFromSupabase(token)
			if err != nil {
				fmt.Printf("Auth error: %v\n", err)
				http.Error(w, "Unauthorized: invalid token or database error", http.StatusUnauthorized)
				return
			}
			setCachedUserClinics(token, allowedClinics)
		}

		q := r.URL.Query()
		clinicsStr := strings.Join(allowedClinics, ",")
		if clinicsStr == "" {
			clinicsStr = "__none__"
		}
		q.Set("clinics", clinicsStr)
		r.URL.RawQuery = q.Encode()

		next.ServeHTTP(w, r)
	}
}
