package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	xdraw "golang.org/x/image/draw"
)

// previewMaxSize is the fixed render size for the local preview image.
// Capping the rendered frame ensures the layout never expands when ffmpeg
// returns a larger native frame (e.g. 640x480 or 1920x1080).
const previewMaxW, previewMaxH = 320, 240

// blankFrame is a 1x1 fully transparent image used in place of nil so that
// canvas.Image always has content. A visible canvas.Image with nil content
// thrashes the GL texture cache (see fyne issue #4345), evicting cached
// button text glyphs and making every button in the window look blank.
var blankFrame = func() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	return img
}()

// fitToPreview returns a copy of src scaled to fit within previewMaxW x previewMaxH,
// preserving aspect ratio.
func fitToPreview(src image.Image) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return image.NewRGBA(image.Rect(0, 0, previewMaxW, previewMaxH))
	}
	dw, dh := previewMaxW, previewMaxH
	srcRatio := float64(sw) / float64(sh)
	dstRatio := float64(dw) / float64(dh)
	if srcRatio > dstRatio {
		dh = int(float64(dw) / srcRatio)
	} else {
		dw = int(float64(dh) * srcRatio)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}

// LineParser function signature
type LineParser func(line string) (interface{}, error)

// AppConfig holds all persisted settings.
type AppConfig struct {
	ServerBase  string `json:"server_base"`
	ClinicName  string `json:"clinic_name"`
	PatientName string `json:"patient_name"`
	StethMAC    string `json:"steth_mac"`
	LightMode   bool   `json:"light_mode"`
}

func configFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".medicart_config.json"
	}
	return filepath.Join(home, ".medicart", "config.json")
}

func loadAppConfig() AppConfig {
	cfg := AppConfig{ServerBase: "http://localhost:8081"}
	data, err := os.ReadFile(configFilePath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	if cfg.ServerBase == "" {
		cfg.ServerBase = "http://localhost:8081"
	}
	return cfg
}

func saveAppConfig(cfg AppConfig) error {
	p := configFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}

// ingestURL derives the HTTP ingest endpoint from a base URL.
func ingestURL(base string) string {
	return strings.TrimRight(base, "/") + "/api/ingest"
}

// feedWSURL derives the WebSocket feed endpoint from a base URL.
func feedWSURL(base string) string {
	u := strings.TrimRight(base, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	if !strings.HasPrefix(u, "ws") {
		u = "ws://" + u
	}
	return u + "/ws/feed"
}

var (
	currentCmd    *exec.Cmd
	cmdMutex      sync.Mutex
	cancelFunc    context.CancelFunc
	previewMu     sync.Mutex
	previewCancel context.CancelFunc
	wsConn        *websocket.Conn
	wsMu          sync.Mutex
	wsCancel      context.CancelFunc
	streamCancel  context.CancelFunc

	// Audio mic WS (separate connection to /ws/audio-feed)
	micWsConn       *websocket.Conn
	micWsMu         sync.Mutex
	micWsCancel     context.CancelFunc
	micStreamCancel context.CancelFunc
)

func main() {
	myApp := app.New()
	myWindow := myApp.NewWindow("Medicart Uploader")

	var connectWS func() error
	var btnPreviewStart, btnPreviewStop *widget.Button
	var btnBroadcastStart, btnBroadcastStop *widget.Button

	// Load persisted settings from ~/.medicart/config.json
	cfg := loadAppConfig()

	// Theme Toggle — applies immediately, persisted on Save
	lightModeCheck := widget.NewCheck("Light Mode", func(checked bool) {
		if checked {
			myApp.Settings().SetTheme(theme.LightTheme())
		} else {
			myApp.Settings().SetTheme(theme.DarkTheme())
		}
	})
	lightModeCheck.Checked = cfg.LightMode
	if lightModeCheck.Checked {
		myApp.Settings().SetTheme(theme.LightTheme())
	}

	// Server Base URL (e.g. http://localhost:8081) — /api/ingest and /ws/feed are derived
	serverBaseLabel := widget.NewLabel("Server Base URL:")
	serverBaseEntry := widget.NewEntry()
	serverBaseEntry.SetPlaceHolder("http://your-server.com")
	serverBaseEntry.SetText(cfg.ServerBase)

	// Patient Name Input
	patientNameLabel := widget.NewLabel("Patient Name:")
	patientNameEntry := widget.NewEntry()
	patientNameEntry.SetPlaceHolder("Enter patient name")
	patientNameEntry.SetText(cfg.PatientName)

	// Clinic Name Input
	clinicNameLabel := widget.NewLabel("Clinic Name:")
	clinicNameEntry := widget.NewEntry()
	clinicNameEntry.SetPlaceHolder("Enter clinic name")
	clinicNameEntry.SetText(cfg.ClinicName)

	// Status Area
	statusLabel := widget.NewRichTextFromMarkdown("Status: Idle")
	logArea := widget.NewMultiLineEntry()
	logArea.Disable()
	logArea.SetMinRowsVisible(10)

	// Camera-specific log shown inside the Comms tab so camera errors
	// don't pollute the readings Live Console.
	cameraLogLabel := widget.NewLabel("")
	cameraLogLabel.Wrapping = fyne.TextWrapWord

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
	previewImage := canvas.NewImageFromImage(blankFrame)
	previewImage.FillMode = canvas.ImageFillContain
	previewImage.SetMinSize(fyne.NewSize(previewMaxW, previewMaxH))
	previewImageFlip := false

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

	cameraLog := func(msg string) {
		fyne.Do(func() {
			timestamp := time.Now().Format("15:04:05")
			cameraLogLabel.SetText(fmt.Sprintf("[%s] %s", timestamp, msg))
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

		targetURL := ingestURL(strings.TrimSpace(serverBaseEntry.Text))
		if strings.TrimSpace(serverBaseEntry.Text) == "" {
			log("Error: Please enter a Server Base URL in Settings")
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
			fyne.Do(func() { stopBtn.Disable() })
		})
	}

	stopBtn = widget.NewButtonWithIcon("Emergency Stop", theme.MediaStopIcon(), func() {
		cmdMutex.Lock()
		defer cmdMutex.Unlock()
		if cancelFunc != nil {
			cancelFunc()
			log("Stopping process...")
		}
	})
	stopBtn.Importance = widget.DangerImportance
	stopBtn.Disable()

	btnHeartRate := widget.NewButtonWithIcon("Heart Rate / SpO2", theme.MediaPlayIcon(), func() {
		startProcess("HeartRate", []string{"-heartrate"}, parseHeartRateLine)
	})

	btnNIBP := widget.NewButtonWithIcon("NIBP", theme.MediaPlayIcon(), func() {
		startProcess("NIBP", []string{"-nibp"}, parseNIBPLine)
	})

	btnGlucose := widget.NewButtonWithIcon("Glucose", theme.MediaPlayIcon(), func() {
		startProcess("Glucose", []string{"-glu"}, parseGlucoseLine)
	})

	btnTemp := widget.NewButtonWithIcon("Temperature", theme.MediaPlayIcon(), func() {
		startProcess("Temperature", []string{"-temperature"}, parseTemperatureLine)
	})

	// Stethoscope Buttons
	var btnStethoscopeList *widget.Button
	var stethMacEntry *widget.Entry

	btnStethoscopeList = widget.NewButtonWithIcon("List Stethoscopes", theme.ListIcon(), func() {
		startProcess("StethoscopeList", []string{"-list"}, parseStethoscopeLine)
	})

	stethMacEntry = widget.NewEntry()
	stethMacEntry.SetPlaceHolder("AA:BB:CC:DD:EE:FF")
	stethMacEntry.SetText(cfg.StethMAC)

	btnStethoscopeConnect := widget.NewButtonWithIcon("Connect", theme.MediaPlayIcon(), func() {
		mac := strings.TrimSpace(stethMacEntry.Text)
		if mac == "" {
			// If no MAC is entered, try to auto-detect if there's exactly one device

			cmdPath := "MinttiCLI.exe"
			if _, err := exec.LookPath(cmdPath); err != nil {
				cmdPath = "./MinttiCLI.exe"
			}

			// Run a quick scan to see if we can find exactly one device
			go func() {
				cmd := exec.Command(cmdPath, "-list")
				output, err := cmd.CombinedOutput()
				if err != nil {
					log(fmt.Sprintf("Auto-scan failed: %v", err))
					return
				}

				lines := strings.Split(string(output), "\n")
				var foundMacs []string
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if strings.Contains(line, "DATA:ITEM") {
						// Simple extraction of mac="..."
						if start := strings.Index(line, "mac=\""); start != -1 {
							rest := line[start+len("mac=\""):]
							if end := strings.Index(rest, "\""); end != -1 {
								foundMacs = append(foundMacs, rest[:end])
							}
						}
					}
				}

				if len(foundMacs) == 1 {
					autoMac := foundMacs[0]
					fyne.Do(func() {
						stethMacEntry.SetText(autoMac)
						log(fmt.Sprintf("Auto-detected single stethoscope: %s", autoMac))
						// Start the process now that we have the MAC
						startProcess("StethoscopeStream", []string{"-connect", "-mac", autoMac}, parseStethoscopeLine)
					})
				} else if len(foundMacs) > 1 {
					log(fmt.Sprintf("Found %d devices. Please enter a MAC address manually.", len(foundMacs)))
				} else {
					log("No stethoscopes found. Ensure device is on and in range.")
				}
			}()
			return
		}
		startProcess("StethoscopeStream", []string{"-connect", "-mac", mac}, parseStethoscopeLine)
	})

	runCameraCommand := func(action string, args []string) {
		go func() {
			cameraLog(fmt.Sprintf("Camera: %s ...", action))

			cmdPath := "camera_cli.exe"
			if _, err := exec.LookPath(cmdPath); err != nil {
				cmdPath = "./camera_cli.exe"
			}

			cmd := exec.Command(cmdPath, args...)
			outputBytes, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(outputBytes))

			if output != "" {
				cameraLog(fmt.Sprintf("Camera output: %s", output))
			}
			if err != nil {
				cameraLog(fmt.Sprintf("Error running camera %s: %v", action, err))
				return
			}

			upper := strings.ToUpper(output)
			if strings.HasPrefix(upper, "DATA:ERROR") {
				cameraLog(fmt.Sprintf("Camera %s reported error: %s", action, output))
				return
			}

			cameraLog(fmt.Sprintf("Camera %s completed", action))
		}()
	}

	btnCamList := widget.NewButtonWithIcon("Scan", theme.SearchIcon(), func() {
		runCameraCommand("list", []string{"-list"})
	})
	btnCamLeft := widget.NewButtonWithIcon("", theme.NavigateBackIcon(), func() {
		runCameraCommand("move-left", []string{"-move-left"})
	})
	btnCamRight := widget.NewButtonWithIcon("", theme.NavigateNextIcon(), func() {
		runCameraCommand("move-right", []string{"-move-right"})
	})
	btnCamUp := widget.NewButtonWithIcon("", theme.MoveUpIcon(), func() {
		runCameraCommand("move-up", []string{"-move-up"})
	})
	btnCamDown := widget.NewButtonWithIcon("", theme.MoveDownIcon(), func() {
		runCameraCommand("move-down", []string{"-move-down"})
	})
	btnCamFlip := widget.NewButtonWithIcon("Flip", theme.ViewRefreshIcon(), func() {
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
		if previewCancel == nil {
			previewMu.Unlock()
			return
		}
		previewCancel()
		previewCancel = nil
		previewMu.Unlock()
		// Release lock before all UI work to avoid nesting fyne.Do inside a mutex hold.
		if logMsg != "" {
			log(logMsg)
		}
		fyne.Do(func() {
			// Use a placeholder rather than nil — see fyne issue #4345.
			previewImage.Image = blankFrame
			previewImage.Refresh()
			btnPreviewStart.Enable()
			btnPreviewStop.Disable()
		})
	}

	startPreview := func() {
		device := strings.TrimSpace(cameraEntry.Selected)
		if device == "" {
			if autoDevice, err := detectDefaultCameraDevice(); err == nil && autoDevice != "" {
				device = autoDevice
				cameraLog(fmt.Sprintf("Using detected camera: %s", device))
			} else {
				cameraLog("Error: No camera device found. Set a device name (advanced options).")
				return
			}
		}

		previewMu.Lock()
		if previewCancel != nil {
			previewMu.Unlock()
			cameraLog("Preview already running")
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		previewCancel = cancel
		previewMu.Unlock()

		cameraLog(fmt.Sprintf("Starting camera preview for %s", device))
		fyne.Do(func() {
			btnPreviewStart.Disable()
			btnPreviewStop.Enable()
		})

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
						// ffmpeg returns "signal: killed" when we cancel ctx; that's expected.
						if ctx.Err() != nil {
							return
						}
						cameraLog(fmt.Sprintf("Capture error: %v", err))
						continue
					}
					// Always downscale to a fixed preview size so the canvas
					// never asks the layout for more space than we want.
					scaled := fitToPreview(img)
					var rendered image.Image = scaled
					if previewImageFlip {
						b := scaled.Bounds()
						flipped := image.NewRGBA(b)
						h := b.Dy()
						for y := 0; y < h; y++ {
							for x := b.Min.X; x < b.Max.X; x++ {
								flipped.Set(x, b.Min.Y+(h-1)-(y-b.Min.Y), scaled.At(x, y+b.Min.Y))
							}
						}
						rendered = flipped
					}
					fyne.Do(func() {
						previewImage.Image = rendered
						previewImage.Refresh()
					})
				}
			}
		}()
	}

	stopPreview := func() {
		stopPreviewInternal("")
		cameraLog("Camera preview stopped")
	}

	// Streaming helpers
	resolveDevice := func(sel string) (string, error) {
		device := strings.TrimSpace(sel)
		if device == "" {
			if autoDevice, err := detectDefaultCameraDevice(); err == nil && autoDevice != "" {
				device = autoDevice
				cameraLog(fmt.Sprintf("Using detected camera: %s", device))
			} else {
				return "", fmt.Errorf("no camera device found")
			}
		}
		return device, nil
	}

	stopStreaming := func() {
		wsMu.Lock()
		if streamCancel == nil {
			wsMu.Unlock()
			return
		}
		streamCancel()
		streamCancel = nil
		wsMu.Unlock()
		cameraLog("Stream stopped")
		fyne.Do(func() {
			btnBroadcastStart.Enable()
			btnBroadcastStop.Disable()
		})
	}

	startStreaming := func() {
		wsMu.Lock()
		if wsConn == nil {
			wsMu.Unlock()
			log("Connecting to server...")
			if err := connectWS(); err != nil {
				log(fmt.Sprintf("Auto-connect failed: %v", err))
				return
			}
			// Wait a brief moment for connection to stabilize if needed
			time.Sleep(100 * time.Millisecond)
		} else {
			wsMu.Unlock()
		}

		wsMu.Lock()
		if streamCancel != nil {
			wsMu.Unlock()
			cameraLog("Stream already running")
			return
		}
		wsMu.Unlock()

		device, err := resolveDevice(cameraEntry.Selected)
		if err != nil {
			cameraLog(fmt.Sprintf("Error: %v", err))
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		wsMu.Lock()
		streamCancel = cancel
		wsMu.Unlock()

		cameraLog(fmt.Sprintf("Starting stream for %s", device))
		fyne.Do(func() {
			btnBroadcastStart.Disable()
			btnBroadcastStop.Enable()
		})

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
						if ctx.Err() != nil {
							return
						}
						cameraLog(fmt.Sprintf("Stream capture error: %v", err))
						continue
					}
					go func(img image.Image) {
						// Always use the latest clinic name from the entry
						currentClinic := strings.TrimSpace(clinicNameEntry.Text)
						meta := map[string]string{
							"clinic_name": currentClinic,
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
							cameraLog("WS disconnected during stream")
							stopStreaming()
							return
						}
						_ = c.WriteMessage(websocket.TextMessage, metaJSON)
						if err := c.WriteMessage(websocket.BinaryMessage, buf.Bytes()); err != nil {
							cameraLog(fmt.Sprintf("WS send error: %v", err))
							stopStreaming()
							return
						}
					}(img)
				}
			}
		}()
	}

	// --- Microphone streaming (audio) ---

	var btnMicStart, btnMicStop *widget.Button
	micLog := func(msg string) {
		log(fmt.Sprintf("[MIC] %s", msg))
	}

	var connectMicWS func() error
	var startMicStreaming func()
	var stopMicStreaming func()

	stopMicStreaming = func() {
		micWsMu.Lock()
		if micStreamCancel == nil {
			micWsMu.Unlock()
			return
		}
		micStreamCancel()
		micStreamCancel = nil
		micWsMu.Unlock()
		micLog("Mic stream stopped")
		fyne.Do(func() {
			btnMicStart.Enable()
			btnMicStop.Disable()
		})
	}

	startMicStreaming = func() {
		micWsMu.Lock()
		if micWsConn == nil {
			micWsMu.Unlock()
			micLog("Connecting mic WS…")
			if err := connectMicWS(); err != nil {
				micLog(fmt.Sprintf("Mic WS connect failed: %v", err))
				return
			}
			time.Sleep(100 * time.Millisecond)
		} else {
			micWsMu.Unlock()
		}

		micWsMu.Lock()
		if micStreamCancel != nil {
			micWsMu.Unlock()
			micLog("Mic already streaming")
			return
		}
		micWsMu.Unlock()

		device, err := detectDefaultMicDevice()
		if err != nil {
			micLog(fmt.Sprintf("Mic detect error: %v", err))
			return
		}
		micLog(fmt.Sprintf("Detected mic: %s", device))

		ffArgs := buildFFmpegArgsForAudio(device)
		micLog(fmt.Sprintf("ffmpeg %s", strings.Join(ffArgs, " ")))

		ctx, cancel := context.WithCancel(context.Background())
		micWsMu.Lock()
		micStreamCancel = cancel
		micWsMu.Unlock()

		fyne.Do(func() {
			btnMicStart.Disable()
			btnMicStop.Enable()
		})

		go func() {
			defer stopMicStreaming()

			// Register clinic on the audio feed WS
			currentClinic := strings.TrimSpace(clinicNameEntry.Text)
			meta, _ := json.Marshal(map[string]string{"clinic_name": currentClinic})
			micWsMu.Lock()
			c := micWsConn
			micWsMu.Unlock()
			if c == nil {
				micLog("Mic WS not connected")
				return
			}
			_ = c.WriteMessage(websocket.TextMessage, meta)

			cmd := exec.CommandContext(ctx, "ffmpeg", ffArgs...)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				micLog(fmt.Sprintf("ffmpeg pipe error: %v", err))
				return
			}
			if err := cmd.Start(); err != nil {
				micLog(fmt.Sprintf("ffmpeg start error: %v", err))
				return
			}
			defer cmd.Wait()

			// Read 100 ms chunks: 16000 Hz × 2 bytes × 1 ch × 0.1 s = 3200 bytes
			const chunkSize = 3200
			buf := make([]byte, chunkSize)
			for {
				if ctx.Err() != nil {
					return
				}
				_, err := io.ReadFull(stdout, buf)
				if err != nil {
					if ctx.Err() == nil {
						micLog(fmt.Sprintf("ffmpeg read error: %v", err))
					}
					return
				}
				chunk := make([]byte, chunkSize)
				copy(chunk, buf)
				micWsMu.Lock()
				c := micWsConn
				micWsMu.Unlock()
				if c == nil {
					return
				}
				if err := c.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
					micLog(fmt.Sprintf("WS send error: %v", err))
					return
				}
			}
		}()
	}

	connectMicWS = func() error {
		micWsMu.Lock()
		if micWsConn != nil {
			micWsMu.Unlock()
			return nil
		}
		micWsMu.Unlock()

		base := strings.TrimSpace(serverBaseEntry.Text)
		if base == "" {
			return fmt.Errorf("missing base URL")
		}
		u := audioFeedWSURL(base)

		ctx, cancel := context.WithCancel(context.Background())
		c, _, err := websocket.DefaultDialer.DialContext(ctx, u, nil)
		if err != nil {
			cancel()
			return err
		}

		micWsMu.Lock()
		micWsConn = c
		micWsCancel = cancel
		micWsMu.Unlock()
		micLog("Mic WS connected to " + u)

		// Register clinic immediately
		clinic := strings.TrimSpace(clinicNameEntry.Text)
		if clinic != "" {
			meta, _ := json.Marshal(map[string]string{"clinic_name": clinic})
			_ = c.WriteMessage(websocket.TextMessage, meta)
		}

		go func() {
			defer func() {
				micWsMu.Lock()
				if micWsConn != nil {
					micWsConn.Close()
				}
				micWsConn = nil
				if micWsCancel != nil {
					micWsCancel()
				}
				micWsCancel = nil
				micWsMu.Unlock()
				micLog("Mic WS disconnected")
			}()

			// Incoming binary = doctor mic audio → play via ffplay
			var ffplayCmd *exec.Cmd
			var ffplayStdin io.WriteCloser
			startFFPlay := func() {
				if ffplayCmd != nil {
					return
				}
				cmd := exec.Command("ffplay",
					"-f", "s16le", "-ar", "16000", "-ac", "1",
					"-nodisp", "-autoexit", "-loglevel", "quiet", "-",
				)
				var err error
				ffplayStdin, err = cmd.StdinPipe()
				if err != nil {
					micLog(fmt.Sprintf("ffplay stdin error: %v", err))
					return
				}
				if err := cmd.Start(); err != nil {
					micLog(fmt.Sprintf("ffplay not found — install ffplay for audio playback: %v", err))
					ffplayStdin = nil
					return
				}
				ffplayCmd = cmd
				micLog("Audio playback started (ffplay)")
			}

			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					break
				}
				if mt == websocket.BinaryMessage && len(msg) > 0 {
					if ffplayStdin == nil {
						startFFPlay()
					}
					if ffplayStdin != nil {
						_, _ = ffplayStdin.Write(msg)
					}
				}
			}

			if ffplayCmd != nil {
				if ffplayStdin != nil {
					ffplayStdin.Close()
				}
				_ = ffplayCmd.Wait()
			}
		}()

		return nil
	}

	btnMicStart = widget.NewButtonWithIcon("Unmute Mic", theme.MediaRecordIcon(), func() {
		go startMicStreaming()
	})
	btnMicStop = widget.NewButtonWithIcon("Mute Mic", theme.MediaStopIcon(), func() {
		go stopMicStreaming()
	})
	btnMicStop.Disable()

	btnPreviewStart = widget.NewButtonWithIcon("Start Preview", theme.VisibilityIcon(), startPreview)
	btnPreviewStop = widget.NewButtonWithIcon("Stop Preview", theme.VisibilityOffIcon(), stopPreview)

	startBroadcast := func() {
		startStreaming()
	}
	stopBroadcast := func() {
		stopStreaming()
	}

	btnBroadcastStart = widget.NewButtonWithIcon("Go Live", theme.MediaPlayIcon(), startBroadcast)
	btnBroadcastStop = widget.NewButtonWithIcon("End Stream", theme.MediaStopIcon(), stopBroadcast)
	btnBroadcastStop.Disable()
	btnPreviewStop.Disable()

	// WebSocket to server for camera feed control
	wsStatus := widget.NewLabel("WS: Disconnected")

	// Declare connectWS early so it can be used by startStreaming
	// var connectWS func() error // Moved to top of main

	connectWS = func() error {
		wsMu.Lock()
		if wsConn != nil {
			wsMu.Unlock()
			log("Error: WS already connected")
			return nil
		}
		wsMu.Unlock()

		base := strings.TrimSpace(serverBaseEntry.Text)
		if base == "" {
			log("Error: Enter a Server Base URL in Settings")
			return fmt.Errorf("missing base URL")
		}
		u := feedWSURL(base)

		ctx, cancel := context.WithCancel(context.Background())
		c, _, err := websocket.DefaultDialer.DialContext(ctx, u, nil)
		if err != nil {
			log(fmt.Sprintf("WS connect error: %v", err))
			cancel()
			return err
		}

		wsMu.Lock()
		wsConn = c
		wsCancel = cancel
		wsMu.Unlock()
		fyne.Do(func() {
			wsStatus.SetText("WS: Connected")
		})
		log("WS connected")

		// Auto-connect mic WS so audio channel is ready as soon as camera connects
		go func() {
			if err := connectMicWS(); err != nil {
				log(fmt.Sprintf("Mic WS auto-connect failed (non-fatal): %v", err))
			}
		}()

		// Send identity immediately so server registers this clinic connection
		clinic := strings.TrimSpace(clinicNameEntry.Text)
		if clinic != "" {
			meta := map[string]string{"clinic_name": clinic}
			mj, _ := json.Marshal(meta)
			c.WriteMessage(websocket.TextMessage, mj)
		}

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
				cameraLog("WS command: start streaming")
				go startStreaming()
			case "stop":
				cameraLog("WS command: stop streaming")
				go stopStreaming()
			case "mic-on":
				cameraLog("WS command: mic on")
				go startMicStreaming()
			case "mic-off":
				cameraLog("WS command: mic off")
				go stopMicStreaming()
			case "move-left", "move-right", "move-up", "move-down":
				cameraLog(fmt.Sprintf("WS camera command: %s", cmd))
				runCameraCommand(cmd, []string{"-" + cmd})
			case "flip":
				fyne.Do(func() {
					previewImageFlip = !previewImageFlip
					cameraLog("WS camera command: flip preview")
					if previewImage.Image != nil {
						previewImage.Refresh()
					}
				})
			default:
				cameraLog(fmt.Sprintf("WS unknown command: %s", cmd))
			}
			}
		}()

		return nil
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

	wsConnectBtn := widget.NewButtonWithIcon("Connect", theme.SettingsIcon(), func() { connectWS() })
	wsDisconnectBtn := widget.NewButtonWithIcon("Disconnect", theme.CancelIcon(), disconnectWS)

	btnSaveSettings := widget.NewButtonWithIcon("Save Settings", theme.DocumentSaveIcon(), func() {
		newCfg := AppConfig{
			ServerBase:  strings.TrimSpace(serverBaseEntry.Text),
			ClinicName:  strings.TrimSpace(clinicNameEntry.Text),
			PatientName: strings.TrimSpace(patientNameEntry.Text),
			StethMAC:    strings.TrimSpace(stethMacEntry.Text),
			LightMode:   lightModeCheck.Checked,
		}
		if err := saveAppConfig(newCfg); err != nil {
			log(fmt.Sprintf("Error saving settings: %v", err))
		} else {
			log(fmt.Sprintf("Settings saved (server: %s)", newCfg.ServerBase))
		}
	})
	btnSaveSettings.Importance = widget.HighImportance


	// --- Layout Refactor ---

	// 1. Patients Tab
	// Mimicking the Patient Info & Overview section
	ageEntry := widget.NewEntry()
	ageEntry.SetText("62")
	weightEntry := widget.NewEntry()
	weightEntry.SetText("85")
	heightEntry := widget.NewEntry()
	heightEntry.SetText("180")

	patientOverview := container.NewGridWithColumns(3,
		container.NewVBox(widget.NewLabelWithStyle("AGE (YRS)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), ageEntry),
		container.NewVBox(widget.NewLabelWithStyle("WEIGHT (KG)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), weightEntry),
		container.NewVBox(widget.NewLabelWithStyle("HEIGHT (CM)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), heightEntry),
	)

	patientsContent := container.NewVBox(
		widget.NewCard("Identity Context", "Select Patient", container.NewVBox(
			patientNameLabel, patientNameEntry,
		)),
		widget.NewCard("Patient Overview", "", patientOverview),
	)

	// 2. Readings Tab
	// Mimicking the Vitals & Stethoscope section + Live Console
	readingsContent := container.NewVBox(
		widget.NewLabelWithStyle("Patient Readings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Select a vital sign to begin live monitoring."),
		container.NewGridWithColumns(2,
			btnHeartRate,
			btnNIBP,
			btnGlucose,
			btnTemp,
		),
		widget.NewSeparator(),
		widget.NewCard("Connect Stethoscope", "Stream high-fidelity auscultation", container.NewVBox(
			btnStethoscopeList,
			widget.NewLabel("MAC Address:"),
			stethMacEntry,
			btnStethoscopeConnect,
		)),
		widget.NewSeparator(),
		widget.NewCard("Live Console", "System Active", container.NewVBox(
			statusLabel,
			container.NewStack(logArea),
			stopBtn,
		)),
	)

	// 3. Comms Tab
	// Mimicking the Remote Comm & Camera section
	commsContent := container.NewVBox(
		widget.NewCard("Live Feed", "Local Preview", container.NewVBox(
			container.NewGridWithColumns(2, btnPreviewStart, btnPreviewStop),
			previewImage,
		)),
		widget.NewCard("Server Streaming", "Broadcast to Clinic Dashboard", container.NewVBox(
			container.NewGridWithColumns(2, btnBroadcastStart, btnBroadcastStop),
			advancedBtn,
			advancedContainer,
		)),
		widget.NewCard("Microphone", "Auto-connects when camera connects — mute/unmute to control", container.NewVBox(
			container.NewGridWithColumns(2, btnMicStart, btnMicStop),
		)),
		widget.NewCard("Camera Status", "", cameraLogLabel),
		widget.NewCard("Camera Control", "Precision Pan-Tilt-Zoom", container.NewVBox(
			btnCamList,
			container.NewCenter(
				container.NewGridWithColumns(3,
					widget.NewLabel(""), btnCamUp, widget.NewLabel(""),
					btnCamLeft, widget.NewButtonWithIcon("Reset", theme.ViewRefreshIcon(), func() {
						runCameraCommand("reset", []string{"-reset"}) // Assuming reset exists
					}), btnCamRight,
					widget.NewLabel(""), btnCamDown, widget.NewLabel(""),
				),
			),
			btnCamFlip,
		)),
	)

	// 4. Settings Tab
	settingsContent := container.NewVBox(
		widget.NewCard("Configuration", "Saved to ~/.medicart/config.json", container.NewVBox(
			widget.NewLabelWithStyle("Clinic Identity", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			clinicNameLabel, clinicNameEntry,
			widget.NewSeparator(),
			widget.NewLabelWithStyle("Server", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			serverBaseLabel, serverBaseEntry,
			widget.NewLabelWithStyle("  → ingest: {base}/api/ingest\n  → feed:   {base}/ws/feed", fyne.TextAlignLeading, fyne.TextStyle{Italic: true}),
			widget.NewSeparator(),
			btnSaveSettings,
		)),
		widget.NewCard("Camera Feed Connection", "", container.NewVBox(
			wsStatus,
			container.NewGridWithColumns(2, wsConnectBtn, wsDisconnectBtn),
		)),
		widget.NewCard("Display", "", lightModeCheck),
	)

	// Main Tab Container
	tabs := container.NewAppTabs(
		container.NewTabItemWithIcon("Patients", theme.AccountIcon(), container.NewVScroll(patientsContent)),
		container.NewTabItemWithIcon("Readings", theme.InfoIcon(), container.NewVScroll(readingsContent)),
		container.NewTabItemWithIcon("Comms", theme.VisibilityIcon(), container.NewVScroll(commsContent)),
		container.NewTabItemWithIcon("Settings", theme.SettingsIcon(), container.NewVScroll(settingsContent)),
	)
	tabs.SetTabLocation(container.TabLocationBottom) // Move tabs to bottom to mimic mobile/app feel from the designs

	myWindow.SetContent(tabs)
	myWindow.Resize(fyne.NewSize(480, 800))

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
	if name == "StethoscopeList" || name == "StethoscopeStream" {
		cmdPath = "MinttiCLI.exe"
	}

	if _, err := exec.LookPath(cmdPath); err != nil {
		if name == "StethoscopeList" || name == "StethoscopeStream" {
			cmdPath = "./MinttiCLI.exe"
		} else {
			cmdPath = "./lepu_cli.exe"
		}
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
			// Older ffmpeg: [AVFoundation input device @ ...] [0] Camera
			// Newer ffmpeg: [AVFoundation indev @ ...] [0] Camera
			if strings.Contains(ln, "AVFoundation") && strings.Contains(ln, "] [") {
				parts := strings.Split(ln, "] [")
				if len(parts) >= 2 {
					idxEnd := strings.Index(parts[1], "]")
					if idxEnd > 0 {
						idx := strings.TrimSpace(parts[1][:idxEnd])
						deviceName := strings.TrimSpace(parts[1][idxEnd+1:])

						// Skip audio devices if they appear here, and skip screen capture
						if idx != "" && !strings.Contains(strings.ToLower(deviceName), "capture screen") && !strings.Contains(strings.ToLower(ln), "audio devices") {
							return idx, nil
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
// audioFeedWSURL derives the WebSocket audio-feed endpoint from a base URL.
func audioFeedWSURL(base string) string {
	u := strings.TrimRight(base, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	if !strings.HasPrefix(u, "ws") {
		u = "ws://" + u
	}
	return u + "/ws/audio-feed"
}

// detectDefaultMicDevice finds the first available audio input device.
func detectDefaultMicDevice() (string, error) {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("ffmpeg", "-list_devices", "true", "-f", "dshow", "-i", "dummy")
		var stderr bytes.Buffer
		cmd.Stdout = &stderr
		cmd.Stderr = &stderr
		_ = cmd.Run()
		lines := strings.Split(stderr.String(), "\n")
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if strings.Contains(ln, "(audio)") && strings.Count(ln, "\"") >= 2 {
				start := strings.Index(ln, "\"")
				end := strings.LastIndex(ln, "\"")
				if start >= 0 && end > start {
					name := ln[start+1 : end]
					if name != "" {
						return name, nil
					}
				}
			}
		}
		return "", fmt.Errorf("no audio devices found")
	}

	if runtime.GOOS == "darwin" {
		cmd := exec.Command("ffmpeg", "-f", "avfoundation", "-list_devices", "true", "-i", "")
		var stderr bytes.Buffer
		cmd.Stdout = &stderr
		cmd.Stderr = &stderr
		_ = cmd.Run()
		lines := strings.Split(stderr.String(), "\n")
		inAudioSection := false
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			// Both old (input device) and new (indev) ffmpeg output contain "AVFoundation"
			if strings.Contains(ln, "AVFoundation") && strings.Contains(strings.ToLower(ln), "audio devices") {
				inAudioSection = true
				continue
			}
			if inAudioSection && strings.Contains(ln, "AVFoundation") && strings.Contains(ln, "] [") {
				parts := strings.Split(ln, "] [")
				if len(parts) >= 2 {
					idxEnd := strings.Index(parts[1], "]")
					if idxEnd > 0 {
						idx := strings.TrimSpace(parts[1][:idxEnd])
						if idx != "" {
							return idx, nil
						}
					}
				}
			}
		}
		return "", fmt.Errorf("no audio devices found on macOS")
	}

	// Linux: default alsa/pulse device
	return "default", nil
}

// buildFFmpegArgsForAudio returns ffmpeg args to stream mic audio as raw s16le PCM to stdout.
// Output is signed 16-bit little-endian, 16 kHz, mono — ready to be chunked over WebSocket.
func buildFFmpegArgsForAudio(device string) []string {
	out := []string{"-f", "s16le", "-ar", "16000", "-ac", "1", "-"}
	switch runtime.GOOS {
	case "windows":
		return append([]string{"-f", "dshow", "-i", `audio="` + device + `"`}, out...)
	case "darwin":
		// "none:<audio_idx>" captures audio only; accepted by both old and new ffmpeg builds.
		return append([]string{"-f", "avfoundation", "-i", "none:" + device}, out...)
	default:
		return append([]string{"-f", "alsa", "-i", device}, out...)
	}
}

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

// Stethoscope
func parseStethoscopeLine(line string) (interface{}, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "DATA:") {
		return nil, nil
	}

	parts := strings.TrimPrefix(line, "DATA:")
	if strings.HasPrefix(parts, "OK") {
		return map[string]interface{}{
			"type": "status",
			"msg":  parts,
		}, nil
	}
	if strings.HasPrefix(parts, "ERROR") {
		return map[string]interface{}{
			"type": "error",
			"msg":  parts,
		}, nil
	}
	if strings.HasPrefix(parts, "STATUS") {
		return map[string]interface{}{
			"type": "status",
			"msg":  parts,
		}, nil
	}
	if strings.HasPrefix(parts, "LIST") || strings.HasPrefix(parts, "ITEM") {
		return map[string]interface{}{
			"type": "discovery",
			"msg":  parts,
		}, nil
	}
	if strings.HasPrefix(parts, "STREAM") {
		// DATA:STREAM type=audio data=[...]
		// DATA:STREAM type=heartrate value=N
		streamParts := parseKVSpace(parts)
		res := map[string]interface{}{
			"type": "stream",
		}
		for k, v := range streamParts {
			if k == "type" {
				res["stream_type"] = v
			} else if k == "data" {
				var audioData []int16
				if err := json.Unmarshal([]byte(v), &audioData); err == nil {
					res["data"] = audioData
				} else {
					res["data"] = v
				}
			} else if k == "value" {
				if val, err := strconv.Atoi(v); err == nil {
					res["value"] = val
				} else {
					res["value"] = v
				}
			} else {
				res[k] = v
			}
		}
		return res, nil
	}

	return map[string]interface{}{
		"type": "raw",
		"msg":  parts,
	}, nil
}

func parseKVSpace(input string) map[string]string {
	result := make(map[string]string)
	// Simple space-based KV parser for "type=audio data=[...]"
	// This is naive but should work for the expected format
	pairs := strings.Split(input, " ")
	for _, p := range pairs {
		if strings.Contains(p, "=") {
			parts := strings.SplitN(p, "=", 2)
			if len(parts) == 2 {
				result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}
	return result
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
