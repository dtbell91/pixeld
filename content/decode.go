// Package content turns image files (PNG, APNG, JPEG, GIF) into 32x32 frames with display durations.
package content

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"time"

	"pixeld/panel"
)

// Frame is one picture of an item and how long it stays up.
type Frame struct {
	Pixels   panel.Frame
	Duration time.Duration
}

const (
	MaxFrames      = 600
	minGIFDelay    = 20 * time.Millisecond // browsers treat 0-10 ms GIF delays as 100 ms; we clamp to 20 ms
	defaultGIFStep = 100 * time.Millisecond
)

// Formats lists what Decode reads, for Home Assistant to pick from (APNG: full-colour animation).
var Formats = []string{"png", "apng", "jpeg", "gif"}

// Decode reads PNG, APNG, JPEG or GIF bytes. Still images give one frame with Duration 0 (shown until replaced).
func Decode(data []byte) ([]Frame, error) {
	if bytes.HasPrefix(data, []byte("GIF8")) {
		return decodeGIF(data)
	}
	if isAPNG(data) {
		return decodeAPNG(data)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return []Frame{{Pixels: toFrame(img)}}, nil
}

func decodeGIF(data []byte) ([]Frame, error) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if len(g.Image) == 0 {
		return nil, errors.New("gif has no frames")
	}
	if len(g.Image) > MaxFrames {
		return nil, fmt.Errorf("gif has %d frames (max %d)", len(g.Image), MaxFrames)
	}
	bounds := image.Rect(0, 0, g.Config.Width, g.Config.Height)
	if bounds.Empty() {
		bounds = g.Image[0].Bounds()
	}
	canvas := image.NewRGBA(bounds)
	out := make([]Frame, 0, len(g.Image))
	for i, fr := range g.Image {
		var restore *image.RGBA
		if i < len(g.Disposal) && g.Disposal[i] == gif.DisposalPrevious {
			restore = image.NewRGBA(bounds)
			copy(restore.Pix, canvas.Pix)
		}
		draw.Draw(canvas, fr.Bounds(), fr, fr.Bounds().Min, draw.Over)
		d := defaultGIFStep
		if i < len(g.Delay) {
			d = time.Duration(g.Delay[i]) * 10 * time.Millisecond
			if d < minGIFDelay {
				d = defaultGIFStep
			}
		}
		out = append(out, Frame{Pixels: toFrame(canvas), Duration: d})
		if i < len(g.Disposal) {
			switch g.Disposal[i] {
			case gif.DisposalBackground:
				draw.Draw(canvas, fr.Bounds(), image.Transparent, image.Point{}, draw.Src)
			case gif.DisposalPrevious:
				canvas = restore
			}
		}
	}
	if len(out) == 1 {
		out[0].Duration = 0
	}
	return out, nil
}

// toFrame scales (area average) to 32x32 and composites transparency onto black.
func toFrame(img image.Image) panel.Frame {
	var f panel.Frame
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	for y := 0; y < panel.Size; y++ {
		y0, y1 := b.Min.Y+y*h/panel.Size, b.Min.Y+(y+1)*h/panel.Size
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < panel.Size; x++ {
			x0, x1 := b.Min.X+x*w/panel.Size, b.Min.X+(x+1)*w/panel.Size
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n uint32
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					c := color.NRGBAModel.Convert(img.At(xx, yy)).(color.NRGBA)
					a := uint32(c.A)
					r += uint32(c.R) * a / 255
					g += uint32(c.G) * a / 255
					bl += uint32(c.B) * a / 255
					n++
				}
			}
			f[y][x] = [3]uint8{uint8(r / n), uint8(g / n), uint8(bl / n)}
		}
	}
	return f
}

// Solid returns a single-colour frame.
func Solid(r, g, b uint8) panel.Frame {
	var f panel.Frame
	for y := range f {
		for x := range f[y] {
			f[y][x] = [3]uint8{r, g, b}
		}
	}
	return f
}
