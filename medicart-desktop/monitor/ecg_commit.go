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

// CanCommitECG checks freshness and sample count for the default lead.
func CanCommitECG(lead LeadSnapshot, now time.Time) (bool, string) {
	if len(lead.Samples) < ecgMinSamples {
		return false, "No ECG waveform buffered yet — wait for monitor W01 stream"
	}
	if lead.UpdatedAt.IsZero() || now.Sub(lead.UpdatedAt) > MaxStaleness {
		return false, "No fresh ECG waveform from monitor"
	}
	return true, ""
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
	want := int(ECGStripSeconds * rate)
	samples := lead.Samples
	if len(samples) > want {
		samples = samples[len(samples)-want:]
	}
	png, err := RenderECGStripPNG(samples, 800, 240)
	if err != nil {
		return ECGCommitResult{Err: err}
	}
	return ECGCommitResult{PNG: png}
}
