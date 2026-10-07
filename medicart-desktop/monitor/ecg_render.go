package monitor

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// RenderECGStripPNG draws a simple ECG trace on white background.
func RenderECGStripPNG(samples []float64, width, height int) ([]byte, error) {
	if width <= 0 {
		width = 800
	}
	if height <= 0 {
		height = 240
	}
	if len(samples) < 2 {
		return nil, fmt.Errorf("not enough samples to render")
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	white := color.RGBA{255, 255, 255, 255}
	grid := color.RGBA{230, 230, 230, 255}
	trace := color.RGBA{20, 20, 20, 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, white)
		}
	}
	for x := 0; x < width; x += 20 {
		for y := 0; y < height; y++ {
			img.Set(x, y, grid)
		}
	}
	for y := 0; y < height; y += 20 {
		for x := 0; x < width; x++ {
			img.Set(x, y, grid)
		}
	}

	minV, maxV := samples[0], samples[0]
	for _, v := range samples {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	span := maxV - minV
	if span < 1e-6 {
		span = 1
	}
	margin := 16.0
	plotH := float64(height) - 2*margin

	n := len(samples)
	prevX, prevY := -1, -1
	for i, v := range samples {
		x := int(float64(i) / float64(n-1) * float64(width-1))
		norm := (v - minV) / span
		y := int(margin + (1-norm)*plotH)
		if y < 0 {
			y = 0
		}
		if y >= height {
			y = height - 1
		}
		if prevX >= 0 {
			drawLine(img, prevX, prevY, x, y, trace)
		}
		prevX, prevY = x, y
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	dx := abs(x1 - x0)
	dy := -abs(y1 - y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		if image.Pt(x0, y0).In(img.Bounds()) {
			img.Set(x0, y0, c)
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
