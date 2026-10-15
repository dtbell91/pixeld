// Package cards draws dashboard cards on the light itself (things that must keep time without Home Assistant).
package cards

import (
	"strings"
	"unicode"

	"pixeld/panel"
)

type RGB = [3]uint8

func set(f *panel.Frame, x, y int, c RGB) {
	if x >= 0 && y >= 0 && x < panel.Size && y < panel.Size {
		f[y][x] = c
	}
}

func glyph(r rune) uint16 {
	if g, ok := font3x5[unicode.ToUpper(r)]; ok {
		return g
	}
	return font3x5['?']
}

// Text draws s with its top-left at (x, y) using the 3x5 font scaled by scale; returns the x after it.
func Text(f *panel.Frame, s string, x, y, scale int, c RGB) int {
	for _, r := range s {
		g := glyph(r)
		for k := 0; k < 15; k++ {
			if g>>(14-k)&1 == 0 {
				continue
			}
			gx, gy := k%3, k/3
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					set(f, x+gx*scale+dx, y+gy*scale+dy, c)
				}
			}
		}
		x += 4 * scale
	}
	return x
}

// TextWidth is the drawn width of s (no trailing gap).
func TextWidth(s string, scale int) int {
	n := len([]rune(s))
	if n == 0 {
		return 0
	}
	return n*4*scale - scale
}

func dim(c RGB, f float64) RGB {
	return RGB{uint8(float64(c[0]) * f), uint8(float64(c[1]) * f), uint8(float64(c[2]) * f)}
}

// Printable reduces s to what the 3x5 font can draw: capitals, with unknown characters dropped and runs
// of spaces collapsed.
func Printable(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToUpper(s) {
		if unicode.IsSpace(r) {
			r = ' '
		} else if _, ok := font3x5[r]; !ok {
			continue
		}
		if r == ' ' && space {
			continue
		}
		space = r == ' '
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// Fill paints the rectangle w×h at (x, y).
func Fill(f *panel.Frame, x, y, w, h int, c RGB) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			set(f, i, j, c)
		}
	}
}

// Dim scales a colour's brightness by f (0..1).
func Dim(c RGB, f float64) RGB { return dim(c, f) }
