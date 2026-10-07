//go:build !windows

package monitor

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// openUDPListen binds an IPv4 UDP socket (matches typical TR8 / PowerShell UdpClient behavior on Windows).
func openUDPListen(host string, port int) (net.PacketConn, string, error) {
	if strings.TrimSpace(host) == "" {
		host = "0.0.0.0"
	}
	if port <= 0 {
		port = 5000
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		// Fallback for platforms where udp4 on 0.0.0.0 fails.
		pc, err2 := lc.ListenPacket(context.Background(), "udp", addr)
		if err2 != nil {
			return nil, addr, err
		}
		return pc, addr, nil
	}
	return pc, addr, nil
}
