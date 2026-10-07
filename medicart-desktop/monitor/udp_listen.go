//go:build !windows

package monitor

import (
	"fmt"
	"net"
	"strings"
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
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, addr.String(), err
	}
	return conn, conn.LocalAddr().String(), nil
}
