package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

func readingPayload(raw map[string]interface{}) map[string]interface{} {
	if nested, ok := raw["data"].(map[string]interface{}); ok {
		return nested
	}
	return raw
}

func payloadType(payload map[string]interface{}) string {
	if t, ok := payload["type"].(string); ok {
		return t
	}
	return ""
}

func payloadHasPositiveNumber(payload map[string]interface{}, key string) bool {
	v, ok := payload[key]
	if !ok {
		return false
	}
	switch n := v.(type) {
	case float64:
		return n > 0
	case int:
		return n > 0
	case int64:
		return n > 0
	default:
		return false
	}
}

func shouldPersistReading(raw map[string]interface{}) bool {
	if _, ok := profilePayloadFromRecord(raw); ok {
		return false
	}

	payload := readingPayload(raw)
	switch payloadType(payload) {
	case "cuff_update", "status", "error", "discovery", "profile":
		return false
	}

	if payloadHasPositiveNumber(payload, "cuff_pressure") &&
		(!payloadHasPositiveNumber(payload, "sys") || !payloadHasPositiveNumber(payload, "dia")) {
		return false
	}

	if payloadHasPositiveNumber(payload, "sys") && payloadHasPositiveNumber(payload, "dia") {
		return true
	}
	if payloadHasPositiveNumber(payload, "pr") || payloadHasPositiveNumber(payload, "spo2") {
		return true
	}
	if payloadHasPositiveNumber(payload, "glu") {
		return true
	}
	if payloadHasPositiveNumber(payload, "temp") {
		return true
	}
	if payloadType(payload) == "ecg" {
		if image, ok := payload["image"].(string); ok && image != "" {
			return true
		}
	}

	return false
}

func readingItemFromRecord(r Record) map[string]interface{} {
	payload := readingPayload(r.RawData)
	item := make(map[string]interface{}, len(payload)+1)
	for k, v := range payload {
		if k == "patient_name" || k == "clinic_name" {
			continue
		}
		item[k] = v
	}
	if _, hasTS := item["timestamp"]; !hasTS {
		item["timestamp"] = r.Timestamp.UTC().Format(time.RFC3339)
	}
	return item
}

func isDisplayableReading(item map[string]interface{}) bool {
	return shouldPersistReading(item)
}

func writeMetricRecordFile(path string, record Record) error {
	data, err := json.MarshalIndent([]Record{record}, "", "  ")
	if err != nil {
		return err
	}
	return saveFile(path, data)
}

func findLatestMetricRecord(dir, metric string) (string, Record, bool) {
	files, err := listFiles(dir)
	if err != nil {
		return "", Record{}, false
	}

	prefix := metric + "_"
	var latestFile string
	var latestTS int64

	for _, name := range files {
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		ts := metricFilenameTS(name)
		if ts >= latestTS {
			latestTS = ts
			latestFile = name
		}
	}

	if latestFile == "" {
		return "", Record{}, false
	}

	b, err := readFile(filepath.Join(dir, latestFile))
	if err != nil {
		return "", Record{}, false
	}

	var recs []Record
	if err := json.Unmarshal(b, &recs); err != nil || len(recs) == 0 {
		return "", Record{}, false
	}

	return latestFile, recs[len(recs)-1], true
}
