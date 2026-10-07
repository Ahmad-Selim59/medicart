package monitor

import (
	"sync"
	"time"
)

const (
	// DefaultECGLeadKey is the lead used for strip capture (Lead II).
	DefaultECGLeadKey = "MDC_ECG_ELEC_POTL_II"
	DefaultECGLeadCode = "131330"
	WaveformMaxSeconds   = 4.0
	DefaultSampleRate    = 500.0
	ECGStripSeconds      = 3.0
)

// LeadSampleChunk is one OBX waveform append from HL7 W01.
type LeadSampleChunk struct {
	LeadKey    string
	Samples    []float64
	SampleRate float64
	Resolution float64
}

// WaveformUpdate batches chunks from one W01 message.
type WaveformUpdate struct {
	Chunks []LeadSampleChunk
}

// LeadBuffer holds rolling samples for one ECG lead.
type LeadBuffer struct {
	LeadKey    string
	Samples    []float64
	SampleRate float64
	Resolution float64
	UpdatedAt  time.Time
}

// WaveformCache stores rolling ECG waveform data from W01.
type WaveformCache struct {
	mu    sync.RWMutex
	leads map[string]*LeadBuffer
}

func NewWaveformCache() *WaveformCache {
	return &WaveformCache{leads: make(map[string]*LeadBuffer)}
}

func leadMapKey(key string) string {
	if key == DefaultECGLeadCode {
		return DefaultECGLeadKey
	}
	return key
}

// Apply merges a waveform update into the cache.
func (c *WaveformCache) Apply(up *WaveformUpdate, at time.Time) {
	if c == nil || up == nil || len(up.Chunks) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, chunk := range up.Chunks {
		if len(chunk.Samples) == 0 {
			continue
		}
		k := leadMapKey(chunk.LeadKey)
		buf, ok := c.leads[k]
		if !ok {
			buf = &LeadBuffer{LeadKey: k}
			c.leads[k] = buf
		}
		if chunk.SampleRate > 0 {
			buf.SampleRate = chunk.SampleRate
		} else if buf.SampleRate <= 0 {
			buf.SampleRate = DefaultSampleRate
		}
		if chunk.Resolution > 0 {
			buf.Resolution = chunk.Resolution
		}
		buf.Samples = append(buf.Samples, chunk.Samples...)
		maxLen := int(WaveformMaxSeconds * buf.SampleRate)
		if maxLen < 100 {
			maxLen = int(WaveformMaxSeconds * DefaultSampleRate)
		}
		if len(buf.Samples) > maxLen {
			buf.Samples = buf.Samples[len(buf.Samples)-maxLen:]
		}
		buf.UpdatedAt = at
	}
}

// LeadSnapshot is a copy of one lead buffer.
type LeadSnapshot struct {
	LeadKey    string
	Samples    []float64
	SampleRate float64
	Resolution float64
	UpdatedAt  time.Time
}

// SnapshotLead returns a copy of the requested lead (aliases code 131330).
func (c *WaveformCache) SnapshotLead(leadKey string) LeadSnapshot {
	if c == nil {
		return LeadSnapshot{}
	}
	k := leadMapKey(leadKey)
	c.mu.RLock()
	defer c.mu.RUnlock()
	buf, ok := c.leads[k]
	if !ok {
		if k == DefaultECGLeadKey {
			buf = c.leads[DefaultECGLeadCode]
		}
	}
	if buf == nil {
		return LeadSnapshot{LeadKey: k}
	}
	out := make([]float64, len(buf.Samples))
	copy(out, buf.Samples)
	return LeadSnapshot{
		LeadKey:    buf.LeadKey,
		Samples:    out,
		SampleRate: buf.SampleRate,
		Resolution: buf.Resolution,
		UpdatedAt:  buf.UpdatedAt,
	}
}
