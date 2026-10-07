package monitor

import (
	"strings"
)

// Canonical ingest/display units: temperature °C, weight kg, height cm.

func obxUnitsKey(seg hl7Segment) string {
	if len(seg.fields) <= 6 {
		return ""
	}
	return normalizeUnitKey(obxMDCKey(seg.fields[6], seg.seps))
}

func normalizeUnitKey(key string) string {
	key = strings.ToUpper(strings.TrimSpace(key))
	key = strings.TrimPrefix(key, "MDC_DIM_")
	return key
}

// ConvertTempToCelsius uses OBX-6 unit when present; missing unit assumes Celsius (TR8 default).
func ConvertTempToCelsius(value float64, unitKey string) (float64, bool) {
	switch classifyTempUnit(unitKey) {
	case tempUnitCelsius, tempUnitUnknown:
		return value, true
	case tempUnitFahrenheit:
		return (value - 32) * 5 / 9, true
	default:
		return 0, false
	}
}

type tempUnitKind int

const (
	tempUnitUnknown tempUnitKind = iota
	tempUnitCelsius
	tempUnitFahrenheit
	tempUnitUnsupported
)

func classifyTempUnit(unitKey string) tempUnitKind {
	u := normalizeUnitKey(unitKey)
	if u == "" {
		return tempUnitUnknown
	}
	switch u {
	case "DEGC", "CELSIUS", "DEG_C", "268192", "C":
		return tempUnitCelsius
	case "DEGF", "FAHR", "DEG_F", "F", "268224", "268208":
		return tempUnitFahrenheit
	default:
		if strings.Contains(u, "DEGC") || strings.Contains(u, "CELSIUS") {
			return tempUnitCelsius
		}
		if strings.Contains(u, "DEGF") || strings.Contains(u, "FAHR") {
			return tempUnitFahrenheit
		}
		return tempUnitUnsupported
	}
}

// ConvertWeightToKg uses OBX-6; missing unit assumes kg.
func ConvertWeightToKg(value float64, unitKey string) (float64, bool) {
	switch classifyWeightUnit(unitKey) {
	case weightUnitKg, weightUnitUnknown:
		return value, true
	case weightUnitLb:
		return value * 0.45359237, true
	default:
		return 0, false
	}
}

type weightUnitKind int

const (
	weightUnitUnknown weightUnitKind = iota
	weightUnitKg
	weightUnitLb
	weightUnitUnsupported
)

func classifyWeightUnit(unitKey string) weightUnitKind {
	u := normalizeUnitKey(unitKey)
	if u == "" {
		return weightUnitUnknown
	}
	switch u {
	case "KG", "KGM", "263441", "263875":
		return weightUnitKg
	case "LB", "LBM", "POUND", "263747", "264320":
		return weightUnitLb
	default:
		if strings.Contains(u, "KG") {
			return weightUnitKg
		}
		if strings.Contains(u, "LB") || strings.Contains(u, "POUND") {
			return weightUnitLb
		}
		return weightUnitUnsupported
	}
}

// ConvertHeightToCm uses OBX-6; missing unit assumes cm.
func ConvertHeightToCm(value float64, unitKey string) (float64, bool) {
	switch classifyHeightUnit(unitKey) {
	case heightUnitCm, heightUnitUnknown:
		return value, true
	case heightUnitM:
		return value * 100, true
	case heightUnitIn:
		return value * 2.54, true
	case heightUnitFt:
		return value * 30.48, true
	default:
		return 0, false
	}
}

type heightUnitKind int

const (
	heightUnitUnknown heightUnitKind = iota
	heightUnitCm
	heightUnitM
	heightUnitIn
	heightUnitFt
	heightUnitUnsupported
)

func classifyHeightUnit(unitKey string) heightUnitKind {
	u := normalizeUnitKey(unitKey)
	if u == "" {
		return heightUnitUnknown
	}
	switch u {
	case "CM", "CENTIM", "263875", "262656":
		return heightUnitCm
	case "M", "METER", "METRE", "263440":
		return heightUnitM
	case "IN", "INCH", "264816", "264320":
		return heightUnitIn
	case "FT", "FOOT", "FEET":
		return heightUnitFt
	default:
		if strings.Contains(u, "CENTIM") || u == "CM" {
			return heightUnitCm
		}
		if strings.Contains(u, "INCH") || strings.HasSuffix(u, "_IN") {
			return heightUnitIn
		}
		if strings.Contains(u, "METER") || strings.Contains(u, "METRE") {
			return heightUnitM
		}
		return heightUnitUnsupported
	}
}
