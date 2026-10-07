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
func Listen(ctx context.Context, cfg ListenConfig, state *MonitorState, onError func(string)) {
	host := strings.TrimSpace(cfg.BindHost)
	if host == "" {
		host = "0.0.0.0"
	}
	port := cfg.UDPPort
	if port <= 0 {
		port = 5000
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		if onError != nil {
			onError(fmt.Sprintf("Monitor UDP listen failed on %s: %v", addr, err))
		}
		return
	}
	defer pc.Close()

	state.SetListening(addr)
	if onError != nil {
		onError(fmt.Sprintf("Monitor UDP listening on %s", addr))
	}

	buf := make([]byte, 65507)
	var packetsSeen int64
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, remote, err := pc.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
				if onError != nil {
					onError(fmt.Sprintf("Monitor UDP read error: %v", err))
				}
				continue
			}
		}
		if cfg.AllowIP != "" {
			hostIP := remote.String()
			if h, _, splitErr := net.SplitHostPort(hostIP); splitErr == nil {
				hostIP = h
			}
			if hostIP != strings.TrimSpace(cfg.AllowIP) {
				continue
			}
		}

		now := time.Now()
		sourceIP := remote.String()
		if h, _, splitErr := net.SplitHostPort(sourceIP); splitErr == nil {
			sourceIP = h
		}
		state.RecordPacket(sourceIP, now)
		packetsSeen++

		msgs, parseErr := ParseHL7Payload(buf[:n])
		if parseErr != nil {
			state.RecordParseError()
			if onError != nil {
				onError(fmt.Sprintf("Monitor HL7 parse error: %v", parseErr))
			}
			continue
		}
		state.RecordMessagesParsed(len(msgs))
		if len(msgs) == 0 && onError != nil && packetsSeen <= 3 {
			preview := strings.TrimSpace(string(buf[:min(n, 80)]))
			preview = strings.ReplaceAll(preview, "\r", `\r`)
			preview = strings.ReplaceAll(preview, "\n", `\n`)
			onError(fmt.Sprintf("Monitor: UDP packet (%d bytes) had no HL7 MSH segment; preview: %q", n, preview))
		}
		for _, msg := range msgs {
			if msg == nil || msg.Skip {
				if msg != nil && msg.Patient != nil {
					state.ApplyParsedMessage(&ParsedMessage{Patient: msg.Patient}, now)
				}
				continue
			}
			state.ApplyParsedMessage(msg, now)
		}
	}
}
