package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/gorilla/websocket"
	xdraw "golang.org/x/image/draw"
)

// previewMaxSize is the fixed render size for the local preview image.
// Capping the rendered frame ensures the layout never expands when ffmpeg
// returns a larger native frame (e.g. 640x480 or 1920x1080).
const previewMaxW, previewMaxH = 320, 240

// captureVideoSize and captureFramerate pin dshow/v4l2 to a modest resolution
// so MJPEG frames stay small and ffmpeg does not buffer high-res captures.
const captureVideoSize = "640x480"
const captureFramerate = "30"

// cameraFPS is kept for the existing function signature but unused now: the
// pipeline runs at the camera's native rate (typically 30 fps) and we don't
// dictate a framerate to the device — older ffmpeg on Windows + cameras that
// only support 15/30 fps reject anything else and refuse to open.
const cameraFPS = 30
const maxChatImageBytes = 2 * 1024 * 1024

// CLI reads can fail if the operator starts monitoring before the device is
// ready (e.g. NIBP cuff not yet started). Retry a few times before giving up.
const (
	cliMaxAttempts = 4
	cliRetryDelay  = 2 * time.Second
)

// readingIdleTimeout is how long heart rate / SpO2 waits for new samples before
// saving the last value and stopping automatically.
const readingIdleTimeout = 3 * time.Second

// cliShutdownTimeout bounds how long we wait for lepu_cli to exit after cancel.
const cliShutdownTimeout = 2 * time.Second

const dependenciesDir = "dependencies"

type readingSessionKind int

const (
	readingSessionContinuous readingSessionKind = iota // heart rate — stop after idle
	readingSessionFinal                                // NIBP, glucose, temp — stop on final reading
)

// blankFrame is a 1x1 fully transparent image used in place of nil so that
// canvas.Image always has content. A visible canvas.Image with nil content
// thrashes the GL texture cache (see fyne issue #4345), evicting cached
// button text glyphs and making every button in the window look blank.
var blankFrame = func() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	return img
}()

// splitMJPEGFrames reads a concatenated MJPEG byte stream and emits complete JPEG buffers.
func splitMJPEGFrames(ctx context.Context, r io.Reader, out chan<- []byte) {
	defer close(out)
	pending := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				si := bytes.Index(pending, []byte{0xFF, 0xD8})
				if si < 0 {
					if len(pending) > 512*1024 {
						pending = pending[len(pending)-1024:]
					}
					break
				}
				if si > 0 {
					pending = pending[si:]
				}
				ei := bytes.Index(pending[2:], []byte{0xFF, 0xD9})
				if ei < 0 {
					break
				}
				end := ei + 4
				frame := make([]byte, end)
				copy(frame, pending[:end])
				pending = pending[end:]
				select {
				case out <- frame:
				case <-ctx.Done():
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// coalesceLatestFrame drains ch until empty and returns the newest JPEG.
// closed is true when ch was closed during draining (frame may still be sent).
func coalesceLatestFrame(ch <-chan []byte, first []byte) (frame []byte, closed bool) {
	frame = first
	for {
		select {
		case newer, ok := <-ch:
			if !ok {
				return frame, true
			}
			frame = newer
		default:
			return frame, false
		}
	}
}

// avConfig captures the parameters avfoundation needs to actually open a given
// camera. avfoundation has built-in defaults (29.97 fps, yuv420p) that real
// hardware almost never accepts, so we have to probe and tell ffmpeg explicitly.
type avConfig struct {
	framerate string
	pixfmt    string
}

var (
	avConfigMu    sync.Mutex
	avConfigCache = map[string]avConfig{}
)

// probeAVFoundationConfig runs ffmpeg twice with deliberately invalid options
// to make avfoundation dump the device's supported framerates and pixel formats
// to stderr. It then returns the highest framerate and a preferred pixel format.
// Cached per device so we only pay this cost once.
func probeAVFoundationConfig(ctx context.Context, device string) avConfig {
	avConfigMu.Lock()
	if c, ok := avConfigCache[device]; ok {
		avConfigMu.Unlock()
		return c
	}
	avConfigMu.Unlock()

	cfg := avConfig{
		framerate: probeAVFramerate(ctx, device),
	}
	cfg.pixfmt = probeAVPixelFormat(ctx, device, cfg.framerate)

	avConfigMu.Lock()
	avConfigCache[device] = cfg
	avConfigMu.Unlock()
	return cfg
}

func probeAVFramerate(ctx context.Context, device string) string {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, "ffmpeg",
		"-nostdin", "-hide_banner",
		"-f", "avfoundation", "-framerate", "9999", "-i", device,
		"-f", "null", "-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()

	re := regexp.MustCompile(`@\[([^\]]+)\]fps`)
	var best float64
	for _, m := range re.FindAllStringSubmatch(stderr.String(), -1) {
		for _, f := range strings.Fields(m[1]) {
			v, err := strconv.ParseFloat(f, 64)
			if err == nil && v > best {
				best = v
			}
		}
	}
	if best <= 0 {
		return ""
	}
	if best == float64(int(best)) {
		return strconv.Itoa(int(best))
	}
	return strconv.FormatFloat(best, 'f', -1, 64)
}

func probeAVPixelFormat(ctx context.Context, device, framerate string) string {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	args := []string{"-nostdin", "-hide_banner", "-f", "avfoundation"}
	if framerate != "" {
		args = append(args, "-framerate", framerate)
	}
	// We pass yuv420p — a real, parseable pixel format that nearly all
	// avfoundation cameras refuse. ffmpeg responds by printing the device's
	// "Supported pixel formats" list to stderr, which we then parse. Using a
	// nonsense name like "zzzzzz" doesn't work: ffmpeg rejects it at argument
	// parse time, before it ever tries to talk to the camera.
	args = append(args, "-pixel_format", "yuv420p", "-i", device, "-f", "null", "-")
	cmd := exec.CommandContext(pctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()

	out := stderr.String()
	idx := strings.Index(out, "Supported pixel formats:")
	if idx < 0 {
		return ""
	}
	// (?m) so ^/$ anchor at line boundaries within the captured chunk.
	lineRE := regexp.MustCompile(`(?m)^\[(?:avfoundation|AVFoundation[^\]]*)[^\]]*\]\s+(\w+)\s*$`)
	var available []string
	for _, m := range lineRE.FindAllStringSubmatch(out[idx:], -1) {
		available = append(available, m[1])
	}
	preferred := []string{"uyvy422", "yuyv422", "nv12", "0rgb", "bgr0"}
	for _, p := range preferred {
		for _, a := range available {
			if a == p {
				return p
			}
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return ""
}

// runMJPEGPipe starts ffmpeg writing continuous MJPEG to stdout. Caller must cancel ctx to stop.
func runMJPEGPipe(ctx context.Context, device string, _ int, logFn func(string)) (<-chan []byte, error) {
	var darwinCfg avConfig
	if runtime.GOOS == "darwin" {
		darwinCfg = probeAVFoundationConfig(ctx, device)
		if darwinCfg.framerate != "" || darwinCfg.pixfmt != "" {
			logFn(fmt.Sprintf("Camera config: framerate=%s pixel_format=%s",
				or(darwinCfg.framerate, "default"), or(darwinCfg.pixfmt, "default")))
		}
	}
	args := buildFFmpegArgsForMJPEGPipe(device, darwinCfg)
	logFn(fmt.Sprintf("ffmpeg %s", strings.Join(args, " ")))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	var stderrBuf bytes.Buffer
	var stderrMu sync.Mutex
	go func() {
		b := make([]byte, 4096)
		for {
			n, rerr := stderrPipe.Read(b)
			if n > 0 {
				stderrMu.Lock()
				stderrBuf.Write(b[:n])
				stderrMu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	out := make(chan []byte, 4)
	go func() {
		// Read stdout into MJPEG frames until ffmpeg closes its stdout (exit or kill).
		splitMJPEGFrames(ctx, stdout, out)
		// Now reap and log ALWAYS — even if ctx was cancelled — so we never lose the
		// real ffmpeg error message.
		waitErr := cmd.Wait()
		stderrMu.Lock()
		msg := strings.TrimSpace(stderrBuf.String())
		stderrMu.Unlock()
		if len(msg) > 1500 {
			msg = msg[len(msg)-1500:]
		}
		switch {
		case waitErr != nil && msg != "":
			logFn(fmt.Sprintf("ffmpeg exited: %v — %s", waitErr, msg))
		case waitErr != nil:
			logFn(fmt.Sprintf("ffmpeg exited: %v", waitErr))
		case msg != "":
			logFn(fmt.Sprintf("ffmpeg stderr: %s", msg))
		}
	}()

	return out, nil
}

// or returns a if non-empty, else b.
func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// buildFFmpegArgsForMJPEGPipe builds the ffmpeg argv for a continuous MJPEG
// stream over stdout. On macOS we pass framerate and pixel_format that the
// camera actually supports; avfoundation's defaults (29.97 fps, yuv420p)
// would otherwise be rejected by most real webcams.
//
// Critical: -use_wallclock_as_timestamps 1 (input) and -vsync passthrough
// (output). Without these, avfoundation hands ffmpeg frames with a bogus
// 1,000,000 fps timebase, causing ffmpeg to "fill in" missing frames by
// duplicating the very first frame thousands of times — which is what makes
// the preview look frozen on one image. Wall-clock timestamps + passthrough
// vsync make ffmpeg emit each captured frame exactly once.
//
// On Windows, dshow defaults to buffering several seconds of video unless we
// pass low-latency input flags (same idea as the audio ffmpeg pipe).
func buildFFmpegArgsForMJPEGPipe(device string, darwin avConfig) []string {
	globals := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	// -vsync passthrough: emit each input frame once, never duplicate or drop.
	// -flush_packets 1: push each MJPEG frame to stdout immediately.
	// (Newer ffmpeg also exposes this as -fps_mode passthrough; -vsync still
	// works in current builds and is the only spelling older ffmpeg knows.)
	tail := []string{"-an", "-vsync", "passthrough", "-flush_packets", "1", "-f", "mjpeg", "-q:v", "6", "-"}
	var mid []string
	switch runtime.GOOS {
	case "windows":
		device = normalizeWindowsDeviceName(device)
		mid = []string{
			"-fflags", "+nobuffer",
			"-flags", "low_delay",
			"-probesize", "32",
			"-analyzeduration", "0",
			"-video_size", captureVideoSize,
			"-framerate", captureFramerate,
			"-f", "dshow",
			"-i", device,
		}
	case "darwin":
		mid = []string{"-f", "avfoundation"}
		if darwin.framerate != "" {
			mid = append(mid, "-framerate", darwin.framerate)
		}
		if darwin.pixfmt != "" {
			mid = append(mid, "-pixel_format", darwin.pixfmt)
		}
		// -use_wallclock_as_timestamps overrides avfoundation's broken
		// device timebase so each frame gets a real, monotonic timestamp.
		mid = append(mid, "-use_wallclock_as_timestamps", "1", "-i", device)
	default:
		mid = []string{
			"-fflags", "+nobuffer",
			"-video_size", captureVideoSize,
			"-framerate", captureFramerate,
			"-f", "v4l2",
			"-i", device,
		}
	}
	return append(append(globals, mid...), tail...)
}

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
	ServerBase    string `json:"server_base"`
	ClinicName    string `json:"clinic_name"`
	PatientName   string `json:"patient_name"`
	PatientAge    string `json:"patient_age"`
	PatientWeight string `json:"patient_weight"`
	PatientHeight string `json:"patient_height"`
	PatientGender string `json:"patient_gender"`
	StethMAC      string `json:"steth_mac"`
	LightMode     bool   `json:"light_mode"`
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

// chatWSURL derives the WebSocket chat endpoint from a base URL.
func chatWSURL(base string) string {
	u := strings.TrimRight(base, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	if !strings.HasPrefix(u, "ws") {
		u = "ws://" + u
	}
	return u + "/ws/chat"
}

func mimeFromImagePath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func prepareChatImageFile(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	mime := mimeFromImagePath(path)
	if len(data) <= maxChatImageBytes {
		return data, mime, nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("unsupported image format")
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	maxDim := 1600
	if w > maxDim || h > maxDim {
		scale := float64(maxDim) / float64(max(w, h))
		nw, nh := int(float64(w)*scale), int(float64(h)*scale)
		resized := image.NewRGBA(image.Rect(0, 0, nw, nh))
		xdraw.CatmullRom.Scale(resized, resized.Bounds(), img, bounds, xdraw.Over, nil)
		img = resized
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", err
	}
	if buf.Len() > maxChatImageBytes {
		return nil, "", fmt.Errorf("image too large (max 2 MB)")
	}
	return buf.Bytes(), "image/jpeg", nil
}

var (
	currentCmd    *exec.Cmd
	cmdMutex      sync.Mutex
	cancelFunc    context.CancelFunc
	previewMu     sync.Mutex
	previewCancel context.CancelFunc
	wsConn        *websocket.Conn
	wsMu          sync.Mutex
	feedWriteMu   sync.Mutex // serializes feed WS writes (frames, pings, metadata)
	wsCancel      context.CancelFunc
	streamCancel  context.CancelFunc

	// Audio mic WS (separate connection to /ws/audio-feed)
	micWsConn       *websocket.Conn
	micWsMu         sync.Mutex
	micWsCancel     context.CancelFunc
	micStreamCancel context.CancelFunc

	// Chat WS (/ws/chat)
	chatWsConn   *websocket.Conn
	chatWsMu     sync.Mutex
	chatWsCancel context.CancelFunc

	// Bounded timeout so ingest calls cannot hang the UI thread indefinitely.
	ingestHTTPClient = &http.Client{Timeout: 15 * time.Second}

	// Shared Camera Manager
	sharedCamMu     sync.Mutex
	sharedCamRef    int
	sharedCamDevice string
	sharedCamCtx    context.Context
	sharedCamCancel context.CancelFunc
	sharedCamFrames []chan []byte
)

func writeFeedMessage(messageType int, data []byte) error {
	wsMu.Lock()
	c := wsConn
	wsMu.Unlock()
	if c == nil {
		return fmt.Errorf("feed websocket not connected")
	}
	feedWriteMu.Lock()
	defer feedWriteMu.Unlock()
	return c.WriteMessage(messageType, data)
}

func writeFeedControl(messageType int, data []byte, deadline time.Time) error {
	wsMu.Lock()
	c := wsConn
	wsMu.Unlock()
	if c == nil {
		return fmt.Errorf("feed websocket not connected")
	}
	feedWriteMu.Lock()
	defer feedWriteMu.Unlock()
	return c.WriteControl(messageType, data, deadline)
}

func subscribeSharedCamera(ctx context.Context, device string, fps int, logFn func(string)) (<-chan []byte, func(), error) {
	sharedCamMu.Lock()
	defer sharedCamMu.Unlock()

	if sharedCamRef > 0 {
		if sharedCamDevice != device {
			return nil, nil, fmt.Errorf("camera already in use by %s", sharedCamDevice)
		}
		sharedCamRef++
		ch := make(chan []byte, 4)
		sharedCamFrames = append(sharedCamFrames, ch)

		var unsubOnce sync.Once
		unsub := func() {
			unsubOnce.Do(func() { unsubscribeSharedCamera(ch) })
		}
		return ch, unsub, nil
	}

	sharedCamCtx, sharedCamCancel = context.WithCancel(context.Background())
	srcFrames, err := runMJPEGPipe(sharedCamCtx, device, fps, logFn)
	if err != nil {
		sharedCamCancel()
		return nil, nil, err
	}

	sharedCamDevice = device
	sharedCamRef = 1
	ch := make(chan []byte, 4)
	sharedCamFrames = []chan []byte{ch}

	go func(bgCtx context.Context) {
		for {
			select {
			case <-bgCtx.Done():
				return
			case frame, ok := <-srcFrames:
				if !ok {
					sharedCamMu.Lock()
					for _, c := range sharedCamFrames {
						close(c)
					}
					sharedCamFrames = nil
					sharedCamRef = 0
					if sharedCamCancel != nil {
						sharedCamCancel()
						sharedCamCancel = nil
					}
					sharedCamMu.Unlock()
					return
				}
				sharedCamMu.Lock()
				for _, c := range sharedCamFrames {
					select {
					case c <- frame:
					default:
					}
				}
				sharedCamMu.Unlock()
			}
		}
	}(sharedCamCtx)

	var unsubOnce sync.Once
	unsub := func() {
		unsubOnce.Do(func() { unsubscribeSharedCamera(ch) })
	}
	return ch, unsub, nil
}

func unsubscribeSharedCamera(ch chan []byte) {
	sharedCamMu.Lock()
	defer sharedCamMu.Unlock()

	for i, c := range sharedCamFrames {
		if c == ch {
			sharedCamFrames = append(sharedCamFrames[:i], sharedCamFrames[i+1:]...)
			break
		}
	}
	close(ch)

	sharedCamRef--
	if sharedCamRef == 0 && sharedCamCancel != nil {
		sharedCamCancel()
		sharedCamCancel = nil
	}
}

func main() {
	myApp := app.New()
	myWindow := myApp.NewWindow("Medicart Uploader")

	var connectWS func() error
	var connectChatWS func() error
	var disconnectChatWS func()
	var btnPreviewStart, btnPreviewStop *widget.Button
	var btnBroadcastStart, btnBroadcastStop *widget.Button
	var startBroadcast func()
	var ageEntry, weightEntry, heightEntry *widget.Entry
	var genderSelect *widget.Select

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
	logRich := widget.NewRichText()
	logRich.Wrapping = fyne.TextWrapWord
	logScroll := container.NewScroll(logRich)
	logScroll.SetMinSize(fyne.NewSize(0, 180))

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
			line := fmt.Sprintf("[%s] %s\n", timestamp, msg)
			style := widget.RichTextStyle{ColorName: theme.ColorNameForeground}
			logRich.Segments = append(
				[]widget.RichTextSegment{&widget.TextSegment{Text: line, Style: style}},
				logRich.Segments...,
			)
			logRich.Refresh()

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

	stopBtn = widget.NewButtonWithIcon("Stop", theme.MediaStopIcon(), func() {
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

	uploadECG := func(path string) {
		base := strings.TrimSpace(serverBaseEntry.Text)
		if base == "" {
			log("Error: Enter a Server Base URL in Settings")
			return
		}
		patientName := strings.TrimSpace(patientNameEntry.Text)
		clinicName := strings.TrimSpace(clinicNameEntry.Text)
		if patientName == "" {
			log("Error: Enter a Patient Name in Patients tab")
			return
		}
		if clinicName == "" {
			log("Error: Enter a Clinic Name in Settings")
			return
		}

		data, mime, err := prepareChatImageFile(path)
		if err != nil {
			log(fmt.Sprintf("ECG image error: %v", err))
			return
		}

		payload := map[string]interface{}{
			"patient_name": patientName,
			"clinic_name":  clinicName,
			"type":         "ecg",
			"image":        base64.StdEncoding.EncodeToString(data),
			"image_mime":   mime,
		}
		if err := sendData(ingestURL(base), payload); err != nil {
			log(fmt.Sprintf("ECG upload failed: %v", err))
			return
		}
		log("ECG uploaded successfully")
	}

	btnECGUpload := widget.NewButtonWithIcon("Upload ECG Image", theme.DocumentIcon(), func() {
		dialog.ShowFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			path := reader.URI().Path()
			reader.Close()
			go uploadECG(path)
		}, myWindow)
	})

	// Stethoscope
	var stethMacEntry *widget.Entry

	stethMacEntry = widget.NewEntry()
	stethMacEntry.SetPlaceHolder("AA:BB:CC:DD:EE:FF")
	stethMacEntry.SetText(cfg.StethMAC)

	btnStethoscope := widget.NewButtonWithIcon("Stethoscope", theme.MediaPlayIcon(), func() {
		mac := strings.TrimSpace(stethMacEntry.Text)
		if mac == "" {
			// If no MAC is entered, try to auto-detect if there's exactly one device

			cmdPath := resolveDependencyCLI("MinttiCLI.exe")

			// Run a quick scan to see if we can find exactly one device
			go func() {
				cmd := exec.Command(cmdPath, "-list")
				configureDependencyCmd(cmd, cmdPath)
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

			cmdPath := resolveDependencyCLI("camera_cli.exe")

			cmd := exec.Command(cmdPath, args...)
			configureDependencyCmd(cmd, cmdPath)
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

	// Camera Preview (continuous MJPEG from ffmpeg, ~cameraFPS fps)
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

		frames, unsub, err := subscribeSharedCamera(ctx, device, cameraFPS, cameraLog)
		if err != nil {
			cameraLog(fmt.Sprintf("Preview ffmpeg error: %v", err))
			stopPreviewInternal("")
			return
		}

		previewMu.Lock()
		previewCancel = func() {
			unsub()
			cancel()
		}
		previewMu.Unlock()

		cameraLog(fmt.Sprintf("Starting camera preview for %s", device))
		fyne.Do(func() {
			btnPreviewStart.Disable()
			btnPreviewStop.Enable()
		})

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case jpegBytes, ok := <-frames:
					if !ok {
						stopPreviewInternal("Camera preview ended")
						return
					}
					var ended bool
					jpegBytes, ended = coalesceLatestFrame(frames, jpegBytes)
					img, err := jpeg.Decode(bytes.NewReader(jpegBytes))
					if err != nil {
						continue
					}
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
					if ended {
						stopPreviewInternal("Camera preview ended")
						return
					}
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

	stopStreaming := func(disconnectChat bool) {
		wsMu.Lock()
		if streamCancel == nil {
			wsMu.Unlock()
			return
		}
		streamCancel()
		streamCancel = nil
		wsMu.Unlock()
		if disconnectChat && disconnectChatWS != nil {
			disconnectChatWS()
		}
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
			go func() {
				if connectChatWS != nil {
					if err := connectChatWS(); err != nil {
						log(fmt.Sprintf("Chat auto-connect failed (non-fatal): %v", err))
					}
				}
			}()
			return
		}
		wsMu.Unlock()

		device, err := resolveDevice(cameraEntry.Selected)
		if err != nil {
			cameraLog(fmt.Sprintf("Error: %v", err))
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		
		frames, unsub, err := subscribeSharedCamera(ctx, device, cameraFPS, cameraLog)
		if err != nil {
			cameraLog(fmt.Sprintf("Stream ffmpeg error: %v", err))
			cancel()
			return
		}

		wsMu.Lock()
		streamCancel = func() {
			unsub()
			cancel()
		}
		wsMu.Unlock()

		cameraLog(fmt.Sprintf("Starting stream for %s", device))
		fyne.Do(func() {
			btnBroadcastStart.Disable()
			btnBroadcastStop.Enable()
		})

		go func() {
			wsMu.Lock()
			c := wsConn
			wsMu.Unlock()
			if c == nil {
				cameraLog("WS disconnected before stream")
				stopStreaming(false)
				return
			}
			// Refresh clinic registration once (connectWS already sent on dial).
			if clinic := strings.TrimSpace(clinicNameEntry.Text); clinic != "" {
				metaJSON, _ := json.Marshal(map[string]string{"clinic_name": clinic})
				_ = writeFeedMessage(websocket.TextMessage, metaJSON)
			}

			for {
				select {
				case <-ctx.Done():
					return
				case jpegBytes, ok := <-frames:
					if !ok {
						cameraLog("Stream: ffmpeg ended")
						stopStreaming(false)
						return
					}
					var ended bool
					jpegBytes, ended = coalesceLatestFrame(frames, jpegBytes)

					wsMu.Lock()
					c = wsConn
					wsMu.Unlock()
					if c == nil {
						cameraLog("WS disconnected during stream")
						stopStreaming(false)
						return
					}
					if err := writeFeedMessage(websocket.BinaryMessage, jpegBytes); err != nil {
						cameraLog(fmt.Sprintf("WS send error: %v", err))
						stopStreaming(false)
						return
					}
					if ended {
						cameraLog("Stream: ffmpeg ended")
						stopStreaming(false)
						return
					}
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
			stderrPipe, err := cmd.StderrPipe()
			if err != nil {
				micLog(fmt.Sprintf("ffmpeg stderr pipe error: %v", err))
				return
			}
			var stderrBuf bytes.Buffer
			go func() { _, _ = io.Copy(&stderrBuf, stderrPipe) }()

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
						msg := strings.TrimSpace(stderrBuf.String())
						if msg != "" {
							if len(msg) > 1500 {
								msg = msg[len(msg)-1500:]
							}
							micLog(fmt.Sprintf("ffmpeg stderr: %s", msg))
						}
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

		// Register clinic immediately so the server forwards doctor audio to us.
		clinic := strings.TrimSpace(clinicNameEntry.Text)
		if clinic != "" {
			meta, _ := json.Marshal(map[string]string{"clinic_name": clinic})
			if err := c.WriteMessage(websocket.TextMessage, meta); err != nil {
				micLog(fmt.Sprintf("Mic WS clinic register failed: %v", err))
			} else {
				micLog(fmt.Sprintf("Mic WS registered as clinic %q", clinic))
			}
		} else {
			micLog("WARN: clinic name is empty — doctor audio will NOT be routed here")
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

		// Keep-alive: send a WebSocket ping every 20 s so idle connections
		// survive reverse-proxy / load-balancer TCP timeouts (typically 60 s).
		// Reset the read deadline on every pong so the receive loop only breaks
		// when the connection is truly dead.
		const (
			pingInterval  = 20 * time.Second
			pongDeadline  = 60 * time.Second
		)
		c.SetReadDeadline(time.Now().Add(pongDeadline))
		c.SetPongHandler(func(string) error {
			c.SetReadDeadline(time.Now().Add(pongDeadline))
			return nil
		})
		pingStop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(pingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					deadline := time.Now().Add(5 * time.Second)
					if err := c.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
						return
					}
				case <-pingStop:
					return
				}
			}
		}()
		defer close(pingStop)

		// Incoming binary = doctor mic audio (s16le 16 kHz mono).
		//
		// Playback strategy is platform-specific:
		//   macOS  → ffmpeg piped to -f audiotoolbox (CoreAudio output device).
		//            ffplay 8 silently drops raw-PCM frames; oto's CoreAudio
		//            backend stalls inside fyne (likely run-loop conflict).
		//            ffmpeg + audiotoolbox actually decodes and plays.
		//   Linux  → ffmpeg piped to -f alsa.
		//   Windows → oto/v3 (works reliably; ffplay's SDL is broken on
		//             old Win32 builds).
		var sinkWriter io.WriteCloser
		var ffmpegCmd *exec.Cmd
		// devicePaced is true when sinkWriter is throttled by the audio
		// hardware itself (WinMM): writes block until a buffer drains, so the
		// device clock paces playback and we must NOT also pace with a ticker.
		var devicePaced bool

		switch runtime.GOOS {
		case "windows":
			// Native WinMM waveOut with 16-bit PCM. Works on every Windows
			// release (unlike oto's WASAPI/IAudioClient2 path, which needs
			// Win8+) and matches the s16le wire format exactly. The sink
			// driver below feeds it via sinkWriter; the deferred cleanup
			// closes it.
			micLog("Audio: initialising output (WinMM PCM)…")
			player, perr := newWindowsPCMPlayer(16000, 1, 16)
			if perr != nil {
				micLog(fmt.Sprintf("Audio init FAILED: %v — doctor audio unavailable", perr))
			} else {
				sinkWriter = player
				devicePaced = true
				micLog("Audio playback active (WinMM PCM)")
			}

		default:
			// macOS / Linux — pipe PCM into ffmpeg's audio output device.
			//
			// Latency-critical flags:
			//   -nostdin                  don't capture our terminal stdin
			//   -fflags +nobuffer         no input-side accumulation
			//   -flush_packets 1          (output) push each packet to the
			//                             device immediately
			//   -probesize 32 / -analyzeduration 0
			//                             skip format probing (we already
			//                             specify s16le/16k/mono)
			//
			// We do NOT add aresample here. The doctor browser stops sending
			// any frames at all when their mic is muted — so ffmpeg sees a
			// hard input gap. aresample stretches by ~1k samples/s which
			// can't mask multi-second silences; the audiotoolbox device
			// underruns and refuses to restart. Instead we generate our own
			// silence in the sink driver below, so ffmpeg's input is always
			// continuous and audiotoolbox never sees a gap.
			outFmt := "audiotoolbox"
			if runtime.GOOS == "linux" {
				outFmt = "alsa"
			}
			cmd := exec.Command("ffmpeg",
				"-hide_banner", "-loglevel", "warning", "-nostdin",
				"-fflags", "+nobuffer",
				"-probesize", "32", "-analyzeduration", "0",
				"-f", "s16le", "-ar", "16000", "-ac", "1",
				"-i", "pipe:0",
				"-flush_packets", "1",
				"-f", outFmt, "-",
			)
			cmd.Stdout = io.Discard // audiotoolbox doesn't write stdout, but be explicit
			stdin, perr := cmd.StdinPipe()
			if perr != nil {
				micLog(fmt.Sprintf("ffmpeg stdin error: %v", perr))
			} else {
				stderrPipe, _ := cmd.StderrPipe()
				if err := cmd.Start(); err != nil {
					micLog(fmt.Sprintf("ffmpeg playback start error: %v", err))
				} else {
					ffmpegCmd = cmd
					sinkWriter = stdin
					micLog(fmt.Sprintf("Audio playback active (ffmpeg → %s, pid=%d)", outFmt, cmd.Process.Pid))
					if stderrPipe != nil {
						go func() {
							buf := make([]byte, 1024)
							for {
								n, err := stderrPipe.Read(buf)
								if n > 0 {
									if msg := strings.TrimSpace(string(buf[:n])); msg != "" {
										micLog("ffmpeg-out: " + msg)
									}
								}
								if err != nil {
									return
								}
							}
						}()
					}
					go func() {
						_ = cmd.Wait()
						micLog("ffmpeg playback process exited")
					}()
				}
			}
		}

		// Sink driver — owns sinkWriter exclusively.
		//
		// Real-time paced playback with a bounded jitter buffer. We emit a
		// fixed-size frame on every tick, so the OS audio device queue never
		// grows beyond one frame — this is what keeps doctor→patient latency
		// low. Incoming chunks are appended to a small jitter buffer; if the
		// doctor's audio arrives in a burst (network jitter, server flush)
		// and the buffer exceeds maxJitterBytes, we drop the OLDEST audio so
		// latency stays bounded instead of accumulating permanently (the old
		// "write every chunk immediately" approach let bursts inflate the
		// audiotoolbox queue and that delay never drained). When the buffer
		// runs dry we emit silence so the device never underruns.
		audioCh := make(chan []byte, 64)
		const (
			frameMs         = 20
			bytesPerMs      = 16000 * 2 / 1000     // 32 B/ms (s16le 16 kHz mono)
			frameBytes      = frameMs * bytesPerMs // 640 B per frame
			prebufferBytes  = 80 * bytesPerMs      // ~80 ms cushion before playback starts
			maxJitterBytes  = 120 * bytesPerMs     // ~120 ms ceiling (ffmpeg path latency cap)
			maxLatencyBytes = 300 * bytesPerMs     // device path: drop oldest beyond ~300 ms
		)
		silenceFrame := make([]byte, frameBytes)

		sinkDone := make(chan struct{})
		go func() {
			defer close(sinkDone)
			var framesWritten, realFrames uint64

			if devicePaced {
				// Windows WinMM: the audio hardware paces playback — each
				// sinkWriter.Write blocks until a device buffer drains, so the
				// loop runs at the device rate (no software ticker; Windows
				// tickers fire too coarsely (~15 ms) and starved the buffers,
				// which made the audio choppy).
				//
				// We run a continuous jitter buffer: every cycle we write one
				// frame — real audio when we have it, otherwise a short silence
				// frame. Writing silence on a gap (instead of going idle) keeps
				// the device clock running so playback self-heals smoothly
				// without the stall-and-restart that caused "sometimes nothing
				// comes out". A small prebuffer absorbs normal jitter, and we
				// drop the oldest audio past maxLatencyBytes so delay can't
				// creep up.
				jitter := make([]byte, 0, maxLatencyBytes+frameBytes)
				frame := make([]byte, frameBytes)
				started := false
				for {
					if !started {
						// Accumulate (blocking) until we have the prebuffer.
						chunk, ok := <-audioCh
						if !ok {
							return
						}
						jitter = append(jitter, chunk...)
						if len(jitter) < prebufferBytes {
							continue
						}
						started = true
					} else {
						// Pull everything queued without blocking — we must keep
						// feeding the device on schedule.
					drainCh:
						for {
							select {
							case chunk, ok := <-audioCh:
								if !ok {
									// Stream closed (hang-up). Stop promptly;
									// dropping the last <300 ms is fine.
									return
								}
								jitter = append(jitter, chunk...)
							default:
								break drainCh
							}
						}
					}

					if len(jitter) > maxLatencyBytes {
						jitter = jitter[len(jitter)-maxLatencyBytes:]
					}
					if sinkWriter == nil {
						return
					}

					out := silenceFrame
					real := false
					if len(jitter) >= frameBytes {
						copy(frame, jitter[:frameBytes])
						jitter = jitter[frameBytes:]
						out = frame
						real = true
					}
					if _, err := sinkWriter.Write(out); err != nil { // blocks at device rate
						micLog(fmt.Sprintf("Sink write error: %v", err))
						sinkWriter = nil
						return
					}
					framesWritten++
					if real {
						if realFrames == 0 {
							micLog("Audio: first real frame written to device")
						}
						realFrames++
					}
					if framesWritten%100 == 0 {
						micLog(fmt.Sprintf("Audio: wrote %d frames to device (%d real / %d silence)", framesWritten, realFrames, framesWritten-realFrames))
					}
				}
			}

			// macOS / Linux (ffmpeg): software-paced continuous feed with
			// silence on underrun, because audiotoolbox underruns hard on gaps
			// and refuses to re-arm. The bounded jitter buffer drops the oldest
			// audio on bursts so doctor→patient latency stays low.
			jitter := make([]byte, 0, maxJitterBytes+frameBytes)
			frame := make([]byte, frameBytes)
			ticker := time.NewTicker(frameMs * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case chunk, ok := <-audioCh:
					if !ok {
						return
					}
					jitter = append(jitter, chunk...)
					if len(jitter) > maxJitterBytes {
						jitter = jitter[len(jitter)-maxJitterBytes:]
					}
				case <-ticker.C:
					if sinkWriter == nil {
						continue
					}
					out := silenceFrame
					real := false
					if n := copy(frame, jitter); n > 0 {
						for i := n; i < frameBytes; i++ {
							frame[i] = 0
						}
						jitter = jitter[n:]
						out = frame
						real = true
					}
					if _, err := sinkWriter.Write(out); err != nil {
						micLog(fmt.Sprintf("Sink write error: %v", err))
						sinkWriter = nil
						continue
					}
					framesWritten++
					if real {
						if realFrames == 0 {
							micLog("Audio: first real frame written to device")
						}
						realFrames++
					}
					if framesWritten%100 == 0 {
						micLog(fmt.Sprintf("Audio: wrote %d frames to device (%d real / %d silence)", framesWritten, realFrames, framesWritten-realFrames))
					}
				}
			}
		}()

		// Cleanup: close audioCh → sink drains → close sink → kill ffmpeg.
		audioChClosed := false
		closeAudioCh := func() {
			if !audioChClosed {
				audioChClosed = true
				close(audioCh)
			}
		}
		defer func() {
			closeAudioCh()
			<-sinkDone
			if w := sinkWriter; w != nil {
				_ = w.Close()
			}
			if ffmpegCmd != nil && ffmpegCmd.Process != nil {
				_ = ffmpegCmd.Process.Kill()
			}
		}()

		var chunkCount uint64
		var byteCount uint64
		var dropped uint64
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				micLog(fmt.Sprintf("Mic WS read ended: %v (rx=%d chunks / %d bytes, dropped=%d)", err, chunkCount, byteCount, dropped))
				break
			}
			if mt != websocket.BinaryMessage || len(msg) == 0 {
				continue
			}
			chunkCount++
			byteCount += uint64(len(msg))
			if chunkCount == 1 {
				micLog(fmt.Sprintf("Audio: first doctor chunk received (%d bytes)", len(msg)))
			} else if chunkCount%50 == 0 {
				micLog(fmt.Sprintf("Audio: rx=%d chunks / %d bytes (dropped=%d)", chunkCount, byteCount, dropped))
			}
			if audioChClosed {
				continue
			}
			// Non-blocking send. If the sink is briefly behind, drop the
			// chunk rather than back up the WS receive loop. The silence
			// timer will paper over the dropped chunk.
			select {
			case audioCh <- msg:
			default:
				dropped++
			}
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

	startBroadcast = func() {
		startStreaming()
	}
	stopBroadcast := func() {
		stopStreaming(true)
	}

	btnBroadcastStart = widget.NewButtonWithIcon("Go Live", theme.MediaPlayIcon(), func() { startBroadcast() })
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
			_ = writeFeedMessage(websocket.TextMessage, mj)
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

			// Keep-alive pings (same reason as mic WS — reverse-proxy timeouts).
			c.SetReadDeadline(time.Now().Add(60 * time.Second))
			c.SetPongHandler(func(string) error {
				c.SetReadDeadline(time.Now().Add(60 * time.Second))
				return nil
			})
			wsPingStop := make(chan struct{})
			go func() {
				ticker := time.NewTicker(20 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						_ = writeFeedControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
					case <-wsPingStop:
						return
					}
				}
			}()
			defer close(wsPingStop)

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
				go stopStreaming(false)
		case "mic-on":
			micLog("WS command: mic-on — starting clinic mic stream")
			go startMicStreaming()
		case "mic-off":
			micLog("WS command: mic-off — stopping clinic mic stream")
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
		c := wsConn
		if wsCancel != nil {
			wsCancel()
		}
		wsConn = nil
		wsCancel = nil
		wsMu.Unlock()
		if c != nil {
			_ = writeFeedMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
			c.Close()
		}
		wsStatus.SetText("WS: Disconnected")
	}

	wsConnectBtn := widget.NewButtonWithIcon("Connect", theme.SettingsIcon(), func() { connectWS() })
	wsDisconnectBtn := widget.NewButtonWithIcon("Disconnect", theme.CancelIcon(), disconnectWS)

	// --- Chat (doctor ↔ nurse via web server) ---
	chatStatusLabel := widget.NewLabel("Chat: Disconnected")
	chatFeedBox := container.NewVBox()
	chatScroll := container.NewVScroll(chatFeedBox)
	chatScroll.SetMinSize(fyne.NewSize(0, 280))

	appendChatText := func(line string) {
		fyne.Do(func() {
			lbl := widget.NewLabel(line)
			lbl.Wrapping = fyne.TextWrapWord
			chatFeedBox.Add(lbl)
			chatFeedBox.Refresh()
			chatScroll.ScrollToBottom()
		})
	}

	clearChatFeed := func() {
		fyne.Do(func() {
			chatFeedBox.Objects = nil
			chatFeedBox.Refresh()
		})
	}

	formatChatHeader := func(msg map[string]interface{}) string {
		sender, _ := msg["sender"].(string)
		role, _ := msg["role"].(string)
		ts, _ := msg["timestamp"].(string)
		timeLabel := ts
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			timeLabel = parsed.Local().Format("15:04")
		}
		label := sender
		if role != "" {
			label = fmt.Sprintf("%s (%s)", sender, role)
		}
		if timeLabel != "" {
			return fmt.Sprintf("[%s] %s", timeLabel, label)
		}
		return label
	}

	appendChatImage := func(header, imageB64, mime, caption string) {
		fyne.Do(func() {
			if header != "" {
				lbl := widget.NewLabel(header)
				lbl.Wrapping = fyne.TextWrapWord
				chatFeedBox.Add(lbl)
			}
			data, err := base64.StdEncoding.DecodeString(imageB64)
			if err != nil {
				chatFeedBox.Add(widget.NewLabel("[Invalid image attachment]"))
				chatFeedBox.Refresh()
				chatScroll.ScrollToBottom()
				return
			}
			img := canvas.NewImageFromResource(fyne.NewStaticResource("chat-image", data))
			img.FillMode = canvas.ImageFillContain
			img.SetMinSize(fyne.NewSize(220, 160))

			imageData := append([]byte(nil), data...)
			saveBtn := widget.NewButtonWithIcon("Save image", theme.DownloadIcon(), func() {
				dialog.ShowFileSave(func(writer fyne.URIWriteCloser, err error) {
					if err != nil || writer == nil {
						return
					}
					defer writer.Close()
					_, _ = writer.Write(imageData)
				}, myWindow)
			})

			parts := []fyne.CanvasObject{img}
			if caption != "" {
				capLbl := widget.NewLabel(caption)
				capLbl.Wrapping = fyne.TextWrapWord
				parts = append(parts, capLbl)
			}
			parts = append(parts, saveBtn)
			chatFeedBox.Add(container.NewVBox(parts...))
			chatFeedBox.Refresh()
			chatScroll.ScrollToBottom()
		})
	}

	formatChatLine := func(msg map[string]interface{}) string {
		header := formatChatHeader(msg)
		text, _ := msg["text"].(string)
		if text == "" {
			return header
		}
		return fmt.Sprintf("%s: %s", header, text)
	}

	disconnectChatWS = func() {
		chatWsMu.Lock()
		if chatWsConn != nil {
			_ = chatWsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
			chatWsConn.Close()
		}
		if chatWsCancel != nil {
			chatWsCancel()
		}
		chatWsConn = nil
		chatWsCancel = nil
		chatWsMu.Unlock()
		fyne.Do(func() { chatStatusLabel.SetText("Chat: Disconnected") })
	}

	connectChatWS = func() error {
		chatWsMu.Lock()
		if chatWsConn != nil {
			chatWsMu.Unlock()
			return nil
		}
		chatWsMu.Unlock()

		base := strings.TrimSpace(serverBaseEntry.Text)
		if base == "" {
			return fmt.Errorf("enter a Server Base URL in Settings")
		}
		clinic := strings.TrimSpace(clinicNameEntry.Text)
		if clinic == "" {
			return fmt.Errorf("enter a Clinic Name in Settings")
		}

		ctx, cancel := context.WithCancel(context.Background())
		c, _, err := websocket.DefaultDialer.DialContext(ctx, chatWSURL(base), nil)
		if err != nil {
			cancel()
			return err
		}

		chatWsMu.Lock()
		chatWsConn = c
		chatWsCancel = cancel
		chatWsMu.Unlock()

		fyne.Do(func() { chatStatusLabel.SetText("Chat: Connected") })
		clearChatFeed()
		appendChatText("Connected to clinic chat.")

		register := map[string]string{
			"type":        "register",
			"clinic_name": clinic,
			"sender":      "Clinic Nurse",
			"role":        "nurse",
		}
		regJSON, _ := json.Marshal(register)
		if err := c.WriteMessage(websocket.TextMessage, regJSON); err != nil {
			disconnectChatWS()
			return err
		}

		go func() {
			conn := c
			defer func() {
				chatWsMu.Lock()
				if chatWsConn == conn {
					chatWsConn = nil
				}
				chatWsMu.Unlock()
				fyne.Do(func() { chatStatusLabel.SetText("Chat: Disconnected") })
			}()
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			conn.SetPongHandler(func(string) error {
				conn.SetReadDeadline(time.Now().Add(60 * time.Second))
				return nil
			})
			pingStop := make(chan struct{})
			go func() {
				ticker := time.NewTicker(20 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
					case <-pingStop:
						return
					case <-ctx.Done():
						return
					}
				}
			}()
			defer close(pingStop)

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				conn.SetReadDeadline(time.Now().Add(60 * time.Second))
				_, msg, err := conn.ReadMessage()
				if err != nil {
					if ctx.Err() != context.Canceled {
						appendChatText(fmt.Sprintf("Chat disconnected: %v", err))
					}
					return
				}
				var payload map[string]interface{}
				if err := json.Unmarshal(msg, &payload); err != nil {
					continue
				}
				if payload["type"] == "chat" {
					if img, ok := payload["image"].(string); ok && img != "" {
						mime, _ := payload["image_mime"].(string)
						caption, _ := payload["text"].(string)
						appendChatImage(formatChatHeader(payload), img, mime, caption)
					} else {
						appendChatText(formatChatLine(payload))
					}
				}
			}
		}()

		return nil
	}

	startBroadcast = func() {
		go func() {
			if err := connectChatWS(); err != nil {
				log(fmt.Sprintf("Chat auto-connect failed (non-fatal): %v", err))
			}
		}()
		go startMicStreaming()
		startStreaming()
	}

	chatInput := widget.NewEntry()
	chatInput.SetPlaceHolder("Message the doctor…")

	sendChatMessage := func() {
		text := strings.TrimSpace(chatInput.Text)
		if text == "" {
			return
		}
		chatWsMu.Lock()
		c := chatWsConn
		chatWsMu.Unlock()
		if c == nil {
			appendChatText("Chat not connected — go live to enable chat.")
			return
		}
		payload, _ := json.Marshal(map[string]string{
			"type": "chat",
			"text": text,
		})
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			appendChatText(fmt.Sprintf("Send failed: %v", err))
			return
		}
		fyne.Do(func() { chatInput.SetText("") })
	}

	sendChatImage := func(path string) {
		chatWsMu.Lock()
		c := chatWsConn
		chatWsMu.Unlock()
		if c == nil {
			appendChatText("Chat not connected — go live to enable chat.")
			return
		}

		data, mime, err := prepareChatImageFile(path)
		if err != nil {
			appendChatText(fmt.Sprintf("Image error: %v", err))
			return
		}

		msg := map[string]string{
			"type":       "chat",
			"image":      base64.StdEncoding.EncodeToString(data),
			"image_mime": mime,
		}
		if text := strings.TrimSpace(chatInput.Text); text != "" {
			msg["text"] = text
		}
		payload, _ := json.Marshal(msg)
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			appendChatText(fmt.Sprintf("Send failed: %v", err))
			return
		}
		fyne.Do(func() { chatInput.SetText("") })
	}

	btnChatAttach := widget.NewButtonWithIcon("", theme.ContentAddIcon(), func() {
		dialog.ShowFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			path := reader.URI().Path()
			reader.Close()
			go sendChatImage(path)
		}, myWindow)
	})
	btnChatAttach.Importance = widget.LowImportance

	btnChatSend := widget.NewButtonWithIcon("Send", theme.MailSendIcon(), sendChatMessage)
	btnChatSend.Importance = widget.HighImportance
	chatInput.OnSubmitted = func(string) { sendChatMessage() }

	var btnSaveSettings *widget.Button
	btnSaveSettings = widget.NewButtonWithIcon("Save Settings", theme.DocumentSaveIcon(), func() {
		newCfg := AppConfig{
			ServerBase:    strings.TrimSpace(serverBaseEntry.Text),
			ClinicName:    strings.TrimSpace(clinicNameEntry.Text),
			PatientName:   strings.TrimSpace(patientNameEntry.Text),
			PatientAge:    strings.TrimSpace(ageEntry.Text),
			PatientWeight: strings.TrimSpace(weightEntry.Text),
			PatientHeight: strings.TrimSpace(heightEntry.Text),
			PatientGender: genderSelect.Selected,
			StethMAC:      strings.TrimSpace(stethMacEntry.Text),
			LightMode:     lightModeCheck.Checked,
		}
		btnSaveSettings.Disable()
		log("Saving settings...")
		go func(cfg AppConfig) {
			err := saveAppConfig(cfg)
			fyne.Do(func() {
				btnSaveSettings.Enable()
				if err != nil {
					log(fmt.Sprintf("Error saving settings: %v", err))
					return
				}
				log(fmt.Sprintf("Settings saved (server: %s)", cfg.ServerBase))
				dialog.ShowInformation("Saved Successfully", "Your settings have been saved successfully.", myWindow)
			})
		}(newCfg)
	})
	btnSaveSettings.Importance = widget.HighImportance


	// --- Layout Refactor ---

	// 1. Patients Tab
	ageEntry = widget.NewEntry()
	ageEntry.SetPlaceHolder("e.g. 62")
	if cfg.PatientAge != "" {
		ageEntry.SetText(cfg.PatientAge)
	}
	weightEntry = widget.NewEntry()
	weightEntry.SetPlaceHolder("e.g. 85")
	if cfg.PatientWeight != "" {
		weightEntry.SetText(cfg.PatientWeight)
	}
	heightEntry = widget.NewEntry()
	heightEntry.SetPlaceHolder("e.g. 180")
	if cfg.PatientHeight != "" {
		heightEntry.SetText(cfg.PatientHeight)
	}
	genderOptions := []string{"Male", "Female", "Other"}
	genderSelect = widget.NewSelect(genderOptions, nil)
	if cfg.PatientGender != "" {
		genderSelect.SetSelected(cfg.PatientGender)
	}

	var btnSavePatient *widget.Button
	savePatientProfile := func() {
		base := strings.TrimSpace(serverBaseEntry.Text)
		if base == "" {
			log("Error: Enter a Server Base URL in Settings")
			return
		}
		patientName := strings.TrimSpace(patientNameEntry.Text)
		clinicName := strings.TrimSpace(clinicNameEntry.Text)
		if patientName == "" {
			log("Error: Enter a Patient Name")
			return
		}
		if clinicName == "" {
			log("Error: Enter a Clinic Name in Settings")
			return
		}
		if genderSelect.Selected == "" {
			log("Error: Select a gender")
			return
		}

		age, _ := strconv.Atoi(strings.TrimSpace(ageEntry.Text))
		weight, _ := strconv.ParseFloat(strings.TrimSpace(weightEntry.Text), 64)
		height, _ := strconv.ParseFloat(strings.TrimSpace(heightEntry.Text), 64)

		newCfg := AppConfig{
			ServerBase:    base,
			ClinicName:    clinicName,
			PatientName:   patientName,
			PatientAge:    strings.TrimSpace(ageEntry.Text),
			PatientWeight: strings.TrimSpace(weightEntry.Text),
			PatientHeight: strings.TrimSpace(heightEntry.Text),
			PatientGender: genderSelect.Selected,
			StethMAC:      strings.TrimSpace(stethMacEntry.Text),
			LightMode:     lightModeCheck.Checked,
		}
		payload := map[string]interface{}{
			"type":         "profile",
			"patient_name": patientName,
			"clinic_name":  clinicName,
			"gender":       genderSelect.Selected,
			"age":          age,
			"weight":       weight,
			"height":       height,
		}

		btnSavePatient.Disable()
		log("Saving patient profile...")
		go func(cfg AppConfig, uploadURL string, body map[string]interface{}) {
			if err := saveAppConfig(cfg); err != nil {
				fyne.Do(func() {
					btnSavePatient.Enable()
					log(fmt.Sprintf("Error saving patient info locally: %v", err))
				})
				return
			}
			if err := sendData(uploadURL, body); err != nil {
				fyne.Do(func() {
					btnSavePatient.Enable()
					log(fmt.Sprintf("Patient profile upload failed: %v", err))
				})
				return
			}
			fyne.Do(func() {
				btnSavePatient.Enable()
				log("Patient profile saved")
				dialog.ShowInformation("Saved Successfully", "Patient profile has been saved successfully.", myWindow)
			})
		}(newCfg, ingestURL(base), payload)
	}

	btnSavePatient = widget.NewButtonWithIcon("Save Patient Info", theme.DocumentSaveIcon(), savePatientProfile)
	btnSavePatient.Importance = widget.HighImportance

	patientOverview := container.NewGridWithColumns(2,
		container.NewVBox(widget.NewLabelWithStyle("AGE (YRS)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), ageEntry),
		container.NewVBox(widget.NewLabelWithStyle("GENDER", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), genderSelect),
		container.NewVBox(widget.NewLabelWithStyle("WEIGHT (KG)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), weightEntry),
		container.NewVBox(widget.NewLabelWithStyle("HEIGHT (CM)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), heightEntry),
	)

	patientsContent := container.NewVBox(
		widget.NewCard("Identity Context", "Select Patient", container.NewVBox(
			patientNameLabel, patientNameEntry,
			btnSavePatient,
		)),
		widget.NewCard("Patient Overview", "Demographics associated with this patient", patientOverview),
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
		widget.NewCard("ECG", "Upload an ECG strip or snapshot image", container.NewVBox(
			btnECGUpload,
		)),
		widget.NewSeparator(),
		widget.NewCard("Stethoscope", "Search, connect, and stream auscultation", container.NewVBox(
			widget.NewLabel("MAC Address (optional):"),
			stethMacEntry,
			btnStethoscope,
		)),
		widget.NewSeparator(),
		widget.NewCard("Live Console", "System Active", container.NewVBox(
			statusLabel,
			container.NewStack(logScroll),
			stopBtn,
		)),
	)

	// 3. Comms Tab — split into Video, Control, and Chat sub-tabs
	videoContent := container.NewVBox(
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
	)

	controlContent := container.NewVBox(
		widget.NewCard("Camera Control", "Precision Pan-Tilt-Zoom", container.NewVBox(
			btnCamList,
			container.NewCenter(
				container.NewGridWithColumns(3,
					widget.NewLabel(""), btnCamUp, widget.NewLabel(""),
					btnCamLeft, widget.NewButtonWithIcon("Reset", theme.ViewRefreshIcon(), func() {
						runCameraCommand("reset", []string{"-reset"})
					}), btnCamRight,
					widget.NewLabel(""), btnCamDown, widget.NewLabel(""),
				),
			),
			btnCamFlip,
		)),
	)

	chatContent := container.NewBorder(
		container.NewVBox(
			chatStatusLabel,
			widget.NewLabel("Chat activates automatically when you go live."),
		),
		container.NewBorder(nil, nil, btnChatAttach, btnChatSend, chatInput),
		nil,
		nil,
		chatScroll,
	)

	commsTabs := container.NewAppTabs(
		container.NewTabItemWithIcon("Video", theme.MediaPlayIcon(), container.NewVScroll(videoContent)),
		container.NewTabItemWithIcon("Control", theme.SettingsIcon(), container.NewVScroll(controlContent)),
		container.NewTabItemWithIcon("Chat", theme.MailComposeIcon(), chatContent),
	)
	commsTabs.SetTabLocation(container.TabLocationTop)

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
		container.NewTabItemWithIcon("Comms", theme.VisibilityIcon(), commsTabs),
		container.NewTabItemWithIcon("Settings", theme.SettingsIcon(), container.NewVScroll(settingsContent)),
	)
	tabs.SetTabLocation(container.TabLocationBottom) // Move tabs to bottom to mimic mobile/app feel from the designs

	myWindow.SetContent(tabs)
	myWindow.Resize(fyne.NewSize(480, 800))

	myWindow.ShowAndRun()
}

type cliAttemptResult struct {
	cancelled      bool
	completed      bool // auto-stopped after final reading or idle timeout
	receivedOutput bool
	errMsg         string
}

func (r cliAttemptResult) succeeded() bool {
	return r.receivedOutput || r.completed
}

func readingSessionKindForName(name string) readingSessionKind {
	switch name {
	case "HeartRate":
		return readingSessionContinuous
	default:
		return readingSessionFinal
	}
}

func isFinalReading(data map[string]interface{}) bool {
	switch t, _ := data["type"].(string); t {
	case "result":
		return true
	case "data":
		_, hasGlu := data["glu"]
		_, hasTemp := data["temp"]
		return hasGlu || hasTemp
	default:
		return false
	}
}

func sendReadingPayload(log func(string), targetURL, clinicName, patientName string, data map[string]interface{}) error {
	payload := maps.Clone(data)
	payload["patient_name"] = patientName
	payload["clinic_name"] = clinicName
	log(fmt.Sprintf("Sending reading: %v", payload))
	return sendData(targetURL, payload)
}

func waitCLIShutdown(scanDone <-chan struct{}, cmd *exec.Cmd) error {
	select {
	case <-scanDone:
	case <-time.After(cliShutdownTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-scanDone:
		case <-time.After(cliShutdownTimeout):
		}
	}
	return cmd.Wait()
}

func normalizeCLILine(line string) string {
	normalized := strings.TrimSpace(line)
	normalized = strings.TrimPrefix(normalized, "\ufeff")
	normalized = strings.ReplaceAll(normalized, " ", "")
	return strings.ToUpper(normalized)
}

func defaultAppBaseDir() string {
	exe, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		if !isGoBuildCacheDir(dir) {
			return dir
		}
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return "."
	}
	return wd
}

var appBaseDir = defaultAppBaseDir

func isGoBuildCacheDir(dir string) bool {
	base := filepath.Base(dir)
	return strings.HasPrefix(base, "go-build") ||
		strings.Contains(dir, filepath.Join(os.TempDir(), "go-build"))
}

func bundledCLIPath(exeName string) string {
	return filepath.Join(appBaseDir(), dependenciesDir, exeName)
}

func resolveDependencyCLI(exeName string) string {
	if bundled := bundledCLIPath(exeName); fileExists(bundled) {
		return mustAbs(bundled)
	}

	if path, err := exec.LookPath(exeName); err == nil && fileExists(path) {
		return mustAbs(path)
	}

	if local := filepath.Join(".", exeName); fileExists(local) {
		return mustAbs(local)
	}

	return mustAbs(bundledCLIPath(exeName))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func configureDependencyCmd(cmd *exec.Cmd, cmdPath string) {
	abs := mustAbs(cmdPath)
	cmd.Path = abs
	if len(cmd.Args) > 0 {
		cmd.Args[0] = abs
	}
	if dir := filepath.Dir(abs); dir != "" && dir != "." {
		cmd.Dir = dir
	}
}

func resolveCLIPath(name string) string {
	cmdPath := "lepu_cli.exe"
	if name == "StethoscopeList" || name == "StethoscopeStream" {
		cmdPath = "MinttiCLI.exe"
	}
	return resolveDependencyCLI(cmdPath)
}

func runCLIOnce(ctx context.Context, cancel context.CancelFunc, cmdPath string, args []string, parser LineParser, sessionKind readingSessionKind, targetURL, clinicName, patientName string, log func(string)) cliAttemptResult {
	result := cliAttemptResult{}

	if ctx.Err() != nil {
		result.cancelled = true
		return result
	}

	cmd := exec.CommandContext(ctx, cmdPath, args...)
	configureDependencyCmd(cmd, cmdPath)

	cmdMutex.Lock()
	currentCmd = cmd
	cmdMutex.Unlock()

	defer func() {
		cmdMutex.Lock()
		if currentCmd == cmd {
			currentCmd = nil
		}
		cmdMutex.Unlock()
	}()

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.errMsg = fmt.Sprintf("error creating stdout pipe: %v", err)
		return result
	}

	if err := cmd.Start(); err != nil {
		result.errMsg = fmt.Sprintf("error starting process: %v", err)
		return result
	}

	scanner := bufio.NewScanner(stdout)
	lineCh := make(chan string)
	scanDone := make(chan struct{})
	go func() {
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		close(lineCh)
		close(scanDone)
	}()

	var pendingReading map[string]interface{}
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	resetIdle := func() {
		if sessionKind != readingSessionContinuous {
			return
		}
		if idleTimer == nil {
			idleTimer = time.NewTimer(readingIdleTimeout)
			idleC = idleTimer.C
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(readingIdleTimeout)
	}
	if sessionKind == readingSessionContinuous {
		resetIdle()
	}
	defer func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
	}()

	finishAfterSend := func(reason string) cliAttemptResult {
		if pendingReading == nil {
			result.errMsg = "no reading to send"
			cancel()
			go waitCLIShutdown(scanDone, cmd)
			return result
		}
		if reason != "" {
			log(reason)
		}
		if err := sendReadingPayload(log, targetURL, clinicName, patientName, pendingReading); err != nil {
			log(fmt.Sprintf("Error sending data: %v", err))
			result.errMsg = err.Error()
		} else {
			result.completed = true
		}
		cancel()
		go waitCLIShutdown(scanDone, cmd)
		return result
	}

	for {
		select {
		case <-ctx.Done():
			goto flush
		case <-idleC:
			if pendingReading == nil {
				log("No readings received.")
				cancel()
				go waitCLIShutdown(scanDone, cmd)
				result.errMsg = "no data received from device"
				return result
			}
			return finishAfterSend("No new readings — saving last value.")
		case line, ok := <-lineCh:
			if !ok {
				goto flush
			}
			resetIdle()

			data, err := parser(line)
			if err != nil {
				continue
			}
			if data == nil {
				continue
			}

			result.receivedOutput = true
			dataMap, ok := data.(map[string]interface{})
			if !ok {
				continue
			}

			if dataMap["type"] == "error" {
				log(fmt.Sprintf("Device error: %v", dataMap))
				cancel()
				go waitCLIShutdown(scanDone, cmd)
				result.errMsg = fmt.Sprintf("device error: %v", dataMap)
				return result
			}
			if !shouldSendReading(dataMap) {
				continue
			}

			pendingReading = maps.Clone(dataMap)
			if sessionKind == readingSessionFinal && isFinalReading(dataMap) {
				return finishAfterSend("Final reading received.")
			}
		}
	}

flush:
	if pendingReading != nil && !result.completed {
		if err := sendReadingPayload(log, targetURL, clinicName, patientName, pendingReading); err != nil {
			log(fmt.Sprintf("Error sending data: %v", err))
		} else {
			result.completed = true
		}
	}

	waitErr := waitCLIShutdown(scanDone, cmd)
	if ctx.Err() == context.Canceled {
		if result.completed {
			return result
		}
		result.cancelled = true
		return result
	}

	if waitErr != nil {
		result.errMsg = waitErr.Error()
		if stderr := strings.TrimSpace(stderrBuf.String()); stderr != "" {
			result.errMsg = fmt.Sprintf("%s: %s", result.errMsg, stderr)
		}
	} else if !result.receivedOutput {
		result.errMsg = "no data received from device"
		if stderr := strings.TrimSpace(stderrBuf.String()); stderr != "" {
			result.errMsg = fmt.Sprintf("%s: %s", result.errMsg, stderr)
		}
	}

	return result
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

	cmdPath := resolveCLIPath(name)
	var lastErr string

	for attempt := 1; attempt <= cliMaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				log("Process stopped by user.")
				return
			case <-time.After(cliRetryDelay):
			}
		}

		log(fmt.Sprintf("Starting %s (attempt %d/%d, %s)...", name, attempt, cliMaxAttempts, cmdPath))

		result := runCLIOnce(ctx, cancel, cmdPath, args, parser, readingSessionKindForName(name), targetURL, clinicName, patientName, log)
		if result.cancelled {
			log("Process stopped by user.")
			return
		}
		if result.completed || result.succeeded() {
			if result.receivedOutput {
				if result.completed {
					log("Reading complete.")
				} else {
					log("Process finished successfully.")
				}
			}
			return
		}

		lastErr = result.errMsg
		if attempt < cliMaxAttempts {
			log(fmt.Sprintf("Attempt %d failed: %s", attempt, lastErr))
		}
	}

	log(fmt.Sprintf("Error: %s failed after %d attempts: %s", name, cliMaxAttempts, lastErr))
}

func sendData(url string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	resp, err := ingestHTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
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
	pre := []string{"-nostdin", "-hide_banner"}
	switch runtime.GOOS {
	case "windows":
		// exec.Command passes args directly to the OS — no shell quoting.
		// dshow expects audio=Device Name (no embedded quotes).
		// Strip any quotes the detection step may have left in.
		device = strings.Trim(device, `"`)
		return append(append(pre, "-f", "dshow", "-i", "audio="+device), out...)
	case "darwin":
		// "none:<audio_idx>" captures audio only; accepted by both old and new ffmpeg builds.
		return append(append(pre, "-f", "avfoundation", "-i", "none:"+device), out...)
	default:
		return append(append(pre, "-f", "alsa", "-i", device), out...)
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

func shouldSendReading(data map[string]interface{}) bool {
	switch t, _ := data["type"].(string); t {
	case "cuff_update", "status", "error", "discovery":
		return false
	default:
		return true
	}
}

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
	normalized := normalizeCLILine(line)
	if strings.HasPrefix(normalized, "DATA:GLU=") {
		valStr := strings.TrimPrefix(normalized, "DATA:GLU=")
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
	normalized := normalizeCLILine(line)
	if strings.HasPrefix(normalized, "DATA:TEMP=") {
		valStr := strings.TrimPrefix(normalized, "DATA:TEMP=")
		if idx := strings.Index(valStr, ","); idx >= 0 {
			valStr = valStr[:idx]
		}
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
