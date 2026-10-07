package monitor

import (
	"strconv"
	"strings"
)

// IsWaveformMessage reports ORU^W01 (case-insensitive).
func IsWaveformMessage(msgType string) bool {
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(msgType)), "^")
	return len(parts) >= 2 && parts[0] == "ORU" && parts[1] == "W01"
}

func isECGLeadIdentifier(id string) bool {
	id = strings.ToUpper(strings.TrimSpace(id))
	if strings.HasPrefix(id, "MDC_ECG_ELEC_POTL") {
		return true
	}
	switch id {
	case "131329", "131330", "131331", "131389", "131390", "131391", "131392":
		return true
	default:
		return false
	}
}

func parseSampleList(raw string) []float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "^")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			continue
		}
		out = append(out, f)
	}
	return out
}

// BuildWaveformUpdate extracts ECG lead samples from parsed HL7 segments (W01).
func BuildWaveformUpdate(segments []hl7Segment, msgType string) *WaveformUpdate {
	if !IsWaveformMessage(msgType) {
		return nil
	}
	var chunks []LeadSampleChunk
	// sub-id prefix -> pending lead key for attr OBX
	type leadMeta struct {
		key        string
		sampleRate float64
		resolution float64
	}
	metaBySub := make(map[string]*leadMeta)

	for _, seg := range segments {
		if seg.name != "OBX" || len(seg.fields) < 6 {
			continue
		}
		id := obxMDCKey(seg.fields[3], seg.seps)
		subID := ""
		if len(seg.fields) > 4 {
			subID = strings.TrimSpace(seg.fields[4])
		}
		valType := ""
		if len(seg.fields) > 2 {
			valType = strings.ToUpper(strings.TrimSpace(seg.fields[2]))
		}

		switch {
		case id == "MDC_ATTR_SAMP_RATE" || id == "0":
			if strings.Contains(strings.ToUpper(seg.fields[3]), "SAMP_RATE") {
				if f, ok := validFloat(strings.TrimSpace(seg.fields[5])); ok {
					base := waveformSubIDBase(subID)
					m := metaBySub[base]
					if m == nil {
						m = &leadMeta{}
						metaBySub[base] = m
					}
					m.sampleRate = f
				}
			}
		case id == "2327" || strings.Contains(id, "NU_MSMT_RES"):
			if f, ok := validFloat(strings.TrimSpace(seg.fields[5])); ok {
				base := waveformSubIDBase(subID)
				m := metaBySub[base]
				if m == nil {
					m = &leadMeta{}
					metaBySub[base] = m
				}
				m.resolution = f
			}
		case isECGLeadIdentifier(id) && (valType == "NA" || valType == "NM"):
			samples := parseSampleList(obxWaveformValue(seg))
			if len(samples) == 0 {
				continue
			}
			base := waveformSubIDBase(subID)
			m := metaBySub[base]
			rate, res := DefaultSampleRate, 0.0
			if m != nil {
				if m.sampleRate > 0 {
					rate = m.sampleRate
				}
				res = m.resolution
			}
			if res > 0 {
				for i := range samples {
					samples[i] *= res
				}
			}
			chunks = append(chunks, LeadSampleChunk{
				LeadKey:    id,
				Samples:    samples,
				SampleRate: rate,
				Resolution: res,
			})
			if m == nil {
				m = &leadMeta{key: id}
				metaBySub[base] = m
			}
			m.key = id
		}
	}
	if len(chunks) == 0 {
		return nil
	}
	return &WaveformUpdate{Chunks: chunks}
}

func waveformSubIDBase(subID string) string {
	subID = strings.TrimSpace(subID)
	if i := strings.LastIndex(subID, "."); i > 0 {
		return subID[:i]
	}
	return subID
}

// obxWaveformValue returns OBX-5 sample list (TR8 puts units in OBX-6).
func obxWaveformValue(seg hl7Segment) string {
	if len(seg.fields) < 6 {
		return ""
	}
	raw := strings.TrimSpace(seg.fields[5])
	if raw != "" {
		return raw
	}
	next := strings.TrimSpace(seg.fields[6])
	if next != "" && !strings.Contains(strings.ToUpper(next), "MDC_DIM") {
		return next
	}
	return raw
}
