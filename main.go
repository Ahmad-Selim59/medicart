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
	"runtime"
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
	"github.com/gorilla/websocket"
)

// LineParser function signature
type LineParser func(line string) (interface{}, error)

var (
	currentCmd *exec.Cmd
	cmdMutex   sync.Mutex
	cancelFunc context.CancelFunc
	previewMu  sync.Mutex
	previewCancel context.CancelFunc
	wsConn    *websocket.Conn
	wsMu      sync.Mutex
	wsCancel  context.CancelFunc
	streamCancel context.CancelFunc
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

	// Clinic Name Input
	clinicNameLabel := widget.NewLabel("Clinic Name:")
	clinicNameEntry := widget.NewEntry()
	clinicNameEntry.SetPlaceHolder("Enter clinic name")

	// Status Area
	statusLabel := widget.NewRichTextFromMarkdown("Status: Idle")
	logArea := widget.NewMultiLineEntry()
	logArea.Disable()
	logArea.SetMinRowsVisible(10)

	// Camera Device Input (for ffmpeg dshow)
	cameraLabel := widget.NewLabel("Camera Device (optional):")
	cameraEntry := widget.NewSelect([]string{}, nil)
	cameraEntry.PlaceHolder = "Auto (first camera)"

	// Advanced toggle for showing device select
	advancedOpen := false
	advancedBtn := widget.NewButton("Show Advanced Camera Options", nil)
	advancedContainer := container.NewVBox(cameraLabel, cameraEntry)
	advancedContainer.Hide()
	advancedBtn.OnTapped = func() {
		advancedOpen = !advancedOpen
		if advancedOpen {
			advancedContainer.Show()
			advancedBtn.SetText("Hide Advanced Camera Options")
		} else {
			advancedContainer.Hide()
			advancedBtn.SetText("Show Advanced Camera Options")
		}
	}

	// Camera Preview
	previewImage := canvas.NewImageFromImage(nil)
	previewImage.FillMode = canvas.ImageFillContain
	previewImage.SetMinSize(fyne.NewSize(320, 240))
	previewImageFlip := false

	// Buttons that may need refresh on redraw glitches
	refreshButtons := []*widget.Button{}
	buttonDefaults := map[*widget.Button]string{}

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

			// Force redraw of buttons if they visually glitch
			for _, b := range refreshButtons {
				if b != nil {
					if b.Text == "" {
						if txt, ok := buttonDefaults[b]; ok {
							b.SetText(txt)
						}
					}
					b.Refresh()
				}
			}
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

		clinicName := clinicNameEntry.Text
		if clinicName == "" {
			log("Error: Please enter a Clinic Name")
			return
		}

		patientName := patientNameEntry.Text
		if patientName == "" {
			log("Error: Please enter a Patient Name")
			return
		}

		stopBtn.Enable()
		go runCLIAndSend(name, args, parser, targetURL, clinicName, patientName, log, func() {
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
	btnCamFlip := widget.NewButton("Flip Preview (Vertical)", func() {
		previewImageFlip = !previewImageFlip
		applyPreview := func() {
			if previewImage.Image != nil {
				previewImage.Refresh()
			}
		}
		if previewImage.Image == nil {
			log("Preview flip toggled; will apply when preview shows an image.")
		}
		applyPreview()
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
		device := strings.TrimSpace(cameraEntry.Selected)
		if device == "" {
			if autoDevice, err := detectDefaultCameraDevice(); err == nil && autoDevice != "" {
				device = autoDevice
				log(fmt.Sprintf("Using detected camera: %s", device))
			} else {
				log("Error: No camera device found. Set a device name (advanced options).")
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
					// Avoid re-entering render loop while UI might be mid-refresh.
					go func(img image.Image, flip bool) {
						fyne.Do(func() {
							if flip {
								b := img.Bounds()
								flipped := image.NewRGBA(b)
								h := b.Dy()
								for y := 0; y < h; y++ {
									for x := b.Min.X; x < b.Max.X; x++ {
										flipped.Set(x, b.Min.Y+(h-1)-(y-b.Min.Y), img.At(x, y+b.Min.Y))
									}
								}
								previewImage.Image = flipped
							} else {
								previewImage.Image = img
							}
							previewImage.Refresh()
						})
					}(img, previewImageFlip)
				}
			}
		}()
	}

	stopPreview := func() {
		stopPreviewInternal("Camera preview stopped")
	}

	btnPreviewStart := widget.NewButton("Start Preview", startPreview)
	btnPreviewStop := widget.NewButton("Stop Preview", stopPreview)

	// Streaming helpers
	resolveDevice := func(sel string) (string, error) {
		device := strings.TrimSpace(sel)
		if device == "" {
			if autoDevice, err := detectDefaultCameraDevice(); err == nil && autoDevice != "" {
				device = autoDevice
				log(fmt.Sprintf("Using detected camera: %s", device))
			} else {
				return "", fmt.Errorf("no camera device found")
			}
		}
		return device, nil
	}

	stopStreaming := func() {
		wsMu.Lock()
		if streamCancel != nil {
			streamCancel()
			streamCancel = nil
			log("Stream stopped")
		}
		wsMu.Unlock()
	}

	startStreaming := func() {
		wsMu.Lock()
		if wsConn == nil {
			wsMu.Unlock()
			log("Error: WS not connected")
			return
		}
		wsMu.Unlock()

		wsMu.Lock()
		if streamCancel != nil {
			wsMu.Unlock()
			log("Error: Stream already running")
			return
		}
		wsMu.Unlock()

		device, err := resolveDevice(cameraEntry.Selected)
		if err != nil {
			log(fmt.Sprintf("Error: %v", err))
			return
		}

		clinic := strings.TrimSpace(clinicNameEntry.Text)
		patient := strings.TrimSpace(patientNameEntry.Text)

		ctx, cancel := context.WithCancel(context.Background())
		wsMu.Lock()
		streamCancel = cancel
		wsMu.Unlock()

		log(fmt.Sprintf("Starting stream for %s", device))

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
					go func(img image.Image) {
						// send metadata first once per stream? We'll send periodically
						meta := map[string]string{
							"clinic_name":  clinic,
							"patient_name": patient,
						}
						metaJSON, _ := json.Marshal(meta)

						var buf bytes.Buffer
						if err := jpeg.Encode(&buf, img, nil); err != nil {
							log(fmt.Sprintf("Encode error: %v", err))
							return
						}
						wsMu.Lock()
						c := wsConn
						wsMu.Unlock()
						if c == nil {
							log("WS disconnected during stream")
							stopStreaming()
							return
						}
						// send meta
						_ = c.WriteMessage(websocket.TextMessage, metaJSON)
						if err := c.WriteMessage(websocket.BinaryMessage, buf.Bytes()); err != nil {
							log(fmt.Sprintf("WS send error: %v", err))
							stopStreaming()
							return
						}
					}(img)
				}
			}
		}()
	}

	// WebSocket to server for camera feed control
	wsURLLabel := widget.NewLabel("WebSocket URL (feed control):")
	wsURLEntry := widget.NewEntry()
	wsURLEntry.SetText("ws://localhost:8081/ws/feed")
	wsStatus := widget.NewLabel("WS: Disconnected")

	connectWS := func() {
		wsMu.Lock()
		if wsConn != nil {
			wsMu.Unlock()
			log("Error: WS already connected")
			return
		}
		wsMu.Unlock()

		u := strings.TrimSpace(wsURLEntry.Text)
		if u == "" {
			log("Error: Enter WS URL")
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		c, _, err := websocket.DefaultDialer.DialContext(ctx, u, nil)
		if err != nil {
			log(fmt.Sprintf("WS connect error: %v", err))
			cancel()
			return
		}

		wsMu.Lock()
		wsConn = c
		wsCancel = cancel
		wsMu.Unlock()
		wsStatus.SetText("WS: Connected")
		log("WS connected")

		go func() {
			defer func() {
				wsMu.Lock()
				if wsConn != nil {
					wsConn.Close()
				}
				wsConn = nil
				if wsCancel != nil {
					wsCancel()
				}
				wsCancel = nil
				wsMu.Unlock()
				fyne.Do(func() { wsStatus.SetText("WS: Disconnected") })
			}()

			for {
				_, msg, err := c.ReadMessage()
				if err != nil {
					log(fmt.Sprintf("WS read error: %v", err))
					return
				}
				cmd := strings.ToLower(strings.TrimSpace(string(msg)))
				switch cmd {
				case "start":
					log("WS command: start streaming")
					fyne.Do(func() { startStreaming() })
				case "stop":
					log("WS command: stop streaming")
					fyne.Do(func() { stopStreaming() })
				default:
					log(fmt.Sprintf("WS unknown command: %s", cmd))
				}
			}
		}()
	}

	disconnectWS := func() {
		wsMu.Lock()
		if wsConn != nil {
			wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
			wsConn.Close()
		}
		if wsCancel != nil {
			wsCancel()
		}
		wsConn = nil
		wsCancel = nil
		wsMu.Unlock()
		wsStatus.SetText("WS: Disconnected")
	}

	wsConnectBtn := widget.NewButton("Connect WS", connectWS)
	wsDisconnectBtn := widget.NewButton("Disconnect WS", disconnectWS)

	// Collect buttons for refresh
	refreshButtons = []*widget.Button{
		stopBtn,
		btnHeartRate, btnNIBP, btnGlucose, btnTemp,
		btnCamList, btnCamLeft, btnCamRight, btnCamUp, btnCamDown, btnCamFlip,
		btnPreviewStart, btnPreviewStop,
		wsConnectBtn, wsDisconnectBtn,
		advancedBtn,
	}
	for _, b := range refreshButtons {
		if b != nil {
			buttonDefaults[b] = b.Text
		}
	}
	// Layout
	mainContent := container.NewVBox(
		lightModeCheck,
		urlLabel,
		urlEntry,
		clinicNameLabel,
		clinicNameEntry,
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
		advancedBtn,
		advancedContainer,
		btnCamList,
		btnCamLeft,
		btnCamRight,
		btnCamUp,
		btnCamDown,
		btnCamFlip,
		container.NewHBox(btnPreviewStart, btnPreviewStop),
		previewImage,
		widget.NewSeparator(),
		widget.NewLabel("WebSocket Feed Control:"),
		wsURLLabel,
		wsURLEntry,
		wsStatus,
		container.NewHBox(wsConnectBtn, wsDisconnectBtn),
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

func runCLIAndSend(name string, args []string, parser LineParser, targetURL string, clinicName string, patientName string, log func(string), onFinish func()) {
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
				dataMap["clinic_name"] = clinicName
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
	args := buildFFmpegArgsForSnapshot(device)

	// Debug log the command being used (without context cancellation details)
	logCmd := strings.Join(append([]string{"ffmpeg"}, args...), " ")
	fmt.Printf("ffmpeg command: %s\n", logCmd)

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

func buildFFmpegArgsForSnapshot(device string) []string {
	// Select correct input driver per OS
	if runtime.GOOS == "windows" {
		// dshow expects video="Device Name"
		device = normalizeWindowsDeviceName(device)
		return []string{
			"-f", "dshow",
			"-i", device,
			"-vframes", "1",
			"-f", "mjpeg",
			"-",
		}
	}

	// macOS: avfoundation, device is usually "0" (video index) or "0:" (video:audio)
	// If user passed a named device, avfoundation still expects an index; auto-detect helps.
	if runtime.GOOS == "darwin" {
		return []string{
			"-f", "avfoundation",
			"-framerate", "30",
			"-video_size", "640x480",
			"-i", device, // device is an index like "0"
			"-vframes", "1",
			"-f", "mjpeg",
			"-",
		}
	}

	// Fallback: try v4l2 on linux
	return []string{
		"-f", "v4l2",
		"-i", device,
		"-vframes", "1",
		"-f", "mjpeg",
		"-",
	}
}

// detectDefaultCameraDevice tries to find the first video device via ffmpeg -list_devices.
func detectDefaultCameraDevice() (string, error) {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("ffmpeg", "-list_devices", "true", "-f", "dshow", "-i", "dummy")
		var stderr bytes.Buffer
		cmd.Stdout = &stderr // ffmpeg prints device list to stderr; stdout unused
		cmd.Stderr = &stderr
		_ = cmd.Run() // non-zero exit is expected
		lines := strings.Split(stderr.String(), "\n")
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
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

	if runtime.GOOS == "darwin" {
		cmd := exec.Command("ffmpeg", "-f", "avfoundation", "-list_devices", "true", "-i", "")
		var stderr bytes.Buffer
		cmd.Stdout = &stderr
		cmd.Stderr = &stderr
		_ = cmd.Run() // ffmpeg returns error exit; ignore
		lines := strings.Split(stderr.String(), "\n")
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			// Match lines like: [AVFoundation input device @ ...] [0] FaceTime HD Camera
			if strings.Contains(ln, "AVFoundation input device") && strings.Contains(ln, "] [") {
				parts := strings.Split(ln, "] [")
				if len(parts) >= 2 {
					// Extract index between '[' and ']' after split
					// e.g. "...] [0] FaceTime HD Camera"
					idxEnd := strings.Index(parts[1], "]")
					if idxEnd > 0 {
						idx := strings.TrimSpace(parts[1][:idxEnd])
						if idx != "" {
							return idx, nil // avfoundation expects numeric index like "0"
						}
					}
				}
			}
		}
		return "", fmt.Errorf("no video devices found")
	}

	return "", fmt.Errorf("auto-detect not supported on this OS")
}

// normalizeWindowsDeviceName ensures dshow format video="Name" without double-wrapping quotes.
func normalizeWindowsDeviceName(device string) string {
	d := strings.TrimSpace(device)
	if d == "" {
		return d
	}
	// Remove leading video= if present, and strip any quotes.
	if strings.HasPrefix(strings.ToLower(d), "video=") {
		d = d[len("video="):]
	}
	name := strings.Trim(d, `"`)
	// For exec.Command we do NOT need quotes; they are only for shell protection.
	return "video=" + name
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
