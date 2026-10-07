package monitor

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// ListenConfig configures the UDP listener.
type ListenConfig struct {
	BindHost string
	UDPPort  int
	AllowIP  string
}

// Listen binds UDP and processes HL7 datagrams until ctx is cancelled.
// onInfo is called from the receiver goroutine — must be non-blocking (do not call fyne.Do from it).
func Listen(ctx context.Context, cfg ListenConfig, state *MonitorState, onInfo func(string)) {
	host := strings.TrimSpace(cfg.BindHost)
	if host == "" {
		host = "0.0.0.0"
	}
	port := cfg.UDPPort
	if port <= 0 {
		port = DefaultUDPPort
	}

	pc, boundAddr, err := openUDPListen(host, port)
	if err != nil {
		if onInfo != nil {
			onInfo(FormatBindError(boundAddr, err))
		}
		state.SetListenError(err)
		return
	}
	state.ClearListenError()
	defer pc.Close()

	state.SetListening(boundAddr)
	allowIP := strings.TrimSpace(cfg.AllowIP)

	if onInfo != nil {
		onInfo(fmt.Sprintf("Monitor UDP bound to %s (udp4) — source IP filter: %s", boundAddr, allowIPLabel(allowIP)))
	}
	go runUDPSelfTest(ctx, boundAddr, port, onInfo)

	buf := make([]byte, 65507)
	var packetsSeen int64
	var filteredLogged bool
	var noMSHLogged int
	lastTempLogBySub := make(map[string]string)
	reassemblers := make(map[string]*hl7Reassembler)

	handleMessages := func(raws [][]byte, now time.Time) {
		for _, raw := range raws {
			msgs, parseErr := ParseHL7Payload(raw)
			if parseErr != nil {
				state.RecordParseError()
				if onInfo != nil {
					onInfo(fmt.Sprintf("Monitor HL7 parse error: %v", parseErr))
				}
				continue
			}
			state.RecordMessagesParsed(len(msgs))
			if len(msgs) == 0 {
				if onInfo != nil && noMSHLogged < 3 {
					noMSHLogged++
					onInfo(fmt.Sprintf("Monitor: dropped %d bytes with no MSH (joined mid-message?); preview: %q", len(raw), previewBytes(raw, 80)))
				}
				continue
			}
			for _, msg := range msgs {
				applyParsedMonitorMessage(state, msg, now)
				if onInfo == nil {
					continue
				}
				for _, line := range msg.Info {
					if sub := tempLogSubKey(line); sub != "" {
						if lastTempLogBySub[sub] == line {
							continue
						}
						lastTempLogBySub[sub] = line
					}
					onInfo(line)
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, remote, err := pc.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				now := time.Now()
				for _, r := range reassemblers {
					handleMessages(r.FlushIdle(now), now)
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
				if onInfo != nil {
					onInfo(fmt.Sprintf("Monitor UDP read error: %v", err))
				}
				continue
			}
		}

		sourceIP := remoteIPString(remote)
		if !ipAllowed(sourceIP, allowIP) {
			state.RecordFilteredPacket()
			if onInfo != nil && !filteredLogged {
				filteredLogged = true
				onInfo(fmt.Sprintf("Monitor: dropping packets from %s (Allow IP is %q — clear it in Settings to accept any monitor)", sourceIP, allowIP))
			}
			continue
		}

		now := time.Now()
		state.RecordPacket(sourceIP, now)
		packetsSeen++
		if onInfo != nil && packetsSeen == 1 {
			onInfo(fmt.Sprintf("Monitor: first UDP packet from %s (%d bytes)", sourceIP, n))
		}

		r := reassemblers[sourceIP]
		if r == nil {
			r = &hl7Reassembler{}
			reassemblers[sourceIP] = r
		}
		handleMessages(r.Push(buf[:n], now), now)
		if r.Overflowed() && onInfo != nil {
			onInfo(fmt.Sprintf("Monitor: discarded oversized partial HL7 buffer from %s (no message end seen)", sourceIP))
		}
	}
}

func applyParsedMonitorMessage(state *MonitorState, msg *ParsedMessage, now time.Time) {
	if msg == nil {
		return
	}
	if msg.Waveform != nil {
		state.ApplyWaveform(msg.Waveform, now)
	}
	if msg.Skip {
		if msg.Patient != nil {
			state.ApplyParsedMessage(&ParsedMessage{Patient: msg.Patient}, now)
		}
		return
	}
	state.ApplyParsedMessage(msg, now)
}

// tempLogSubKey returns the temp channel of a "Monitor temp OBX: sub=…" line so repeats
// are suppressed per channel (T1, T2 and spot alternate within one message).
func tempLogSubKey(line string) string {
	const marker = "temp OBX: sub="
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := line[i+len(marker):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func previewBytes(b []byte, max int) string {
	s := strings.TrimSpace(string(b[:min(len(b), max)]))
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

func allowIPLabel(allowIP string) string {
	if strings.TrimSpace(allowIP) == "" {
		return "off (accept any monitor IP)"
	}
	return allowIP
}

// runUDPSelfTest sends one datagram to localhost to verify the socket can receive.
func runUDPSelfTest(ctx context.Context, boundAddr string, port int, onInfo func(string)) {
	time.Sleep(300 * time.Millisecond)
	select {
	case <-ctx.Done():
		return
	default:
	}
	_, boundPortStr, err := net.SplitHostPort(boundAddr)
	if err != nil {
		boundPortStr = fmt.Sprintf("%d", port)
	}
	target := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: mustAtoi(boundPortStr, port)}
	payload := []byte("MSH|^~\\&|SELFTEST|||||ORU^R01|1|P|2.4\r")
	conn, err := net.DialUDP("udp4", nil, target)
	if err != nil {
		if onInfo != nil {
			onInfo(fmt.Sprintf("Monitor UDP self-test dial failed: %v", err))
		}
		return
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		if onInfo != nil {
			onInfo(fmt.Sprintf("Monitor UDP self-test send failed: %v", err))
		}
		return
	}
	if onInfo != nil {
		onInfo("Monitor UDP self-test sent — if packet count stays 0, Windows may be blocking inbound UDP to this app (firewall)")
	}
}

func mustAtoi(s string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}
