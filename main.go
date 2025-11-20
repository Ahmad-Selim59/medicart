package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true 
	},
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Medicart Server is running")
	})

	http.HandleFunc("/api/heartrate", handleHeartRate)
	http.HandleFunc("/api/nibp", handleNIBP)
	http.HandleFunc("/api/glucose", handleGlucose)
	http.HandleFunc("/api/temperature", handleTemperature)

	port := ":8080"
	fmt.Printf("Server starting on port %s...\n", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}


func handleHeartRate(w http.ResponseWriter, r *http.Request) {
	runCLIAndStream(w, r, []string{"-heartrate"}, parseHeartRateLine)
}

func handleNIBP(w http.ResponseWriter, r *http.Request) {
	runCLIAndStream(w, r, []string{"-nibp"}, parseNIBPLine)
}

func handleGlucose(w http.ResponseWriter, r *http.Request) {
	runCLIAndStream(w, r, []string{"-glu"}, parseGlucoseLine)
}

func handleTemperature(w http.ResponseWriter, r *http.Request) {
	runCLIAndStream(w, r, []string{"-temperature"}, parseTemperatureLine)
}

// --- Core Streaming Logic ---

type LineParser func(line string) (interface{}, error)

func runCLIAndStream(w http.ResponseWriter, r *http.Request, args []string, parser LineParser) {
	// Upgrade to WebSocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade error: %v", err)
		return
	}
	defer ws.Close()

	// Context for cancellation when client disconnects
	// WebSocket CloseHandler doesn't automatically cancel a context, 
	// so we'll listen for the close message or a write error.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Handle client disconnects (ReadMessage will fail if client closes)
	go func() {
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	// Prepare command
	// We assume lepu_cli.exe is in the PATH or current directory
	// For local dev on macOS/Linux, we might need ./ prefix if it's in CWD
	cmdPath := "lepu_cli.exe"
	if _, err := exec.LookPath(cmdPath); err != nil {
		// Not in path, try current directory
		cmdPath = "./lepu_cli.exe"
	}
	cmd := exec.CommandContext(ctx, cmdPath, args...)
	
	// Stdout pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ws.WriteJSON(map[string]string{"type": "error", "message": "Failed to get stdout pipe"})
		return
	}

	if err := cmd.Start(); err != nil {
		ws.WriteJSON(map[string]string{"type": "error", "message": "Failed to start process: " + err.Error()})
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		data, err := parser(line)
		if err != nil {
			continue 
		}
		
		if data != nil {
			if err := ws.WriteJSON(data); err != nil {
				break
			}
		}
	}

	// Clean up
	if err := cmd.Wait(); err != nil {
		// Process might have been killed by context, which is expected on disconnect
		log.Printf("Process finished with: %v", err)
	}
}

// --- Parsers ---

// Heart Rate / SpO2
// Output: DATA:PR=75,SPO2=98
// Or Status: STATUS:PROBE_OFF
func parseHeartRateLine(line string) (interface{}, error) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "DATA:") {
		// DATA:PR=75,SPO2=98
		parts := strings.TrimPrefix(line, "DATA:")
		kv := parseKV(parts)
		
		pr, _ := strconv.Atoi(kv["PR"])
		spo2, _ := strconv.Atoi(kv["SPO2"])

		return map[string]interface{}{
			"type": "data",
			"pr":   pr,
			"spo2": spo2,
		}, nil
	} else if strings.HasPrefix(line, "STATUS:") {
		// STATUS:PROBE_OFF
		status := strings.TrimPrefix(line, "STATUS:")
		return map[string]interface{}{
			"type": "status",
			"msg":  status,
		}, nil
	}
	return nil, nil
}

// NIBP
// Live: DATA:CUFF_PRESSURE=120
// Result: DATA:NIBP_RESULT:SYS=110,DIA=70,MAP=85,PR=72,IRR=False
//        (Or variant: DATA:NIBP_RESULT:SYS=125,DIA=77,MAP94,PR65,IRR=FALSE)
// Error: STATUS:NIBP_ERROR=5
func parseNIBPLine(line string) (interface{}, error) {
	// Clean up the line: remove spaces and carriage returns
	normalized := strings.ReplaceAll(line, " ", "")
	normalized = strings.ReplaceAll(normalized, "\r", "")
	normalized = strings.ToUpper(normalized)

	if strings.HasPrefix(normalized, "DATA:CUFF_PRESSURE=") {
		valStr := strings.TrimPrefix(normalized, "DATA:CUFF_PRESSURE=")
		val, _ := strconv.Atoi(valStr)
		return map[string]interface{}{
			"type":          "cuff_update",
			"cuff_pressure": val,
		}, nil
	} else if strings.HasPrefix(normalized, "DATA:NIBP_RESULT:") {
		partsStr := strings.TrimPrefix(normalized, "DATA:NIBP_RESULT:")
		
		// Custom parsing for the result to handle "MAP94" (missing =)
		// Split by comma
		parts := strings.Split(partsStr, ",")
		resultMap := make(map[string]string)
		
		for _, p := range parts {
			if strings.Contains(p, "=") {
				kv := strings.SplitN(p, "=", 2)
				if len(kv) == 2 {
					resultMap[kv[0]] = kv[1]
				}
			} else {
				// Handle cases like MAP94, PR65
				if strings.HasPrefix(p, "MAP") {
					resultMap["MAP"] = strings.TrimPrefix(p, "MAP")
				} else if strings.HasPrefix(p, "PR") {
					resultMap["PR"] = strings.TrimPrefix(p, "PR")
				} else if strings.HasPrefix(p, "SYS") {
					resultMap["SYS"] = strings.TrimPrefix(p, "SYS")
				} else if strings.HasPrefix(p, "DIA") {
					resultMap["DIA"] = strings.TrimPrefix(p, "DIA")
				}
			}
		}

		sys, _ := strconv.Atoi(resultMap["SYS"])
		dia, _ := strconv.Atoi(resultMap["DIA"])
		mean, _ := strconv.Atoi(resultMap["MAP"])
		pr, _ := strconv.Atoi(resultMap["PR"])
		
		irrVal := resultMap["IRR"]
		irr := irrVal == "TRUE" // We normalized to uppercase

		return map[string]interface{}{
			"type": "result",
			"sys":  sys,
			"dia":  dia,
			"map":  mean,
			"pr":   pr,
			"irr":  irr,
		}, nil
	} else if strings.HasPrefix(normalized, "STATUS:NIBP_ERROR=") {
		codeStr := strings.TrimPrefix(normalized, "STATUS:NIBP_ERROR=")
		code, _ := strconv.Atoi(codeStr)
		return map[string]interface{}{
			"type": "error",
			"code": code,
		}, nil
	} else if strings.HasPrefix(normalized, "STATUS:NIBP_END") {
		return map[string]interface{}{
			"type": "status",
			"msg":  "NIBP_END",
		}, nil
	}
	return nil, nil
}

// Glucose
// Output: DATA:GLU=105
func parseGlucoseLine(line string) (interface{}, error) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "DATA:GLU=") {
		valStr := strings.TrimPrefix(line, "DATA:GLU=")
		val, _ := strconv.Atoi(valStr)
		return map[string]interface{}{
			"type": "data",
			"glu":  val,
		}, nil
	}
	return nil, nil
}

// Temperature
// Output: DATA:TEMP=36.5
func parseTemperatureLine(line string) (interface{}, error) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "DATA:TEMP=") {
		valStr := strings.TrimPrefix(line, "DATA:TEMP=")
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"type": "data",
			"temp": val,
		}, nil
	}
	return nil, nil
}

// Helper to parse comma-separated Key=Value pairs
// e.g., "PR=75,SPO2=98" -> map[PR:75 SPO2:98]
func parseKV(input string) map[string]string {
	result := make(map[string]string)
	pairs := strings.Split(input, ",")
	for _, p := range pairs {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}
