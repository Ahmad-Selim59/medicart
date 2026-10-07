package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func TestResolveDependencyCLIUsesAppBaseDir(t *testing.T) {
	tmp := t.TempDir()
	deps := filepath.Join(tmp, dependenciesDir)
	if err := os.MkdirAll(deps, 0o755); err != nil {
		t.Fatalf("mkdir dependencies: %v", err)
	}
	exePath := filepath.Join(deps, "camera_cli.exe")
	if err := os.WriteFile(exePath, []byte("fake"), 0o755); err != nil {
		t.Fatalf("write fake exe: %v", err)
	}

	orig := appBaseDir
	appBaseDir = func() string { return tmp }
	t.Cleanup(func() { appBaseDir = orig })

	got := resolveDependencyCLI("camera_cli.exe")
	want, err := filepath.Abs(exePath)
	if err != nil {
		t.Fatalf("abs exe path: %v", err)
	}
	if got != want {
		t.Fatalf("resolveDependencyCLI() = %q, want %q", got, want)
	}
}

func makeFakeFrame(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

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

func TestPreviewImageNeverNilWhenVisible(t *testing.T) {
	test.NewTempApp(t)

	_, _, img, root := buildPreviewSection()
	w := test.NewWindow(root)
	defer w.Close()

	if img.Image == nil && img.Resource == nil && img.File == "" && img.Visible() {
		t.Fatal("preview image is nil/empty but visible — will trigger fyne #4345 texture-cache thrashing on macOS GLFW")
	}

	img.Image = blankFrame
	img.Refresh()
	if img.Image == nil {
		t.Fatal("after simulated stop, preview Image is nil — will re-trigger #4345")
	}
}

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

	for i := 0; i < 5; i++ {
		frame := fitToPreview(makeFakeFrame(640, 480, color.RGBA{R: uint8(i * 40), G: 100, B: 200, A: 255}))
		img.Image = frame
		img.Refresh()
		checkText("frame-" + string(rune('0'+i)))
	}

	img.Image = nil
	img.Refresh()
	checkText("after-stop")

	frame := fitToPreview(makeFakeFrame(640, 480, color.RGBA{R: 50, G: 200, B: 50, A: 255}))
	img.Image = frame
	img.Refresh()
	checkText("after-restart")
}

func TestPreviewImageDoesNotExpandLayout(t *testing.T) {
	test.NewTempApp(t)

	_, _, img, root := buildPreviewSection()
	w := test.NewWindow(root)
	defer w.Close()
	w.Resize(fyne.NewSize(480, 600))

	initialMin := root.MinSize()

	bigFrame := fitToPreview(makeFakeFrame(1920, 1080, color.RGBA{R: 200, G: 200, B: 200, A: 255}))
	img.Image = bigFrame
	img.Refresh()

	afterMin := root.MinSize()

	if afterMin.Width > initialMin.Width+1 {
		t.Errorf("layout min width grew: was %.1f, now %.1f", initialMin.Width, afterMin.Width)
	}
	if afterMin.Height > initialMin.Height+1 {
		t.Errorf("layout min height grew: was %.1f, now %.1f", initialMin.Height, afterMin.Height)
	}
}

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
		want := float64(c.w) / float64(c.h)
		got2 := float64(b.Dx()) / float64(b.Dy())
		if diff := want - got2; diff > 0.02 || diff < -0.02 {
			t.Errorf("fitToPreview(%dx%d) aspect %.3f != source %.3f",
				c.w, c.h, got2, want)
		}
	}
}

func TestRenderSnapshot(t *testing.T) {
	test.NewTempApp(t)

	start, stop, img, root := buildPreviewSection()
	w := test.NewWindow(root)
	defer w.Close()
	w.Resize(fyne.NewSize(480, 600))

	frame := fitToPreview(makeFakeFrame(640, 480, color.RGBA{R: 90, G: 90, B: 200, A: 255}))
	img.Image = frame
	img.Refresh()

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

func TestCoalesceLatestFrame(t *testing.T) {
	ch := make(chan []byte, 4)
	ch <- []byte("old")
	ch <- []byte("mid")
	ch <- []byte("new")

	got, closed := coalesceLatestFrame(ch, <-ch)
	if closed {
		t.Fatal("expected channel to remain open")
	}
	if string(got) != "new" {
		t.Fatalf("got %q, want %q", got, "new")
	}

	ch2 := make(chan []byte, 2)
	ch2 <- []byte("last")
	close(ch2)
	got, closed = coalesceLatestFrame(ch2, <-ch2)
	if !closed {
		t.Fatal("expected channel closed")
	}
	if string(got) != "last" {
		t.Fatalf("got %q, want %q", got, "last")
	}
}

func TestBuildFFmpegArgsForMJPEGPipe(t *testing.T) {
	args := buildFFmpegArgsForMJPEGPipe(`video="Cam"`, avConfig{})
	joined := strings.Join(args, " ")

	for _, want := range []string{"-flush_packets", "1", "-vsync", "passthrough"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}

	switch runtime.GOOS {
	case "windows":
		for _, want := range []string{
			"-fflags", "+nobuffer",
			"-flags", "low_delay",
			"-video_size", captureVideoSize,
			"-framerate", captureFramerate,
			"-f", "dshow",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("windows args missing %q: %s", want, joined)
			}
		}
	case "darwin":
		if !strings.Contains(joined, "avfoundation") {
			t.Fatalf("darwin args missing avfoundation: %s", joined)
		}
	}
}
