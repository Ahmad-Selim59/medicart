//go:build windows

package monitor

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func openUDPListen(host string, port int) (net.PacketConn, string, error) {
	if strings.TrimSpace(host) == "" {
		host = "0.0.0.0"
	}
	if port <= 0 {
		port = 5000
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
			})
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		pc, err = lc.ListenPacket(context.Background(), "udp", addr)
	}
	if err != nil {
		return nil, addr, err
	}
	return pc, addr, nil
}
