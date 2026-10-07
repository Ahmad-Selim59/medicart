package monitor

import (
	"strings"
	"testing"
	"time"
)

const testW01ECG = `MSH|^~\&|XH80X^00A037009B000000^EUI-64||||||ORU^W01^ORU_W01|42|P|2.4|||AL|NE||UNICODE UTF-8
PID|||^^^^PI||^^^^^^L|||||
OBR|1||42^XH80X^00A037009B000000^EUI-64|CONTINUOUS WAVEFORM||||
OBX|1|NA|131330^MDC_ECG_ELEC_POTL_II^MDC|1.7.6.131330|1^2^3^4^5^6^7^8^9^10|262656^MDC_DIM_DIMLESS^MDC
OBX|2|NM|0^MDC_ATTR_SAMP_RATE^MDC|1.7.6.131330.1|500|264608^MDC_DIM_PER_SEC^MDC`

func TestBuildWaveformUpdate_LeadII(t *testing.T) {
	msgs, err := ParseHL7Payload([]byte(testW01ECG))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0]
	if !msg.Skip || msg.Waveform == nil {
		t.Fatalf("expected skip with waveform, got skip=%v wf=%v", msg.Skip, msg.Waveform)
	}
	if len(msg.Waveform.Chunks) == 0 {
		t.Fatal("expected chunks")
	}
	found := false
	for _, c := range msg.Waveform.Chunks {
		if strings.Contains(c.LeadKey, "131330") || strings.Contains(c.LeadKey, "POTL_II") {
			found = true
			if len(c.Samples) != 10 {
				t.Fatalf("samples: %d", len(c.Samples))
			}
		}
	}
	if !found {
		t.Fatal("lead II chunk not found")
	}
}

func TestECGCommitPipeline(t *testing.T) {
	state := NewMonitorState()
	msgs, _ := ParseHL7Payload([]byte(testW01ECG))
	now := time.Now()
	for i := 0; i < 60; i++ {
		state.ApplyWaveform(msgs[0].Waveform, now)
	}
	lead := state.ECGLeadSnapshot(DefaultECGLeadKey)
	res := BuildECGCommitPNG(lead, now)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.PNG) < 100 {
		t.Fatalf("png too small: %d", len(res.PNG))
	}
}

func TestRenderECGStripPNG(t *testing.T) {
	samples := make([]float64, 600)
	for i := range samples {
		samples[i] = float64(i % 50)
	}
	png, err := RenderECGStripPNG(samples, 400, 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(png) < 50 {
		t.Fatal("expected png bytes")
	}
}
