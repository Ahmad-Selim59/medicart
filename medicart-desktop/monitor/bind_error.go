package monitor

import (
	"fmt"
	"strings"
)

// FormatBindError turns a UDP bind failure into actionable text (especially Windows excluded ports).
func FormatBindError(boundAddr string, err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	base := fmt.Sprintf("Monitor UDP listen failed on %s: %v", boundAddr, err)

	lower := strings.ToLower(msg)
	if strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "access permissions") ||
		strings.Contains(lower, "permission denied") {
		return base + `

Windows blocked this port (common for UDP 5000 when Hyper-V, WSL, or Docker is installed).
Fix options:
  1) Pick another port in Settings (e.g. 5500 or 9000) and set the same port on the TR8 HL7 destination.
  2) In Admin PowerShell, run: netsh interface ipv4 show excludedportrange protocol=udp
     If 5000 is inside a listed range, either use a port outside those ranges or adjust Hyper-V/WSL port exclusions (then reboot).
  3) Ensure no other app (including a PowerShell test listener) is using the port.`
	}
	if strings.Contains(lower, "address already in use") || strings.Contains(lower, "only one usage") {
		return base + "\n\nAnother program is already using this UDP port. Close PowerShell listeners or other apps bound to the same port."
	}
	return base
}
