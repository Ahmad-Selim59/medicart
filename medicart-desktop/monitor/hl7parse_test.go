package monitor

import (
	"strings"
	"testing"
)

// Golden ORU^R01 from hl7_demo_data.doc (sentinel vitals, empty PID).
const goldenORUR01 = `MSH|^~\&|XH80X^00A037009B000000^EUI-64||||20210611102047||ORU^R01^ORU_R01|41|P|2.4|||AL|NE||UNICODE UTF-8
PID|||^^^^PI||^^^^^^L|||||
PV1||I|^^^
OBR|1|41^XH80X^00A037009B000000^EUI-64|41^XH80X^00A037009B000000^EUI-64|182777000^monitoring of patient^SCT|||20210611102047
OBX|1|NM|150301^MDC_PRESS_CUFF_SYS^MDC|1.1.9.150301|-999|266016^MDC_DIM_MMHG^MDC||||||||20210611102047
OBX|5|NM|150456^MDC_PULS_OXIM_SAT_O2^MDC|1.3.1.150456|-999|262688^MDC_DIM_PERCENT^MDC||||||||20210611102047
OBX|9|NM|147842^MDC_ECG_HEART_RATE^MDC|1.7.4.147842|-999|264864^MDC_DIM_BEAT_PER_MIN^MDC||||||||20210611102047
OBX|7|NM|150344^MDC_TEMP^MDC|1.13.1.150344|-99.9|268192^MDC_DIM_DEGC^MDC||||||||20210611102047`

const goldenORUW01Header = `MSH|^~\&|XH80X^00A037009B000000^EUI-64||||||ORU^W01^ORU_W01|42|P|2.4|||AL|NE||UNICODE UTF-8
PID|||^^^^PI||^^^^^^L|||||
PV1||I|^^^
OBR|1||42^XH80X^00A037009B000000^EUI-64|CONTINUOUS WAVEFORM||||
OBX|1|NA|131329^MDC_ECG_ELEC_POTL_I^MDC|1.7.6.131329|0^0^0`

const syntheticPatientR01 = `MSH|^~\&|TR8|FAC|||||ORU^R01|99|P|2.4
PID|1||P12345||Doe^John||19800115|M|||^^^Bed12^WardA
PV1||I|^^Bed12^WardA
OBR|1|||182777000^monitoring of patient^SCT
OBX|1|NM|147842^MDC_ECG_HEART_RATE^MDC||72|264864^MDC_DIM_BEAT_PER_MIN^MDC||||||||20210611102047
OBX|2|NM|150456^MDC_PULS_OXIM_SAT_O2^MDC||98|262688^MDC_DIM_PERCENT^MDC||||||||20210611102047
OBX|3|NM|150301^MDC_PRESS_CUFF_SYS^MDC||120|266016^MDC_DIM_MMHG^MDC||||||||20210611102047
OBX|4|NM|150302^MDC_PRESS_CUFF_DIA^MDC||80|266016^MDC_DIM_MMHG^MDC||||||||20210611102047
OBX|5|NM|150303^MDC_PRESS_CUFF_MEAN^MDC||93|266016^MDC_DIM_MMHG^MDC||||||||20210611102047
OBX|6|NM|150344^MDC_TEMP^MDC|1.2.5.150344|36.6|268192^MDC_DIM_DEGC^MDC||||||||20210611102047`

func TestParseGoldenORUR01_NoValidVitals(t *testing.T) {
	msgs, err := ParseHL7Payload([]byte(goldenORUR01))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0]
	if msg.Skip {
		t.Fatal("R01 should not be skipped")
	}
	if msg.Vitals == nil {
		return
	}
	v := msg.Vitals
	if v.ECGHeartRate.Valid || v.SpO2.Valid || v.NIBPSys.Valid || v.Temp.Valid {
		t.Fatalf("sentinels should not produce valid vitals: %+v", v)
	}
}

func TestParseGoldenORUW01_Skipped(t *testing.T) {
	msgs, err := ParseHL7Payload([]byte(goldenORUW01Header))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !msgs[0].Skip {
		t.Fatal("W01 waveform should be skipped")
	}
}

func TestParseSentinelMinusOne_NoValidVitals(t *testing.T) {
	const r01 = `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1|NM|147842^MDC_ECG_HEART_RATE^MDC||-1|264864^MDC_DIM_BEAT_PER_MIN^MDC
OBX|2|NM|150456^MDC_PULS_OXIM_SAT_O2^MDC||-1|262688^MDC_DIM_PERCENT^MDC
OBX|3|NM|150301^MDC_PRESS_CUFF_SYS^MDC||-1|266016^MDC_DIM_MMHG^MDC
OBX|4|NM|150302^MDC_PRESS_CUFF_DIA^MDC||-1|266016^MDC_DIM_MMHG^MDC
OBX|5|NM|150303^MDC_PRESS_CUFF_MEAN^MDC||-1|266016^MDC_DIM_MMHG^MDC`
	msgs, err := ParseHL7Payload([]byte(r01))
	if err != nil {
		t.Fatal(err)
	}
	v := msgs[0].Vitals
	if v == nil {
		return
	}
	if v.ECGHeartRate.Valid || v.SpO2.Valid || v.NIBPSys.Valid {
		t.Fatalf("expected no valid vitals for -1 sentinels: %+v", v)
	}
	hr, spo2, nibp, _ := FormatVitalDisplay(*v)
	if hr != "—" || spo2 != "—" || nibp != "—" {
		t.Fatalf("display: hr=%q spo2=%q nibp=%q", hr, spo2, nibp)
	}
}

const tr8TempOBXFull = `OBX|7|NM|150344^MDC_TEMP^MDC|1.13.1.150344|-99.9|268192^MDC_DIM_DEGC^MDC||||||||20210611102047|||^|00A037009B000000^^00A037009B000000^EUI-64`

func TestParseTR8FullTempOBXLine(t *testing.T) {
	msg := `MSH|^~\&|XH80X^00A037009B000000^EUI-64||||20210611102047||ORU^R01^ORU_R01|41|P|2.4|||AL|NE||UNICODE UTF-8
` + tr8TempOBXFull
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range msgs[0].Info {
		if strings.Contains(line, "temp OBX") && strings.Contains(line, "-99.9") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected temp OBX log, Info=%v", msgs[0].Info)
	}
}

func TestParseTempWithLeadingUnicodeMSH(t *testing.T) {
	msg := "\u2028MSH|^~\\&|TR8|FAC|||||ORU^R01|1|P|2.4\r" + tr8TempOBXFull
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].MessageType == "" {
		t.Fatal("expected message type from MSH after unicode prefix")
	}
	if len(msgs[0].Info) == 0 {
		t.Fatal("expected temp OBX debug lines")
	}
}

func TestParseSpotTempFahrenheitGunChannel(t *testing.T) {
	// ZUG Table 40: spot temp OBX-4 = 1.2.5.150344, units °F.
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1|NM|150344^MDC_TEMP^MDC|1.2.5.150344|98.6|268224^MDC_DIM_FAHR^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	temp := msgs[0].Vitals.Temp
	if !temp.Valid {
		t.Fatalf("expected valid temp, Info=%v", msgs[0].Info)
	}
	if temp.Value < 36.9 || temp.Value > 37.2 {
		t.Fatalf("98.6°F should be ~37°C, got %.2f", temp.Value)
	}
	if temp.Rank != 100 {
		t.Fatalf("spot channel rank: %d", temp.Rank)
	}
}

func TestParseTempOBX_SNPicksValueNotMetadata(t *testing.T) {
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1|NM|150344^MDC_TEMP^MDC|1.2.5.150344|1966^36.4^|268192^MDC_DIM_DEGC^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if !msgs[0].Vitals.Temp.Valid || msgs[0].Vitals.Temp.Value != 36.4 {
		t.Fatalf("temp: %+v info=%v", msgs[0].Vitals.Temp, msgs[0].Info)
	}
	alt := false
	for _, line := range msgs[0].Info {
		if strings.Contains(line, "alternate OBX-5 component") {
			alt = true
		}
	}
	if !alt {
		t.Fatalf("expected alternate-component log, Info=%v", msgs[0].Info)
	}
}

func TestParseTempOBX_StrictFirstComponentOnly(t *testing.T) {
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1|NM|150344^MDC_TEMP^MDC|1.2.5.150344|36.4^99.9^|268192^MDC_DIM_DEGC^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if !msgs[0].Vitals.Temp.Valid || msgs[0].Vitals.Temp.Value != 36.4 {
		t.Fatalf("must use first OBX-5 component, got %+v", msgs[0].Vitals.Temp)
	}
	for _, line := range msgs[0].Info {
		if strings.Contains(line, "alternate OBX-5") {
			t.Fatalf("should not use fallback when primary is valid: %s", line)
		}
	}
}

func TestParseTempDiffOBXIgnored(t *testing.T) {
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1|NM|188440^MDC_TEMP_DIFF^MDC|1.2.4.188440|5.0|268224^MDC_DIM_FAHR^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Vitals != nil && msgs[0].Vitals.Temp.Valid {
		t.Fatal("delta temp should not set body temp")
	}
}

func TestParseSpotTempPreferredOverProbe(t *testing.T) {
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1|NM|150344^MDC_TEMP^MDC|1.2.1.150344|36.9|268192^MDC_DIM_DEGC^MDC
OBX|2|NM|150344^MDC_TEMP^MDC|1.2.5.150344|36.4|268192^MDC_DIM_DEGC^MDC||||R|||20261008020630||APERIODIC
OBX|3|NM|150344^MDC_TEMP^MDC|1.2.2.150344|37.1|268192^MDC_DIM_DEGC^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if got := msgs[0].Vitals.Temp.Value; got != 36.4 {
		t.Fatalf("spot temp should win, got %.1f info=%v", got, msgs[0].Info)
	}
}

func TestParseTempInvalidMarker(t *testing.T) {
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1||150344^MDC_TEMP^MDC|1.2.5.150344||268192^MDC_DIM_DEGC^MDC||INV|||X`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Vitals != nil && msgs[0].Vitals.Temp.Valid {
		t.Fatal("INV temp must not be valid")
	}
	if len(msgs[0].Info) == 0 || !strings.Contains(msgs[0].Info[0], "marked invalid") {
		t.Fatalf("expected invalid log, got %v", msgs[0].Info)
	}
}

func TestParseTempAlertMessageIgnored(t *testing.T) {
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R04|2|P|2.4
OBX|1|NM|150344^MDC_TEMP^MDC||36.4|268192^MDC_DIM_DEGC^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Vitals != nil && msgs[0].Vitals.Temp.Valid {
		t.Fatal("R04 alert messages must not update vitals")
	}
}

func TestParseRoomProbeInBodyRangeIgnored(t *testing.T) {
	// A warm room (or probe) reading 26 °C must never show as patient temp.
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBX|1|NM|150344^MDC_TEMP^MDC|1.2.1.150344|26.0|268192^MDC_DIM_DEGC^MDC
OBX|2|NM|150344^MDC_TEMP^MDC|1.2.2.150344|36.5|268192^MDC_DIM_DEGC^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if v := msgs[0].Vitals; v != nil && v.Temp.Valid {
		t.Fatalf("probe channels must not set patient temp: %+v", v.Temp)
	}
}

func TestParseTempOBX_LogsSentinelInfo(t *testing.T) {
	msgs, err := ParseHL7Payload([]byte(goldenORUR01))
	if err != nil {
		t.Fatal(err)
	}
	msg := msgs[0]
	found := false
	for _, line := range msg.Info {
		if strings.Contains(line, "temp OBX") && strings.Contains(line, "-99.9") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected temp debug line for sentinel, got Info=%v", msg.Info)
	}
}

func TestParseSyntheticPatientAndVitals(t *testing.T) {
	msgs, err := ParseHL7Payload([]byte(syntheticPatientR01))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0]
	if msg.Patient == nil {
		t.Fatal("expected patient")
	}
	if msg.Patient.PatientID != "P12345" {
		t.Errorf("patient id: got %q", msg.Patient.PatientID)
	}
	if !strings.Contains(msg.Patient.PatientName, "John") {
		t.Errorf("patient name: got %q", msg.Patient.PatientName)
	}
	if msg.Patient.ClinicName != "WardA" {
		t.Errorf("clinic: got %q", msg.Patient.ClinicName)
	}
	if msg.Patient.BedID != "Bed12" {
		t.Errorf("bed: got %q", msg.Patient.BedID)
	}
	if msg.Patient.Gender != "Male" {
		t.Errorf("gender: got %q", msg.Patient.Gender)
	}
	if msg.Patient.Age == 0 {
		t.Error("expected age from DOB")
	}
	v := msg.Vitals
	if !v.ECGHeartRate.Valid || v.ECGHeartRate.Value != 72 {
		t.Errorf("HR: %+v", v.ECGHeartRate)
	}
	if !v.SpO2.Valid || v.SpO2.Value != 98 {
		t.Errorf("SpO2: %+v", v.SpO2)
	}
	if !v.NIBPSys.Valid || v.NIBPSys.Value != 120 {
		t.Errorf("SYS: %+v", v.NIBPSys)
	}
	if !v.Temp.Valid || v.Temp.Value != 36.6 {
		t.Errorf("Temp: %+v", v.Temp)
	}
}
