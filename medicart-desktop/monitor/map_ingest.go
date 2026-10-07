package monitor

import (
	"fmt"
	"strings"
	"time"
)

// ResolvePatientIdentity merges HL7 demographics with app Settings fallbacks.
// The TR8 often omits a dedicated clinic/facility field; Settings → Clinic Name is used when HL7 has none.
func ResolvePatientIdentity(p PatientSnapshot, settingsClinic, settingsPatient string) PatientSnapshot {
	out := p
	if strings.TrimSpace(out.ClinicName) == "" {
		out.ClinicName = strings.TrimSpace(settingsClinic)
	}
	if strings.TrimSpace(out.PatientName) == "" {
		out.PatientName = strings.TrimSpace(settingsPatient)
	}
	if strings.TrimSpace(out.PatientName) == "" && strings.TrimSpace(out.PatientID) != "" {
		out.PatientName = strings.TrimSpace(out.PatientID)
	}
	if strings.TrimSpace(out.PatientName) == "" && strings.TrimSpace(out.BedID) != "" {
		out.PatientName = "Bed " + strings.TrimSpace(out.BedID)
	}
	return out
}

// CanCommitVitals reports whether identity is sufficient for vitals ingest (after Settings fallbacks).
func CanCommitVitals(p PatientSnapshot, settingsClinic, settingsPatient string) (bool, string) {
	p = ResolvePatientIdentity(p, settingsClinic, settingsPatient)
	clinic := strings.TrimSpace(p.ClinicName)
	if clinic == "" {
		return false, "Set Clinic Name in Settings (the monitor does not send a clinic). Patient name can come from HL7 or Settings."
	}
	id := strings.TrimSpace(p.PatientID)
	name := strings.TrimSpace(p.PatientName)
	if id == "" && name == "" {
		return false, "Patient identity missing: admit/sync a patient on the monitor, or enter Patient Name in Settings."
	}
	return true, ""
}

// BuildProfilePayload returns the profile ingest body (includes type at top level for sendData).
func BuildProfilePayload(p PatientSnapshot, settingsClinic, settingsPatient string) map[string]interface{} {
	p = ResolvePatientIdentity(p, settingsClinic, settingsPatient)
	body := map[string]interface{}{
		"type":         "profile",
		"patient_name": strings.TrimSpace(p.PatientName),
		"clinic_name":  strings.TrimSpace(p.ClinicName),
		"gender":       strings.TrimSpace(p.Gender),
		"age":          p.Age,
		"weight":       p.Weight,
		"height":       p.Height,
	}
	if id := strings.TrimSpace(p.PatientID); id != "" {
		body["patient_id"] = id
	}
	if bed := strings.TrimSpace(p.BedID); bed != "" {
		body["bed_id"] = bed
	}
	if ag := strings.TrimSpace(p.AgeGroup); ag != "" {
		body["age_group"] = ag
	}
	if dob := strings.TrimSpace(p.DOB); dob != "" {
		body["dob"] = dob
	}
	if body["gender"] == "" {
		body["gender"] = "Other"
	}
	return body
}

// CommitResult is the outcome of building a vital commit payload.
type CommitResult struct {
	Data map[string]interface{}
	Err  error
}

// BuildVitalCommit builds ingest data for a vital button press.
func BuildVitalCommit(kind VitalKind, v VitalsSnapshot, now time.Time) CommitResult {
	switch kind {
	case VitalHeartRate:
		return buildHeartRate(v, now)
	case VitalNIBP:
		return buildNIBP(v, now)
	case VitalTemp:
		return buildTemp(v, now)
	default:
		return CommitResult{Err: fmt.Errorf("unknown vital kind %q", kind)}
	}
}

func freshInt(r IntReading, now time.Time) bool {
	return r.Valid && !r.ObservedAt.IsZero() && now.Sub(r.ObservedAt) <= MaxStaleness
}

func freshFloat(r FloatReading, now time.Time) bool {
	return r.Valid && !r.ObservedAt.IsZero() && now.Sub(r.ObservedAt) <= MaxStaleness
}

func buildHeartRate(v VitalsSnapshot, now time.Time) CommitResult {
	var pr int
	var prOK bool
	if freshInt(v.ECGHeartRate, now) {
		pr, prOK = v.ECGHeartRate.Value, true
	} else if freshInt(v.SpO2Pulse, now) {
		pr, prOK = v.SpO2Pulse.Value, true
	}
	if !prOK || !freshInt(v.SpO2, now) {
		return CommitResult{Err: fmt.Errorf("no fresh reading from monitor")}
	}
	return CommitResult{Data: map[string]interface{}{
		"type": "data",
		"pr":   pr,
		"spo2": v.SpO2.Value,
	}}
}

func buildNIBP(v VitalsSnapshot, now time.Time) CommitResult {
	if !freshInt(v.NIBPSys, now) || !freshInt(v.NIBPDia, now) || !freshInt(v.NIBPMap, now) {
		return CommitResult{Err: fmt.Errorf("no fresh reading from monitor")}
	}
	pr := 0
	if freshInt(v.NIBPPulse, now) {
		pr = v.NIBPPulse.Value
	}
	return CommitResult{Data: map[string]interface{}{
		"type": "result",
		"sys":  v.NIBPSys.Value,
		"dia":  v.NIBPDia.Value,
		"map":  v.NIBPMap.Value,
		"pr":   pr,
		"irr":  false,
	}}
}

func buildTemp(v VitalsSnapshot, now time.Time) CommitResult {
	if !freshFloat(v.Temp, now) {
		return CommitResult{Err: fmt.Errorf("no fresh reading from monitor")}
	}
	return CommitResult{Data: map[string]interface{}{
		"type": "data",
		"temp": v.Temp.Value,
	}}
}

// FormatVitalDisplay returns human-readable live values for the UI.
func FormatVitalDisplay(v VitalsSnapshot) (hr, spo2, nibp, temp string) {
	if v.ECGHeartRate.Valid {
		hr = fmt.Sprintf("%d bpm", v.ECGHeartRate.Value)
	} else if v.SpO2Pulse.Valid {
		hr = fmt.Sprintf("%d bpm (SpO2)", v.SpO2Pulse.Value)
	} else {
		hr = "—"
	}
	if v.SpO2.Valid {
		spo2 = fmt.Sprintf("%d%%", v.SpO2.Value)
	} else {
		spo2 = "—"
	}
	if v.NIBPSys.Valid && v.NIBPDia.Valid && v.NIBPMap.Valid {
		nibp = fmt.Sprintf("%d/%d (%d)", v.NIBPSys.Value, v.NIBPDia.Value, v.NIBPMap.Value)
	} else {
		nibp = "—"
	}
	if v.Temp.Valid {
		temp = fmt.Sprintf("%.1f °C", v.Temp.Value)
	} else {
		temp = "—"
	}
	return hr, spo2, nibp, temp
}

// ConnectionStatusText formats monitor link status for the UI.
func ConnectionStatusText(c ConnectionMeta, now time.Time, vitals VitalsSnapshot) string {
	addr := strings.TrimSpace(c.ListenAddr)
	if addr == "" {
		addr = "0.0.0.0:5000"
	}
	if c.LastPacketAt.IsZero() {
		return fmt.Sprintf("Monitor: listening on %s — no UDP packets yet (check TR8 target IP, port %s, and firewall)", addr, addr)
	}
	ago := now.Sub(c.LastPacketAt)
	hasVitals := vitals.ECGHeartRate.Valid || vitals.SpO2.Valid || vitals.SpO2Pulse.Valid ||
		vitals.NIBPSys.Valid || vitals.Temp.Valid
	vitalsHint := ""
	if !hasVitals {
		vitalsHint = " — receiving HL7 but no valid vitals yet (probes on?)"
	}
	if ago < 3*time.Second {
		ip := c.SourceIP
		if ip != "" {
			return fmt.Sprintf("Monitor: connected (%s, %d pkts)%s", ip, c.PacketsReceived, vitalsHint)
		}
		return fmt.Sprintf("Monitor: connected (%d pkts)%s", c.PacketsReceived, vitalsHint)
	}
	return fmt.Sprintf("Monitor: no data for %ds (last: %s, %d pkts)%s", int(ago.Seconds()), c.LastPacketAt.Format("15:04:05"), c.PacketsReceived, vitalsHint)
}
