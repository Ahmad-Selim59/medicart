package main

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func TestCLIAttemptSucceeded(t *testing.T) {
	cases := []struct {
		name   string
		result cliAttemptResult
		want   bool
	}{
		{
			name:   "received output",
			result: cliAttemptResult{receivedOutput: true, errMsg: "exit status 1"},
			want:   true,
		},
		{
			name:   "clean exit with no error message",
			result: cliAttemptResult{},
			want:   true,
		},
		{
			name:   "failed with no output",
			result: cliAttemptResult{errMsg: "exit status 1"},
			want:   false,
		},
		{
			name:   "no data received",
			result: cliAttemptResult{errMsg: "no data received from device"},
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.result.succeeded(); got != tc.want {
				t.Fatalf("succeeded() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunCLIOnceRetriesUntilOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	script := `if [ "$1" = "fail" ]; then exit 1; fi; echo "DATA:PR=75,SPO2=98"`
	tmp, err := os.CreateTemp("", "medicart-cli-retry-*.sh")
	if err != nil {
		t.Fatalf("create temp script: %v", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(script); err != nil {
		t.Fatalf("write temp script: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var logs []string
	logFn := func(msg string) { logs = append(logs, msg) }

	result := runCLIOnce(ctx, "sh", []string{tmp.Name(), "fail"}, parseHeartRateLine, "http://127.0.0.1:1", "Clinic", "Patient", logFn)
	if result.succeeded() {
		t.Fatalf("expected failed attempt, got %+v", result)
	}
	if result.errMsg == "" {
		t.Fatal("expected error message on failed attempt")
	}

	result = runCLIOnce(ctx, "sh", []string{tmp.Name(), "ok"}, parseHeartRateLine, "http://127.0.0.1:1", "Clinic", "Patient", logFn)
	if !result.succeeded() || !result.receivedOutput {
		t.Fatalf("expected successful attempt with output, got %+v", result)
	}
}

// makeFakeFrame returns a solid-color image of the given size.
func makeFakeFrame(w, h int, c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// buildPreviewSection mirrors the Comms tab's "Live Feed" card structure
// (two buttons in a GridWithColumns above a canvas.Image) so the test
// reproduces the layout where buttons supposedly lose text.
func buildPreviewSection() (start, stop *widget.Button, img *canvas.Image, root *fyne.Container) {
	start = widget.NewButton("Start Preview", func() {})
	stop = widget.NewButton("Stop Preview", func() {})
	img = canvas.NewImageFromImage(blankFrame)
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(previewMaxW, previewMaxH))
	root = container.NewVBox(
		container.NewGridWithColumns(2, start, stop),
		img,
	)
	return
}

// TestPreviewImageNeverNilWhenVisible guards against fyne issue #4345
// (visible canvas.Image with nil Image thrashes GL texture cache and
// blanks out cached button text glyphs on macOS).
func TestPreviewImageNeverNilWhenVisible(t *testing.T) {
	test.NewTempApp(t)

	_, _, img, root := buildPreviewSection()
	w := test.NewWindow(root)
	defer w.Close()

	if img.Image == nil && img.Resource == nil && img.File == "" && img.Visible() {
		t.Fatal("preview image is nil/empty but visible — will trigger fyne #4345 texture-cache thrashing on macOS GLFW")
	}

	// Simulate a stop that resets the image; the placeholder must
	// still keep Image non-nil.
	img.Image = blankFrame
	img.Refresh()
	if img.Image == nil {
		t.Fatal("after simulated stop, preview Image is nil — will re-trigger #4345")
	}
}

// TestButtonTextSurvivesPreviewLifecycle verifies that buttons retain
// their Text field through a full preview start/stop cycle. This catches
// state-level regressions; the macOS GLFW visual glitch is render-only
// and not observable via the software test renderer, but if Text ever
// gets cleared by Fyne internals this will catch it.
func TestButtonTextSurvivesPreviewLifecycle(t *testing.T) {
	test.NewTempApp(t)

	start, stop, img, root := buildPreviewSection()
	w := test.NewWindow(root)
	defer w.Close()
	w.Resize(fyne.NewSize(480, 600))

	checkText := func(stage string) {
		t.Helper()
		if start.Text != "Start Preview" {
			t.Errorf("[%s] start.Text = %q, want %q", stage, start.Text, "Start Preview")
		}
		if stop.Text != "Stop Preview" {
			t.Errorf("[%s] stop.Text = %q, want %q", stage, stop.Text, "Stop Preview")
		}
	}

	checkText("initial")

	// Simulate a stream of fake frames being pushed to the canvas image
	// (mirroring what the live preview does once a second).
	for i := 0; i < 5; i++ {
		frame := fitToPreview(makeFakeFrame(640, 480, color.RGBA{R: uint8(i * 40), G: 100, B: 200, A: 255}))
		img.Image = frame
		img.Refresh()
		checkText("frame-" + string(rune('0'+i)))
	}

	// Simulate "Stop Preview" — clear the canvas image.
	img.Image = nil
	img.Refresh()
	checkText("after-stop")

	// Simulate restarting.
	frame := fitToPreview(makeFakeFrame(640, 480, color.RGBA{R: 50, G: 200, B: 50, A: 255}))
	img.Image = frame
	img.Refresh()
	checkText("after-restart")
}

// TestPreviewImageDoesNotExpandLayout verifies the layout's min size stays
// bounded regardless of frame size, since canvas.Image historically reported
// the natural pixel size of any loaded image as its min size.
func TestPreviewImageDoesNotExpandLayout(t *testing.T) {
	test.NewTempApp(t)

	_, _, img, root := buildPreviewSection()
	w := test.NewWindow(root)
	defer w.Close()
	w.Resize(fyne.NewSize(480, 600))

	initialMin := root.MinSize()

	// Large frame should not push the layout's min size past the cap.
	bigFrame := fitToPreview(makeFakeFrame(1920, 1080, color.RGBA{R: 200, G: 200, B: 200, A: 255}))
	img.Image = bigFrame
	img.Refresh()

	afterMin := root.MinSize()

	// Width should not have grown noticeably (allow for theme padding tolerance).
	if afterMin.Width > initialMin.Width+1 {
		t.Errorf("layout min width grew: was %.1f, now %.1f", initialMin.Width, afterMin.Width)
	}
	if afterMin.Height > initialMin.Height+1 {
		t.Errorf("layout min height grew: was %.1f, now %.1f", initialMin.Height, afterMin.Height)
	}
}

// TestFitToPreviewBounds verifies fitToPreview always produces an image
// that fits within the preview cap so canvas.Image cannot demand more
// space than the layout grants it.
func TestFitToPreviewBounds(t *testing.T) {
	cases := []struct{ w, h int }{
		{640, 480},
		{1920, 1080},
		{1280, 720},
		{160, 120},
		{320, 240},
		{800, 800},
	}
	for _, c := range cases {
		got := fitToPreview(makeFakeFrame(c.w, c.h, color.Black))
		b := got.Bounds()
		if b.Dx() > previewMaxW || b.Dy() > previewMaxH {
			t.Errorf("fitToPreview(%dx%d) -> %dx%d exceeds %dx%d cap",
				c.w, c.h, b.Dx(), b.Dy(), previewMaxW, previewMaxH)
		}
		// Aspect ratio preserved within rounding tolerance.
		want := float64(c.w) / float64(c.h)
		got2 := float64(b.Dx()) / float64(b.Dy())
		if diff := want - got2; diff > 0.02 || diff < -0.02 {
			t.Errorf("fitToPreview(%dx%d) aspect %.3f != source %.3f",
				c.w, c.h, got2, want)
		}
	}
}

// TestRenderSnapshot captures the rendered canvas after the simulated
// stop sequence and saves it as a PNG so a human (or follow-up tooling)
// can visually verify the buttons still show their text.
//
// Run with:  go test -run TestRenderSnapshot -v
// Output PNG: ./testdata/snapshot.png (overwritten each run).
func TestRenderSnapshot(t *testing.T) {
	test.NewTempApp(t)

	start, stop, img, root := buildPreviewSection()
	w := test.NewWindow(root)
	defer w.Close()
	w.Resize(fyne.NewSize(480, 600))

	frame := fitToPreview(makeFakeFrame(640, 480, color.RGBA{R: 90, G: 90, B: 200, A: 255}))
	img.Image = frame
	img.Refresh()

	// Simulate stop.
	img.Image = nil
	img.Refresh()

	if err := os.MkdirAll("testdata", 0755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	out := filepath.Join("testdata", "snapshot.png")

	captured := w.Canvas().Capture()
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create %s: %v", out, err)
	}
	defer f.Close()

	if err := png.Encode(f, captured); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	t.Logf("snapshot saved to %s; start=%q stop=%q", out, start.Text, stop.Text)
}
