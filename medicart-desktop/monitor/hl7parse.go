package monitor

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var mshStart = regexp.MustCompile(`(?m)(?:^|\r|\n)MSH\|`)

// ParsedMessage is the extracted content from one HL7 message.
type ParsedMessage struct {
	MessageType string // e.g. ORU^R01
	Skip        bool
	Patient     *PatientSnapshot
	Vitals      *VitalsSnapshot
}

// ParseHL7Payload splits raw UDP bytes into messages and parses each.
func ParseHL7Payload(raw []byte) ([]*ParsedMessage, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	raw = bytes.TrimPrefix(raw, []byte{0xFE, 0xFF})
	raw = bytes.TrimPrefix(raw, []byte{0xFF, 0xFE})
	text := string(raw)
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	text = strings.TrimPrefix(text, "\x0b")
	text = strings.TrimSuffix(text, "\x1c\r")
	text = strings.TrimSuffix(text, "\x1c")
	text = strings.TrimSuffix(text, "\x0d")
	// Strip common log prefixes from demo captures.
	if idx := strings.Index(text, "MSH|"); idx > 0 {
		text = text[idx:]
	}

	indices := mshStart.FindAllStringIndex(text, -1)
	if len(indices) == 0 {
		return nil, nil
	}

	var out []*ParsedMessage
	for i, loc := range indices {
		start := loc[0]
		if start > 0 && (text[start] == '\r' || text[start] == '\n') {
			// Include MSH at loc[1]-len("MSH|") — find actual MSH|
			start = strings.Index(text[loc[0]:], "MSH|")
			if start >= 0 {
				start += loc[0]
			} else {
				start = loc[0]
			}
		}
		end := len(text)
		if i+1 < len(indices) {
			end = indices[i+1][0]
			if j := strings.LastIndex(text[start:end], "\nMSH|"); j >= 0 {
				end = start + j + 1
			} else if j := strings.LastIndex(text[start:end], "\rMSH|"); j >= 0 {
				end = start + j + 1
			}
		}
		chunk := strings.TrimSpace(text[start:end])
		if chunk == "" {
			continue
		}
		msg, err := parseOneMessage(chunk)
		if err != nil {
			return out, err
		}
		if msg != nil {
			out = append(out, msg)
		}
	}
	return out, nil
}

func parseOneMessage(body string) (*ParsedMessage, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	lines := strings.Split(body, "\n")
	var segments []hl7Segment
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Remove BOM / odd leading chars before segment type.
		if i := strings.Index(line, "MSH|"); i == 0 {
			segments = append(segments, parseSegment(line, defaultSeps()))
			continue
		}
		if i := strings.Index(line, "PID|"); i >= 0 {
			line = line[i:]
		} else if i := strings.Index(line, "PV1|"); i >= 0 {
			line = line[i:]
		} else if i := strings.Index(line, "OBR|"); i >= 0 {
			line = line[i:]
		} else if i := strings.Index(line, "OBX|"); i >= 0 {
			line = line[i:]
		} else if len(line) < 4 || line[3] != '|' {
			continue
		}
		seps := defaultSeps()
		if len(segments) > 0 && segments[0].name == "MSH" {
			seps = segments[0].seps
		}
		segments = append(segments, parseSegment(line, seps))
	}
	if len(segments) == 0 {
		return nil, nil
	}

	msg := &ParsedMessage{}
	patient := &PatientSnapshot{}
	vitals := &VitalsSnapshot{}
	hasPatient := false
	hasVitals := false

	var msgType string
	for _, seg := range segments {
		switch seg.name {
		case "MSH":
			// fields[0] is encoding characters; MSH-9 is at index 7.
			if len(seg.fields) > 7 {
				msgType = strings.TrimSpace(seg.fields[7])
				msg.MessageType = msgType
			}
			// MSH-4 sending facility (index 2) when PV1 has no facility — TR8 often leaves PV1 empty.
			if len(seg.fields) > 2 {
				facility := strings.TrimSpace(fieldComponent(seg.fields[2], seg.seps, 0))
				if facility == "" {
					facility = strings.TrimSpace(seg.fields[2])
				}
				if facility != "" && strings.TrimSpace(patient.ClinicName) == "" {
					patient.ClinicName = facility
					hasPatient = true
				}
			}
		case "PID":
			parsePID(seg, patient)
			hasPatient = true
		case "PV1":
			parsePV1(seg, patient)
			hasPatient = true
		case "OBR":
			if isWaveformOBR(seg) {
				msg.Skip = true
			}
		case "OBX":
			if msg.Skip {
				continue
			}
			if shouldProcessVitals(msgType) {
				if parseOBX(seg, vitals, patient) {
					hasVitals = true
				}
			}
		}
	}

	if strings.HasPrefix(msgType, "ORU") {
		parts := strings.Split(msgType, "^")
		if len(parts) >= 2 && parts[1] == "W01" {
			msg.Skip = true
		}
	}
	if msg.Skip {
		return &ParsedMessage{MessageType: msgType, Skip: true, Patient: patientIf(hasPatient, patient)}, nil
	}

	if !shouldProcessVitals(msgType) {
		return &ParsedMessage{
			MessageType: msgType,
			Patient:     patientIf(hasPatient, patient),
		}, nil
	}

	return &ParsedMessage{
		MessageType: msgType,
		Patient:     patientIf(hasPatient, patient),
		Vitals:      vitalsIf(hasVitals, vitals),
	}, nil
}

func patientIf(ok bool, p *PatientSnapshot) *PatientSnapshot {
	if !ok {
		return nil
	}
	return p
}

func vitalsIf(ok bool, v *VitalsSnapshot) *VitalsSnapshot {
	if !ok {
		return nil
	}
	return v
}

func shouldProcessVitals(msgType string) bool {
	parts := strings.Split(strings.ToUpper(msgType), "^")
	if len(parts) < 2 {
		return false
	}
	return parts[0] == "ORU" && parts[1] == "R01"
}

func isWaveformOBR(seg hl7Segment) bool {
	if len(seg.fields) < 4 {
		return false
	}
	// OBR-4 universal service ID
	text := fieldComponent(seg.fields[3], seg.seps, 1)
	if text == "" {
		text = seg.fields[3]
	}
	return strings.Contains(strings.ToUpper(text), "CONTINUOUS WAVEFORM")
}

type hl7Separators struct {
	field    byte
	component byte
	repetition byte
	escape   byte
	sub      byte
}

func defaultSeps() hl7Separators {
	return hl7Separators{field: '|', component: '^', repetition: '~', escape: '\\', sub: '&'}
}

type hl7Segment struct {
	name   string
	fields []string
	seps   hl7Separators
}

func parseSegment(line string, seps hl7Separators) hl7Segment {
	if strings.HasPrefix(line, "MSH|") {
		if len(line) >= 8 {
			seps = hl7Separators{
				field:      '|',
				component:  line[4],
				repetition: line[5],
				escape:     line[6],
				sub:        line[7],
			}
		}
		rest := line[4:]
		parts := strings.Split(rest, string(seps.field))
		fields := make([]string, 0, len(parts)+1)
		fields = append(fields, string(seps.field)+string(seps.component)+string(seps.repetition)+string(seps.escape)+string(seps.sub))
		fields = append(fields, parts[1:]...)
		return hl7Segment{name: "MSH", fields: fields, seps: seps}
	}
	parts := strings.Split(line, string(seps.field))
	name := ""
	if len(parts) > 0 {
		name = parts[0]
	}
	return hl7Segment{name: name, fields: parts, seps: seps}
}

func fieldComponent(field string, seps hl7Separators, idx int) string {
	parts := strings.Split(field, string(seps.component))
	if idx < 0 || idx >= len(parts) {
		return ""
	}
	return strings.TrimSpace(parts[idx])
}

func isPlaceholderHL7(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	stripped := strings.Map(func(r rune) rune {
		if r == '^' || r == '&' || r == '~' || r == ' ' {
			return -1
		}
		return r
	}, s)
	return stripped == ""
}

func parsePID(seg hl7Segment, p *PatientSnapshot) {
	// PID-3 patient ID list — first repetition, first component
	if len(seg.fields) > 3 {
		idField := seg.fields[3]
		idParts := strings.Split(idField, string(seg.seps.repetition))
		if len(idParts) > 0 {
			candidate := fieldComponent(idParts[0], seg.seps, 0)
			if !isPlaceholderHL7(candidate) {
				p.PatientID = candidate
			}
		}
		if p.PatientID == "" && len(seg.fields) > 3 {
			// PID-3.4 facility
			fac := fieldComponent(idField, seg.seps, 3)
			if !isPlaceholderHL7(fac) && p.ClinicName == "" {
				p.ClinicName = fac
			}
		}
	}
	// PID-5 name: family^given (ZUG may send XCN — use first two components)
	if len(seg.fields) > 5 {
		family := fieldComponent(seg.fields[5], seg.seps, 0)
		given := fieldComponent(seg.fields[5], seg.seps, 1)
		if !isPlaceholderHL7(family) || !isPlaceholderHL7(given) {
			p.PatientName = strings.TrimSpace(strings.TrimSpace(given) + " " + strings.TrimSpace(family))
		}
	}
	// PID-2 alternate patient ID
	if len(seg.fields) > 2 && strings.TrimSpace(p.PatientID) == "" {
		candidate := strings.TrimSpace(seg.fields[2])
		if !isPlaceholderHL7(candidate) {
			p.PatientID = candidate
		}
	}
	if len(seg.fields) > 7 {
		p.DOB = strings.TrimSpace(seg.fields[7])
		if age := ageFromDOB(p.DOB); age > 0 {
			p.Age = age
		}
	}
	if len(seg.fields) > 8 {
		p.Gender = mapGender(strings.TrimSpace(seg.fields[8]))
	}
	if len(seg.fields) > 41 {
		p.AgeGroup = strings.TrimSpace(seg.fields[41])
	}
}

func parsePV1(seg hl7Segment, p *PatientSnapshot) {
	if len(seg.fields) > 3 {
		loc := seg.fields[3]
		poc := fieldComponent(loc, seg.seps, 0)
		bed := fieldComponent(loc, seg.seps, 2)
		if bed != "" {
			p.BedID = bed
		}
		fac := fieldComponent(loc, seg.seps, 3)
		if fac != "" {
			p.ClinicName = fac
		} else if poc != "" && strings.TrimSpace(p.ClinicName) == "" {
			p.ClinicName = poc
		}
	}
}

func obxMDCKey(identifierField string, seps hl7Separators) string {
	name := strings.ToUpper(fieldComponent(identifierField, seps, 1))
	code := fieldComponent(identifierField, seps, 0)
	if strings.HasPrefix(name, "MDC_") {
		return name
	}
	return strings.ToUpper(code)
}

func parseOBX(seg hl7Segment, v *VitalsSnapshot, p *PatientSnapshot) bool {
	if len(seg.fields) < 6 {
		return false
	}
	id := obxMDCKey(seg.fields[3], seg.seps)
	rawVal := strings.TrimSpace(seg.fields[5])
	if rawVal == "" && len(seg.fields) > 6 {
		rawVal = strings.TrimSpace(seg.fields[6])
	}
	obsTime := parseHL7Time(seg, 14)

	switch id {
	case "MDC_ECG_HEART_RATE", "147842":
		if n, ok := validInt(rawVal); ok {
			v.ECGHeartRate = IntReading{Value: n, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_PULS_OXIM_PULS_RATE", "149530":
		if n, ok := validInt(rawVal); ok {
			v.SpO2Pulse = IntReading{Value: n, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_PULS_OXIM_SAT_O2", "150456":
		if n, ok := validInt(rawVal); ok {
			v.SpO2 = IntReading{Value: n, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_PRESS_CUFF_SYS", "150301":
		if n, ok := validInt(rawVal); ok {
			v.NIBPSys = IntReading{Value: n, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_PRESS_CUFF_DIA", "150302":
		if n, ok := validInt(rawVal); ok {
			v.NIBPDia = IntReading{Value: n, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_PRESS_CUFF_MEAN", "150303":
		if n, ok := validInt(rawVal); ok {
			v.NIBPMap = IntReading{Value: n, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_PULS_RATE_NON_INV", "149546":
		if n, ok := validInt(rawVal); ok {
			v.NIBPPulse = IntReading{Value: n, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_TEMP", "150344":
		if f, ok := validFloat(rawVal); ok {
			v.Temp = FloatReading{Value: f, Valid: true, ObservedAt: obsTime}
			return true
		}
	case "MDC_ATTR_PT_WEIGHT", "MDC_WEIGHT", "MDC_BODY_WEIGHT":
		if f, ok := validFloat(rawVal); ok {
			p.Weight = f
			return false
		}
	case "MDC_ATTR_PT_HEIGHT", "MDC_HEIGHT", "MDC_BODY_HEIGHT":
		if f, ok := validFloat(rawVal); ok {
			p.Height = f
			return false
		}
	}
	return false
}

func parseHL7Time(seg hl7Segment, fieldIdx int) time.Time {
	if len(seg.fields) <= fieldIdx {
		return time.Time{}
	}
	return parseHL7TimeString(seg.fields[fieldIdx])
}

func parseHL7TimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		"20060102150405",
		"200601021504",
		"20060102",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func ageFromDOB(dob string) int {
	t := parseHL7TimeString(dob)
	if t.IsZero() {
		return 0
}
	now := time.Now()
	age := now.Year() - t.Year()
	if now.YearDay() < t.YearDay() {
		age--
	}
	if age < 0 {
		return 0
	}
	return age
}

func mapGender(code string) string {
	switch strings.ToUpper(code) {
	case "M":
		return "Male"
	case "F":
		return "Female"
	default:
		return "Other"
	}
}

func validInt(raw string) (int, bool) {
	f, ok := validFloat(raw)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func validFloat(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if raw == "-999" || raw == "-99.9" {
		return 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
