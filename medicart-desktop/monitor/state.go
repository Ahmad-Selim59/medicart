package monitor

import (
	"strings"
	"sync"
	"time"
)

// MaxStaleness is how old a cached vital may be when committing via UI button.
const MaxStaleness = 60 * time.Second

// ProfileDebounce is the delay before auto-uploading profile after patient change.
const ProfileDebounce = 2 * time.Second

// VitalKind identifies a committable vital group.
type VitalKind string

const (
	VitalHeartRate VitalKind = "HeartRate"
	VitalNIBP      VitalKind = "NIBP"
	VitalTemp      VitalKind = "Temp"
)

// FloatReading holds one numeric observation.
type FloatReading struct {
	Value      float64
	ObservedAt time.Time
	Valid      bool
}

// IntReading holds one integer observation.
type IntReading struct {
	Value      int
	ObservedAt time.Time
	Valid      bool
}

// ConnectionMeta tracks UDP activity.
type ConnectionMeta struct {
	ListenAddr      string
	ListenError     string
	LastPacketAt    time.Time
	SourceIP        string
	PacketsReceived int64
	PacketsFiltered int64
	MessagesParsed  int64
	ParseErrors     int64
}

// PatientSnapshot is demographics from HL7 PID/PV1 (+ weight/height OBX).
type PatientSnapshot struct {
	PatientID   string
	PatientName string
	ClinicName  string
	BedID       string
	Gender      string
	Age         int
	DOB         string
	AgeGroup    string
	Weight      float64
	Height      float64
	UpdatedAt   time.Time
}

// VitalsSnapshot is the latest cached monitor readings.
type VitalsSnapshot struct {
	ECGHeartRate   IntReading
	SpO2Pulse      IntReading
	SpO2           IntReading
	NIBPSys        IntReading
	NIBPDia        IntReading
	NIBPMap        IntReading
	NIBPPulse      IntReading
	Temp           FloatReading
}

// PublicSnapshot is a copy safe for UI without holding the lock.
type PublicSnapshot struct {
	Connection ConnectionMeta
	Patient    PatientSnapshot
	Vitals     VitalsSnapshot
}

// PatientChangeHandler is invoked when the patient key changes (debounced profile upload).
type PatientChangeHandler func(p PatientSnapshot)

// MonitorState holds thread-safe monitor caches.
type MonitorState struct {
	mu sync.RWMutex

	connection ConnectionMeta
	patient    PatientSnapshot
	vitals     VitalsSnapshot
	waveform   *WaveformCache

	lastPatientKey string

	onPatientChange PatientChangeHandler
	debounceMu      sync.Mutex
	debounceTimer   *time.Timer
	pendingPatient  PatientSnapshot
}

// NewMonitorState creates an empty monitor state.
func NewMonitorState() *MonitorState {
	return &MonitorState{
		waveform: NewWaveformCache(),
	}
}

// SetPatientChangeHandler registers a callback fired after ProfileDebounce on patient key change.
func (s *MonitorState) SetPatientChangeHandler(h PatientChangeHandler) {
	s.mu.Lock()
	s.onPatientChange = h
	s.mu.Unlock()
}

// Snapshot returns a copy of current state.
func (s *MonitorState) Snapshot() PublicSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return PublicSnapshot{
		Connection: s.connection,
		Patient:    s.patient,
		Vitals:     s.vitals,
	}
}

// SetListening records the bound UDP address after a successful listen.
func (s *MonitorState) SetListening(addr string) {
	s.mu.Lock()
	s.connection.ListenAddr = addr
	s.connection.ListenError = ""
	s.mu.Unlock()
}

// SetListenError records a permanent bind failure for the UI.
func (s *MonitorState) SetListenError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.connection.ListenError = err.Error()
	s.mu.Unlock()
}

// ClearListenError clears a prior bind error after a successful listen.
func (s *MonitorState) ClearListenError() {
	s.mu.Lock()
	s.connection.ListenError = ""
	s.mu.Unlock()
}

// RecordPacket updates connection metadata from a received UDP datagram.
func (s *MonitorState) RecordPacket(sourceIP string, at time.Time) {
	s.mu.Lock()
	s.connection.LastPacketAt = at
	s.connection.SourceIP = sourceIP
	s.connection.PacketsReceived++
	s.mu.Unlock()
}

// RecordMessagesParsed adds to the count of HL7 messages handled from UDP.
func (s *MonitorState) RecordMessagesParsed(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.connection.MessagesParsed += int64(n)
	s.mu.Unlock()
}

// RecordFilteredPacket counts a datagram dropped by the allow-IP filter.
func (s *MonitorState) RecordFilteredPacket() {
	s.mu.Lock()
	s.connection.PacketsFiltered++
	s.mu.Unlock()
}

// RecordParseError increments parse error count.
func (s *MonitorState) RecordParseError() {
	s.mu.Lock()
	s.connection.ParseErrors++
	s.mu.Unlock()
}

// ApplyParsedMessage merges a parsed HL7 message into state.
func (s *MonitorState) ApplyParsedMessage(msg *ParsedMessage, receivedAt time.Time) {
	if msg == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if msg.Patient != nil {
		mergePatient(&s.patient, msg.Patient, receivedAt)
	}
	if msg.Vitals != nil {
		mergeVitals(&s.vitals, msg.Vitals, receivedAt)
	}

	key := patientKey(s.patient)
	if key != "" && key != s.lastPatientKey {
		s.lastPatientKey = key
		s.schedulePatientChange(s.patient)
	}
}

func mergePatient(dst *PatientSnapshot, src *PatientSnapshot, at time.Time) {
	if strings.TrimSpace(src.PatientID) != "" {
		dst.PatientID = strings.TrimSpace(src.PatientID)
	}
	if strings.TrimSpace(src.PatientName) != "" {
		dst.PatientName = strings.TrimSpace(src.PatientName)
	}
	if strings.TrimSpace(src.ClinicName) != "" {
		dst.ClinicName = strings.TrimSpace(src.ClinicName)
	}
	if strings.TrimSpace(src.BedID) != "" {
		dst.BedID = strings.TrimSpace(src.BedID)
	}
	if strings.TrimSpace(src.Gender) != "" {
		dst.Gender = strings.TrimSpace(src.Gender)
	}
	if src.Age > 0 {
		dst.Age = src.Age
	}
	if strings.TrimSpace(src.DOB) != "" {
		dst.DOB = strings.TrimSpace(src.DOB)
	}
	if strings.TrimSpace(src.AgeGroup) != "" {
		dst.AgeGroup = strings.TrimSpace(src.AgeGroup)
	}
	if src.Weight > 0 {
		dst.Weight = src.Weight
	}
	if src.Height > 0 {
		dst.Height = src.Height
	}
	dst.UpdatedAt = at
}

func mergeVitals(dst *VitalsSnapshot, src *VitalsSnapshot, at time.Time) {
	mergeInt(&dst.ECGHeartRate, src.ECGHeartRate, at)
	mergeInt(&dst.SpO2Pulse, src.SpO2Pulse, at)
	mergeInt(&dst.SpO2, src.SpO2, at)
	mergeInt(&dst.NIBPSys, src.NIBPSys, at)
	mergeInt(&dst.NIBPDia, src.NIBPDia, at)
	mergeInt(&dst.NIBPMap, src.NIBPMap, at)
	mergeInt(&dst.NIBPPulse, src.NIBPPulse, at)
	mergeFloat(&dst.Temp, src.Temp, at)
}

func mergeInt(dst *IntReading, src IntReading, at time.Time) {
	if !src.Valid {
		return
	}
	dst.Value = src.Value
	dst.Valid = true
	dst.ObservedAt = at
	if !src.ObservedAt.IsZero() {
		dst.ObservedAt = src.ObservedAt
	}
}

func mergeFloat(dst *FloatReading, src FloatReading, at time.Time) {
	if !src.Valid {
		return
	}
	obs := src.ObservedAt
	if obs.IsZero() {
		obs = at
	}
	if dst.Valid && !dst.ObservedAt.IsZero() && obs.Before(dst.ObservedAt) {
		return
	}
	dst.Value = src.Value
	dst.Valid = true
	dst.ObservedAt = obs
}

func patientKey(p PatientSnapshot) string {
	id := strings.TrimSpace(p.PatientID)
	if id != "" {
		return "id:" + strings.ToLower(id)
	}
	name := strings.ToLower(strings.TrimSpace(p.PatientName))
	bed := strings.ToLower(strings.TrimSpace(p.BedID))
	if name != "" || bed != "" {
		return "nb:" + name + "|" + bed
	}
	return ""
}

func (s *MonitorState) schedulePatientChange(p PatientSnapshot) {
	handler := s.onPatientChange
	if handler == nil {
		return
	}
	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()
	s.pendingPatient = p
	if s.debounceTimer != nil {
		s.debounceTimer.Stop()
	}
	s.debounceTimer = time.AfterFunc(ProfileDebounce, func() {
		s.debounceMu.Lock()
		patient := s.pendingPatient
		s.debounceMu.Unlock()
		handler(patient)
	})
}

// ApplyWaveform merges W01 ECG samples into the rolling cache.
func (s *MonitorState) ApplyWaveform(up *WaveformUpdate, at time.Time) {
	if s.waveform == nil {
		return
	}
	s.waveform.Apply(up, at)
}

// ECGLeadSnapshot returns cached samples for a lead (default Lead II).
func (s *MonitorState) ECGLeadSnapshot(leadKey string) LeadSnapshot {
	if s.waveform == nil {
		return LeadSnapshot{}
	}
	return s.waveform.SnapshotLead(leadKey)
}

// ECGBestLeadSnapshot returns the best ECG lead buffer for preview/commit.
func (s *MonitorState) ECGBestLeadSnapshot() LeadSnapshot {
	if s.waveform == nil {
		return LeadSnapshot{}
	}
	return s.waveform.SnapshotBestECGLead()
}

// PatientForCommit returns the current patient snapshot for ingest.
func (s *MonitorState) PatientForCommit() PatientSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.patient
}

// VitalsForCommit returns vitals snapshot for ingest.
func (s *MonitorState) VitalsForCommit() VitalsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.vitals
}
