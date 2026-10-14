// Package cards draws dashboard cards on the light itself (things that must keep time without Home Assistant).
package cards

import (
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
