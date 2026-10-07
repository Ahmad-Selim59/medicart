package monitor

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatBindError_WindowsForbidden(t *testing.T) {
	err := errors.New(`listen udp4 0.0.0.0:5000: bind: An attempt was made to access a socket in a way forbidden by its access permissions.`)
	got := FormatBindError("0.0.0.0:5000", err)
	if !strings.Contains(got, "5500") {
		t.Fatalf("expected port change hint, got: %s", got)
	}
}
