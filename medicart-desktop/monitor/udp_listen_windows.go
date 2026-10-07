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
	if port <= 0 {
		port = 5000
	}
	host = strings.TrimSpace(host)
	var ip net.IP
	switch host {
	case "", "0.0.0.0":
		ip = net.IPv4zero
	default:
		ip = net.ParseIP(host)
		if ip == nil {
			return nil, "", fmt.Errorf("invalid bind host %q", host)
		}
	}
	addr := &net.UDPAddr{IP: ip, Port: port}
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
			})
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", addr.String())
	if err != nil {
		return nil, addr.String(), err
	}
	return pc, pc.LocalAddr().String(), nil
}
