package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// LineParser function signature
type LineParser func(line string) (interface{}, error)

var (
	currentCmd *exec.Cmd
	cmdMutex   sync.Mutex
	cancelFunc context.CancelFunc
	previewMu  sync.Mutex
	previewCancel context.CancelFunc
)

func main() {
	myApp := app.New()
	myWindow := myApp.NewWindow("Medicart Uploader")

	// Theme Toggle
	lightModeCheck := widget.NewCheck("Light Mode", func(checked bool) {
		if checked {
			myApp.Settings().SetTheme(theme.LightTheme())
		} else {
			myApp.Settings().SetTheme(theme.DarkTheme())
		}
	})

	// URL Input
	urlLabel := widget.NewLabel("Web Server URL:")
	urlEntry := widget.NewEntry()
	urlEntry.SetPlaceHolder("http://your-server.com/api/ingest")
	urlEntry.Text = "http://localhost:8080/api/data" // Default for testing

	// Patient Name Input
	patientNameLabel := widget.NewLabel("Patient Name:")
	patientNameEntry := widget.NewEntry()
	patientNameEntry.SetPlaceHolder("Enter patient name")

	// Status Area
	statusLabel := widget.NewRichTextFromMarkdown("Status: Idle")
	logArea := widget.NewMultiLineEntry()
	logArea.Disable()
	logArea.SetMinRowsVisible(10)

	// Camera Device Input (for ffmpeg dshow)
	cameraLabel := widget.NewLabel("Camera Device (dshow name):")
	cameraEntry := widget.NewEntry()
	cameraEntry.SetPlaceHolder(`video="Integrated Camera"`)
	cameraEntry.SetText(`video="Integrated Camera"`)

	// Camera Preview
	previewImage := canvas.NewImageFromImage(nil)
	previewImage.FillMode = canvas.ImageFillContain
	previewImage.SetMinSize(fyne.NewSize(320, 240))

	log := func(msg string) {
		fyne.Do(func() {
			timestamp := time.Now().Format("15:04:05")
			logArea.SetText(fmt.Sprintf("[%s] %s\n%s", timestamp, msg, logArea.Text))

			statusText := "Status: " + msg
			isError := strings.HasPrefix(msg, "Error") || strings.Contains(strings.ToLower(msg), "error")

			if isError {
				statusLabel.Segments = []widget.RichTextSegment{
					&widget.TextSegment{
						Text: statusText,
						Style: widget.RichTextStyle{
							ColorName: theme.ColorNameError,
							Inline:    true,
							TextStyle: fyne.TextStyle{Bold: true},
						},
					},
				}
			} else {
				statusLabel.Segments = []widget.RichTextSegment{
					&widget.TextSegment{
						Text: statusText,
						Style: widget.RichTextStyle{
							ColorName: theme.ColorNameForeground,
							Inline:    true,
						},
					},
				}
			}
			statusLabel.Refresh()
		})
	}

	// Action Buttons
	var stopBtn *widget.Button

	startProcess := func(name string, args []string, parser LineParser) {
		cmdMutex.Lock()
		if currentCmd != nil {
			cmdMutex.Unlock()
			log("Error: A process is already running. Stop it first.")
			return
		}
		cmdMutex.Unlock()

		targetURL := urlEntry.Text
		if targetURL == "" {
			log("Error: Please enter a Web Server URL")
			return
		}

		patientName := patientNameEntry.Text
		if patientName == "" {
			log("Error: Please enter a Patient Name")
			return
		}

		stopBtn.Enable()
		go runCLIAndSend(name, args, parser, targetURL, patientName, log, func() {
			fyne.Do(func() {
				stopBtn.Disable()
			})
		})
	}

	stopBtn = widget.NewButton("Stop", func() {
		cmdMutex.Lock()
		defer cmdMutex.Unlock()
		if cancelFunc != nil {
			cancelFunc() // Cancel the context
			log("Stopping process...")
		}
	})
	stopBtn.Disable()

	btnHeartRate := widget.NewButton("Start Heart Rate / SpO2", func() {
		startProcess("HeartRate", []string{"-heartrate"}, parseHeartRateLine)
	})

	btnNIBP := widget.NewButton("Start NIBP", func() {
		startProcess("NIBP", []string{"-nibp"}, parseNIBPLine)
	})

	btnGlucose := widget.NewButton("Start Glucose", func() {
		startProcess("Glucose", []string{"-glu"}, parseGlucoseLine)
	})

	btnTemp := widget.NewButton("Start Temperature", func() {
		startProcess("Temperature", []string{"-temperature"}, parseTemperatureLine)
	})

	runCameraCommand := func(action string, args []string) {
		go func() {
			log(fmt.Sprintf("Camera: %s ...", action))

			cmdPath := "camera_cli.exe"
			if _, err := exec.LookPath(cmdPath); err != nil {
				cmdPath = "./camera_cli.exe"
			}

			cmd := exec.Command(cmdPath, args...)
			outputBytes, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(outputBytes))

			if output != "" {
				log(fmt.Sprintf("Camera output: %s", output))
			}
			if err != nil {
				log(fmt.Sprintf("Error running camera %s: %v", action, err))
				return
			}

			upper := strings.ToUpper(output)
			if strings.HasPrefix(upper, "DATA:ERROR") {
				log(fmt.Sprintf("Camera %s reported error: %s", action, output))
				return
			}

			log(fmt.Sprintf("Camera %s completed", action))
		}()
	}

	btnCamList := widget.NewButton("List Cameras", func() {
		runCameraCommand("list", []string{"-list"})
	})
	btnCamLeft := widget.NewButton("Move Left", func() {
		runCameraCommand("move-left", []string{"-move-left"})
	})
	btnCamRight := widget.NewButton("Move Right", func() {
		runCameraCommand("move-right", []string{"-move-right"})
	})
	btnCamUp := widget.NewButton("Move Up", func() {
		runCameraCommand("move-up", []string{"-move-up"})
	})
	btnCamDown := widget.NewButton("Move Down", func() {
		runCameraCommand("move-down", []string{"-move-down"})
	})


	// Camera Preview (snapshot via ffmpeg dshow)
	stopPreviewInternal := func(logMsg string) {
		previewMu.Lock()
		if previewCancel != nil {
			previewCancel()
			previewCancel = nil
			if logMsg != "" {
				log(logMsg)
			}
		}
		previewMu.Unlock()
	}

	startPreview := func() {
		device := strings.TrimSpace(cameraEntry.Text)
		if device == "" {
			if autoDevice, err := detectDefaultCameraDevice(); err == nil && autoDevice != "" {
				device = autoDevice
				log(fmt.Sprintf("Using detected camera: %s", device))
			} else {
				log("Error: No camera device found. Set a device name (e.g. video=\"Integrated Camera\")")
				return
			}
		}

		previewMu.Lock()
		if previewCancel != nil {
			previewMu.Unlock()
			log("Error: Preview already running")
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		previewCancel = cancel
		previewMu.Unlock()

		log(fmt.Sprintf("Starting camera preview for %s", device))

		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					img, err := captureSnapshot(ctx, device)
					if err != nil {
						log(fmt.Sprintf("Error capturing frame: %v", err))
						continue
					}
					fyne.Do(func() {
						previewImage.Image = img
						previewImage.Refresh()
					})
				}
			}
		}()
	}

	stopPreview := func() {
		stopPreviewInternal("Camera preview stopped")
	}

	btnPreviewStart := widget.NewButton("Start Preview", startPreview)
	btnPreviewStop := widget.NewButton("Stop Preview", stopPreview)
	// Layout
	mainContent := container.NewVBox(
		lightModeCheck,
		urlLabel,
		urlEntry,
		patientNameLabel,
		patientNameEntry,
		widget.NewSeparator(),
		widget.NewLabel("Select Sensor to Monitor:"),
		btnHeartRate,
		btnNIBP,
		btnGlucose,
		btnTemp,
		widget.NewSeparator(),
		widget.NewLabel("Camera Controls:"),
		cameraLabel,
		cameraEntry,
		btnCamList,
		btnCamLeft,
		btnCamRight,
		btnCamUp,
		btnCamDown,
		container.NewHBox(btnPreviewStart, btnPreviewStop),
		previewImage,
		widget.NewSeparator(),
		stopBtn,
		widget.NewSeparator(),
		statusLabel,
		logArea,
	)

	myWindow.SetContent(container.NewVScroll(mainContent))
	myWindow.Resize(fyne.NewSize(420, 720))
	myWindow.ShowAndRun()
}

func runCLIAndSend(name string, args []string, parser LineParser, targetURL string, patientName string, log func(string), onFinish func()) {
	defer onFinish()

	ctx, cancel := context.WithCancel(context.Background())
	
	cmdMutex.Lock()
	cancelFunc = cancel
	cmdMutex.Unlock()

	defer func() {
		cmdMutex.Lock()
		currentCmd = nil
		cancelFunc = nil
		cmdMutex.Unlock()
	}()

	cmdPath := "lepu_cli.exe"
	if _, err := exec.LookPath(cmdPath); err != nil {
		cmdPath = "./lepu_cli.exe"
	}
	
	log(fmt.Sprintf("Starting %s (%s)...", name, cmdPath))

	cmd := exec.CommandContext(ctx, cmdPath, args...)
	
	cmdMutex.Lock()
	currentCmd = cmd
	cmdMutex.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log(fmt.Sprintf("Error creating stdout pipe: %v", err))
		return
	}

	if err := cmd.Start(); err != nil {
		log(fmt.Sprintf("Error starting process: %v", err))
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		data, err := parser(line)
		if err != nil {
			// Parser error usually means skip
			continue
		}

		if data != nil {
			// Inject Patient Name
			if dataMap, ok := data.(map[string]interface{}); ok {
				dataMap["patient_name"] = patientName
			}

			// Send to server
			log(fmt.Sprintf("Sending data: %v", data))
			if err := sendData(targetURL, data); err != nil {
				log(fmt.Sprintf("Error sending data: %v", err))
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.Canceled {
			log("Process stopped by user.")
		} else {
			log(fmt.Sprintf("Process finished with error: %v", err))
		}
	} else {
		log("Process finished successfully.")
	}
}

func sendData(url string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned status: %s", resp.Status)
	}
	return nil
}

// captureSnapshot uses ffmpeg (dshow) to grab a single JPEG frame from the given device name.
func captureSnapshot(ctx context.Context, device string) (image.Image, error) {
	// Example device string: video="Integrated Camera"
	args := []string{
		"-f", "dshow",
		"-i", device,
		"-vframes", "1",
		"-f", "mjpeg",
		"-",
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg run error: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}

	img, err := jpeg.Decode(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("decode jpeg error: %v", err)
	}
	return img, nil
}

// detectDefaultCameraDevice tries to find the first dshow video device via ffmpeg -list_devices.
func detectDefaultCameraDevice() (string, error) {
	cmd := exec.Command("ffmpeg", "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	var stderr bytes.Buffer
	cmd.Stdout = &stderr // ffmpeg prints device list to stderr; stdout unused
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Listing devices returns an error exit code; that's fine as long as we get output.
	}
	lines := strings.Split(stderr.String(), "\n")
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		// Match lines like: [dshow @ ...] "USB3.0 FULL HD PTZ" (video)
		if strings.Contains(ln, "(video)") && strings.Count(ln, "\"") >= 2 {
			start := strings.Index(ln, "\"")
			end := strings.LastIndex(ln, "\"")
			if start >= 0 && end > start {
				name := ln[start+1 : end]
				if name != "" {
					return fmt.Sprintf(`video="%s"`, name), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no video devices found")
}

// --- Parsers (Copied from legacy/main.go) ---

// Heart Rate / SpO2
// Output: DATA:PR=75,SPO2=98
// Or Status: STATUS:PROBE_OFF
func parseHeartRateLine(line string) (interface{}, error) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "DATA:") {
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
		status := strings.TrimPrefix(line, "STATUS:")
		return map[string]interface{}{
			"type": "status",
			"msg":  status,
		}, nil
	}
	return nil, nil
}

// NIBP
func parseNIBPLine(line string) (interface{}, error) {
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
		parts := strings.Split(partsStr, ",")
		resultMap := make(map[string]string)
		
		for _, p := range parts {
			if strings.Contains(p, "=") {
				kv := strings.SplitN(p, "=", 2)
				if len(kv) == 2 {
					resultMap[kv[0]] = kv[1]
				}
			} else {
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
		irr := irrVal == "TRUE"

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
