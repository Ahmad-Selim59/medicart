package monitor

import (
	"testing"
	"time"
)

func TestBuildVitalCommit(t *testing.T) {
	now := time.Now()
	at := now.Add(-5 * time.Second)
	v := VitalsSnapshot{
		ECGHeartRate: IntReading{Value: 75, Valid: true, ObservedAt: at},
		SpO2:         IntReading{Value: 98, Valid: true, ObservedAt: at},
		NIBPSys:      IntReading{Value: 120, Valid: true, ObservedAt: at},
		NIBPDia:      IntReading{Value: 80, Valid: true, ObservedAt: at},
		NIBPMap:      IntReading{Value: 93, Valid: true, ObservedAt: at},
		NIBPPulse:    IntReading{Value: 72, Valid: true, ObservedAt: at},
		Temp:         FloatReading{Value: 36.6, Valid: true, ObservedAt: at},
	}

	hr := BuildVitalCommit(VitalHeartRate, v, now)
	if hr.Err != nil {
		t.Fatal(hr.Err)
	}
	if hr.Data["type"] != "data" || hr.Data["pr"] != 75 || hr.Data["spo2"] != 98 {
		t.Fatalf("heart rate payload: %v", hr.Data)
	}

	nibp := BuildVitalCommit(VitalNIBP, v, now)
	if nibp.Err != nil {
		t.Fatal(nibp.Err)
	}
	if nibp.Data["type"] != "result" || nibp.Data["sys"] != 120 || nibp.Data["irr"] != false {
		t.Fatalf("nibp payload: %v", nibp.Data)
	}

	temp := BuildVitalCommit(VitalTemp, v, now)
	if temp.Err != nil {
		t.Fatal(temp.Err)
	}
	if temp.Data["temp"] != 36.6 {
		t.Fatalf("temp payload: %v", temp.Data)
	}
}

func TestBuildVitalCommit_Stale(t *testing.T) {
	now := time.Now()
	at := now.Add(-90 * time.Second)
	v := VitalsSnapshot{
		ECGHeartRate: IntReading{Value: 75, Valid: true, ObservedAt: at},
		SpO2:         IntReading{Value: 98, Valid: true, ObservedAt: at},
	}
	r := BuildVitalCommit(VitalHeartRate, v, now)
	if r.Err == nil {
		t.Fatal("expected stale error")
	}
}

func TestCanCommitVitals(t *testing.T) {
	ok, _ := CanCommitVitals(PatientSnapshot{ClinicName: "Ward", PatientID: "1"})
	if !ok {
		t.Fatal("expected ok with id and clinic")
	}
	ok, msg := CanCommitVitals(PatientSnapshot{PatientName: "John"})
	if ok {
		t.Fatal("expected fail without clinic")
	}
	if msg == "" {
		t.Fatal("expected message")
	}
}
