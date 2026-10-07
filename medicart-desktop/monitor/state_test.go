package monitor

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestPatientChangeDebounce(t *testing.T) {
	state := NewMonitorState()
	var count int32
	done := make(chan struct{}, 1)
	state.SetPatientChangeHandler(func(p PatientSnapshot) {
		atomic.AddInt32(&count, 1)
		done <- struct{}{}
	})

	now := time.Now()
	msg1 := &ParsedMessage{
		Patient: &PatientSnapshot{PatientID: "A", ClinicName: "C1", PatientName: "One"},
	}
	state.ApplyParsedMessage(msg1, now)

	msg2 := &ParsedMessage{
		Patient: &PatientSnapshot{PatientID: "B", ClinicName: "C1", PatientName: "Two"},
	}
	state.ApplyParsedMessage(msg2, now.Add(time.Millisecond))

	select {
	case <-done:
	case <-time.After(ProfileDebounce + 500*time.Millisecond):
		t.Fatal("timeout waiting for debounced handler")
	}

	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&count) != 1 {
		t.Fatalf("expected 1 debounced call, got %d", count)
	}
}
