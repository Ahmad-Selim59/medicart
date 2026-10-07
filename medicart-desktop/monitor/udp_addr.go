package monitor

import (
	"net"
	"strings"
)

func remoteIPString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	return ip.String()
}

func ipAllowed(remoteIP, allowIP string) bool {
	allowIP = strings.TrimSpace(allowIP)
	if allowIP == "" {
		return true
	}
	want := net.ParseIP(allowIP)
	got := net.ParseIP(remoteIP)
	if want != nil && got != nil {
		return got.Equal(want)
	}
	return remoteIP == allowIP
}
