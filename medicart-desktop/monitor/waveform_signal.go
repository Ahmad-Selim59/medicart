package monitor

import (
	"fmt"
	"math"
	"time"
)

const ecgMinPeakToPeak = 1e-6 // reject perfectly flat buffers

// WaveformSignalStats summarizes buffered samples for UI/debug.
type WaveformSignalStats struct {
	LeadKey    string
	SampleN    int
	Min        float64
	Max        float64
	PeakToPeak float64
	Flat       bool
}

func statsForSamples(leadKey string, samples []float64) WaveformSignalStats {
	st := WaveformSignalStats{LeadKey: leadKey, SampleN: len(samples)}
	if len(samples) == 0 {
		st.Flat = true
		return st
	}
	minV, maxV := samples[0], samples[0]
	for _, v := range samples {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	st.Min, st.Max = minV, maxV
	st.PeakToPeak = maxV - minV
	st.Flat = st.PeakToPeak < ecgMinPeakToPeak
	return st
}

// SignalStats returns stats for one lead snapshot.
func SignalStats(lead LeadSnapshot) WaveformSignalStats {
	return statsForSamples(lead.LeadKey, lead.Samples)
}

// FormatECGDebugLine is a one-line buffer summary for the desktop UI.
func FormatECGDebugLine(lead LeadSnapshot, now time.Time) string {
	st := SignalStats(lead)
	if st.SampleN == 0 {
		return "ECG W01: no Lead II buffer yet"
	}
	age := "stale"
	if !lead.UpdatedAt.IsZero() {
		age = fmt.Sprintf("%.0fs ago", now.Sub(lead.UpdatedAt).Seconds())
	}
	rate := lead.SampleRate
	if rate <= 0 {
		rate = DefaultSampleRate
	}
	line := fmt.Sprintf("ECG W01 (%s): %d samples @ %.0f Hz, peak %.4g, updated %s",
		shortLeadName(lead.LeadKey), st.SampleN, rate, st.PeakToPeak, age)
	if st.Flat {
		line += " — flat line (TR8 sending zeros in W01; HR on screen comes from R01, not this strip)"
	}
	return line
}

func shortLeadName(key string) string {
	switch key {
	case DefaultECGLeadKey, DefaultECGLeadCode:
		return "Lead II"
	case "MDC_ECG_ELEC_POTL_I", "131329":
		return "Lead I"
	default:
		return key
	}
}

// SnapshotBestECGLead picks the ECG lead with the largest peak-to-peak (prefers Lead II on tie).
func (c *WaveformCache) SnapshotBestECGLead() LeadSnapshot {
	preferred := c.SnapshotLead(DefaultECGLeadKey)
	if st := SignalStats(preferred); !st.Flat {
		return preferred
	}
	if c == nil {
		return LeadSnapshot{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var best LeadSnapshot
	bestP2P := -1.0
	for key, buf := range c.leads {
		if buf == nil || len(buf.Samples) == 0 {
			continue
		}
		if !isECGLeadKey(key) {
			continue
		}
		st := statsForSamples(buf.LeadKey, buf.Samples)
		if st.PeakToPeak > bestP2P || (math.Abs(st.PeakToPeak-bestP2P) < 1e-9 && key == DefaultECGLeadKey) {
			bestP2P = st.PeakToPeak
			out := make([]float64, len(buf.Samples))
			copy(out, buf.Samples)
			best = LeadSnapshot{
				LeadKey:    buf.LeadKey,
				Samples:    out,
				SampleRate: buf.SampleRate,
				Resolution: buf.Resolution,
				UpdatedAt:  buf.UpdatedAt,
			}
		}
	}
	if best.SampleN() > 0 {
		return best
	}
	return preferred
}

func isECGLeadKey(key string) bool {
	return isECGLeadIdentifier(key) || key == DefaultECGLeadKey
}

// SampleN returns len(Samples) (helper for LeadSnapshot).
func (l LeadSnapshot) SampleN() int {
	return len(l.Samples)
}
