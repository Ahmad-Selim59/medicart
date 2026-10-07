package monitor

import (
	"fmt"
	"time"
)

const (
	ecgMinSamples = 500 // ~1s at 500 Hz
)

// ECGCommitResult holds PNG bytes for ingest or an error.
type ECGCommitResult struct {
	PNG []byte
	Err error
}

// CanCommitECG checks freshness, sample count, and non-flat signal.
func CanCommitECG(lead LeadSnapshot, now time.Time) (bool, string) {
	if len(lead.Samples) < ecgMinSamples {
		return false, "No ECG waveform buffered yet — wait for monitor W01 stream"
	}
	if lead.UpdatedAt.IsZero() || now.Sub(lead.UpdatedAt) > MaxStaleness {
		return false, "No fresh ECG waveform from monitor"
	}
	if SignalStats(lead).Flat {
		return false, "ECG waveform is flat (all zeros) — enable ECG wave export on the TR8 HL7/W01 stream; monitor HR is from R01, not the strip"
	}
	return true, ""
}

// PreviewWindowSamples returns the most recent strip window for UI preview.
func PreviewWindowSamples(lead LeadSnapshot, now time.Time) []float64 {
	if len(lead.Samples) < 2 {
		return nil
	}
	rate := lead.SampleRate
	if rate <= 0 {
		rate = DefaultSampleRate
	}
	want := int(ECGStripSeconds * rate)
	if want < 2 {
		want = 2
	}
	samples := lead.Samples
	if len(samples) > want {
		samples = samples[len(samples)-want:]
	}
	return samples
}

// BuildECGCommitPNG renders the default lead strip for upload.
func BuildECGCommitPNG(lead LeadSnapshot, now time.Time) ECGCommitResult {
	if ok, msg := CanCommitECG(lead, now); !ok {
		return ECGCommitResult{Err: fmt.Errorf("%s", msg)}
	}
	rate := lead.SampleRate
	if rate <= 0 {
		rate = DefaultSampleRate
	}
	samples := PreviewWindowSamples(lead, now)
	png, err := RenderECGStripPNG(samples, 800, 240)
	if err != nil {
		return ECGCommitResult{Err: err}
	}
	return ECGCommitResult{PNG: png}
}
