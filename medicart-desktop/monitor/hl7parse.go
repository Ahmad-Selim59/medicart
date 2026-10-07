package monitor

import (
	"bytes"
	"fmt"
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
	Waveform    *WaveformUpdate // ORU^W01 ECG chunks (when Skip)
	Info        []string        // optional debug lines for Live Console (temp tracing, etc.)
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

func normalizeHL7Line(line string) string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "\uFEFF\u2028\u2029\u200B\x00")
	return line
}

func lineFromSegment(seg hl7Segment) string {
	if len(seg.fields) == 0 {
		return seg.name
	}
	return strings.Join(seg.fields, string(seg.seps.field))
}

func splitHL7Segments(body string) []hl7Segment {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	var segments []hl7Segment
	seps := defaultSeps()
	for _, line := range strings.Split(body, "\n") {
		line = normalizeHL7Line(line)
		if line == "" {
			continue
		}
		if idx := strings.Index(line, "MSH|"); idx >= 0 {
			line = line[idx:]
			seg := parseSegment(line, defaultSeps())
			segments = append(segments, seg)
			seps = seg.seps
			continue
		}
		for _, prefix := range []string{"PID|", "PV1|", "OBR|", "OBX|", "EVN|", "NTE|"} {
			if idx := strings.Index(line, prefix); idx >= 0 {
				line = line[idx:]
				break
			}
		}
		if len(line) < 4 || line[3] != '|' {
			continue
		}
		segments = append(segments, parseSegment(line, seps))
	}
	return segments
}

func parseOneMessage(body string) (*ParsedMessage, error) {
	segments := splitHL7Segments(body)
	if len(segments) == 0 {
		return nil, nil
	}

	msg := &ParsedMessage{}
	patient := &PatientSnapshot{}
	vitals := &VitalsSnapshot{}
	hasPatient := false
	hasVitals := false

	var msgType string
	var parseInfo []string
	var tempOBXSegments []hl7Segment
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
			if segmentMentionsTemp(seg) {
				tempOBXSegments = append(tempOBXSegments, seg)
				if !msg.Skip && shouldProcessVitals(msgType) {
					if parseOBXTempFlexible(seg, vitals, &parseInfo) {
						hasVitals = true
					}
				}
				continue
			}
			if msg.Skip {
				continue
			}
			if shouldProcessVitals(msgType) {
				if parseOBX(seg, vitals, patient, &parseInfo) {
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
		out := &ParsedMessage{MessageType: msgType, Skip: true, Patient: patientIf(hasPatient, patient)}
		if IsWaveformMessage(msgType) {
			out.Waveform = BuildWaveformUpdate(segments, msgType)
		}
		return out, nil
	}

	if !shouldProcessVitals(msgType) {
		return &ParsedMessage{
			MessageType: msgType,
			Patient:     patientIf(hasPatient, patient),
		}, nil
	}

	out := &ParsedMessage{
		MessageType: msgType,
		Patient:     patientIf(hasPatient, patient),
		Vitals:      vitalsIf(hasVitals, vitals),
	}
	if shouldProcessVitals(msgType) && len(tempOBXSegments) > 0 {
		hasTempLog := false
		for _, line := range parseInfo {
			if strings.Contains(line, "temp OBX") {
				hasTempLog = true
				break
			}
		}
		if !hasTempLog {
			raw := lineFromSegment(tempOBXSegments[0])
			if len(raw) > 160 {
				raw = raw[:160] + "…"
			}
			parseInfo = append(parseInfo, fmt.Sprintf("Monitor temp OBX: segment seen but not parsed: %s", raw))
		}
	}
	if len(parseInfo) > 0 {
		out.Info = parseInfo
	}
	return out, nil
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
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(msgType)), "^")
	if len(parts) < 2 {
		return false
	}
	if parts[0] != "ORU" {
		return false
	}
	// W01 is waveform-only; other ORU types (R01, R04, …) may carry vitals / gun temp.
	return parts[1] != "W01"
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

func segmentMentionsTemp(seg hl7Segment) bool {
	if seg.name != "OBX" {
		return false
	}
	for i := 1; i < len(seg.fields); i++ {
		u := strings.ToUpper(seg.fields[i])
		if strings.Contains(u, "150344") || strings.Contains(u, "MDC_TEMP") {
			return true
		}
	}
	return false
}

func fieldLooksLikeTempID(field string, seps hl7Separators) bool {
	if field == "" {
		return false
	}
	key := obxMDCKey(field, seps)
	if isTempOBXIdentifier(key, field) {
		return true
	}
	u := strings.ToUpper(field)
	return strings.Contains(u, "150344") || strings.Contains(u, "MDC_TEMP")
}

func obxTempValueAndUnits(seg hl7Segment) (rawVal, units string) {
	identIdx := -1
	for i := 2; i < len(seg.fields); i++ {
		if fieldLooksLikeTempID(seg.fields[i], seg.seps) || tempSubIDField(seg.fields[i]) {
			identIdx = i
			break
		}
	}
	if identIdx < 0 {
		return "", ""
	}
	for j := identIdx + 1; j < len(seg.fields) && j <= identIdx+5; j++ {
		f := strings.TrimSpace(fieldComponent(seg.fields[j], seg.seps, 0))
		if f == "" {
			f = strings.TrimSpace(seg.fields[j])
		}
		if f == "" {
			continue
		}
		if strings.Contains(strings.ToUpper(f), "MDC_DIM") || strings.Contains(f, "^MDC") {
			if units == "" {
				units = normalizeUnitKey(obxMDCKey(f, seg.seps))
			}
			continue
		}
		if isSentinelRaw(f) {
			return f, units
		}
		if _, err := strconv.ParseFloat(f, 64); err == nil {
			if units == "" && j+1 < len(seg.fields) {
				units = obxUnitsKeyAt(seg, j+1)
			}
			return f, units
		}
	}
	return "", units
}

func tempSubIDField(field string) bool {
	u := strings.ToUpper(field)
	return strings.Contains(field, "150344") ||
		strings.Contains(u, "1.13.1") ||
		strings.Contains(u, "1.13.2")
}

func obxUnitsKeyAt(seg hl7Segment, idx int) string {
	if idx < 0 || idx >= len(seg.fields) {
		return ""
	}
	return normalizeUnitKey(obxMDCKey(seg.fields[idx], seg.seps))
}

func parseOBXTempFlexible(seg hl7Segment, v *VitalsSnapshot, info *[]string) bool {
	rawVal, units := obxTempValueAndUnits(seg)
	if rawVal == "" && units == "" && !segmentMentionsTemp(seg) {
		return false
	}
	obsTime := parseHL7Time(seg, 14)
	if units == "" {
		units = obxUnitsKey(seg)
	}
	return parseTempOBX(v, rawVal, units, obsTime, info, seg)
}

func parseOBX(seg hl7Segment, v *VitalsSnapshot, p *PatientSnapshot, info *[]string) bool {
	if segmentMentionsTemp(seg) {
		if len(seg.fields) < 4 {
			return parseOBXTempFlexible(seg, v, info)
		}
	}
	if len(seg.fields) < 6 {
		return false
	}
	id := obxMDCKey(seg.fields[3], seg.seps)
	rawVal := obxNumericValue(seg)
	obsTime := parseHL7Time(seg, 14)
	units := obxUnitsKey(seg)

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
	case "MDC_TEMP", "150344", "188440", "188472":
		return parseTempOBX(v, rawVal, units, obsTime, info, seg)
	case "MDC_ATTR_PT_WEIGHT", "MDC_WEIGHT", "MDC_BODY_WEIGHT":
		if f, ok := validFloat(rawVal); ok {
			kg, ok := ConvertWeightToKg(f, units)
			if ok && kg > 0 {
				p.Weight = kg
			}
			return false
		}
	case "MDC_ATTR_PT_HEIGHT", "MDC_HEIGHT", "MDC_BODY_HEIGHT":
		if f, ok := validFloat(rawVal); ok {
			cm, ok := ConvertHeightToCm(f, units)
			if ok && cm > 0 {
				p.Height = cm
			}
			return false
		}
	}
	if isTempOBXIdentifier(id, seg.fields[3]) || tempOBXSubID(seg) {
		return parseTempOBX(v, rawVal, units, obsTime, info, seg)
	}
	return false
}

func tempOBXSubID(seg hl7Segment) bool {
	if len(seg.fields) < 5 {
		return false
	}
	return strings.Contains(seg.fields[4], "150344") ||
		strings.Contains(strings.ToUpper(seg.fields[4]), "1.13.1") ||
		strings.Contains(strings.ToUpper(seg.fields[4]), "1.13.2")
}

func obxNumericValue(seg hl7Segment) string {
	if len(seg.fields) < 6 {
		return ""
	}
	valType := ""
	if len(seg.fields) > 2 {
		valType = strings.TrimSpace(seg.fields[2])
	}
	raw := strings.TrimSpace(fieldComponent(seg.fields[5], seg.seps, 0))
	if raw == "" {
		raw = strings.TrimSpace(seg.fields[5])
	}
	if raw == "" && (strings.EqualFold(valType, "SN") || strings.EqualFold(valType, "NM")) {
		for _, idx := range []int{1, 2, 3} {
			raw = strings.TrimSpace(fieldComponent(seg.fields[5], seg.seps, idx))
			if raw != "" {
				break
			}
		}
	}
	if raw == "" && len(seg.fields) > 6 {
		raw = strings.TrimSpace(fieldComponent(seg.fields[6], seg.seps, 0))
		if raw == "" {
			raw = strings.TrimSpace(seg.fields[6])
		}
	}
	return raw
}

func isTempOBXIdentifier(key, identifierField string) bool {
	if key == "MDC_TEMP" || key == "150344" || key == "188440" || key == "188472" {
		return true
	}
	u := strings.ToUpper(identifierField)
	return strings.Contains(u, "150344") || strings.Contains(u, "MDC_TEMP")
}

func parseTempOBX(v *VitalsSnapshot, rawVal, units string, obsTime time.Time, info *[]string, seg hl7Segment) bool {
	logTemp := func(msg string) {
		if info != nil {
			*info = append(*info, msg)
		}
	}
	if rawVal == "" && len(seg.fields) > 2 {
		logTemp(fmt.Sprintf("Monitor temp OBX: type=%s id=%q (empty value field)", seg.fields[2], fieldComponent(seg.fields[3], seg.seps, 0)))
		return false
	}
	if isSentinelRaw(rawVal) {
		logTemp(fmt.Sprintf("Monitor temp OBX: raw=%s (no reading in HL7)", rawVal))
		return false
	}
	f, ok := validFloat(rawVal)
	if !ok {
		logTemp(fmt.Sprintf("Monitor temp OBX: raw=%q units=%q (not a numeric value)", rawVal, units))
		return false
	}
	c, ok := ConvertTempToCelsius(f, units)
	if !ok {
		logTemp(fmt.Sprintf("Monitor temp OBX: raw=%s units=%q (unknown unit)", rawVal, units))
		return false
	}
	if !isPlausibleBodyTempC(c) {
		logTemp(fmt.Sprintf("Monitor temp OBX: raw=%s units=%q -> %.2f °C (out of range)", rawVal, units, c))
		return false
	}
	applyBodyTemp(v, c, obsTime)
	logTemp(fmt.Sprintf("Monitor temp OBX: raw=%s units=%q -> %.1f °C (buffered)", rawVal, units, c))
	return true
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
	// TR8 / MDC use -1, -999, -99.9 when a sensor has no reading.
	if f < 0 {
		return 0, false
	}
	return f, true
}

func isSentinelRaw(raw string) bool {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "-1", "-999", "-99.9", "-99", "-999.0":
		return true
	default:
		return false
	}
}

func isPlausibleBodyTempC(celsius float64) bool {
	return celsius >= 25 && celsius <= 45
}

func applyBodyTemp(v *VitalsSnapshot, celsius float64, obsTime time.Time) {
	if v == nil {
		return
	}
	if !v.Temp.Valid {
		v.Temp = FloatReading{Value: celsius, Valid: true, ObservedAt: obsTime}
		return
	}
	if !obsTime.IsZero() && !v.Temp.ObservedAt.IsZero() && obsTime.Before(v.Temp.ObservedAt) {
		return
	}
	v.Temp = FloatReading{Value: celsius, Valid: true, ObservedAt: obsTime}
}
