// Package panel turns 32x32 RGB frames into the H8630's framebuffer format.
//
// The LED panel (SM16169 column drivers + SM5368 row scan) is bit-banged through the SoC's parallel-RGB
// display output. One framebuffer half is 64x1026 ARGB8888 pixels:
//   - every LED owns one 16-pixel group of a line; its 16 slots carry a 16-bit word per colour, MSB first,
//     on bit 21 (red), bit 13 (green) and bit 5 (blue);
//   - everything else is a constant template (driver config registers and scan/latch signals).
//
// Word = Gamma[channel][v * scale / 255] with Govee's own tables; scale comes from the brightness. The result
// is bit-identical to Govee's libgovee_display (see HARDWARE.md and firmware-build/renderer/).
package panel

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	Size      = 32           // LEDs per side
	FBWidth   = 64           // framebuffer pixels per line
	FBLines   = 1026         // lines per framebuffer half
	HalfWords = FBWidth * FBLines
	dataMask  = 0x00202020
)

// Frame is a 32x32 picture, row-major, 8-bit RGB.
type Frame [Size][Size][3]uint8

//go:embed data/template.fb.gz
var templateGz []byte

//go:embed data/gamma.json
var gammaJSON []byte

//go:embed data/ledmap.json
var ledmapJSON []byte

var (
	template [HalfWords]uint32
	gamma    [3][256]uint16
	slotBase [Size][Size]int // index of the LED's first slot in a half
)

func init() {
	if err := load(); err != nil {
		panic("panel: " + err.Error())
	}
}

func load() error {
	zr, err := gzip.NewReader(bytes.NewReader(templateGz))
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return err
	}
	if len(raw) != HalfWords*4 {
		return fmt.Errorf("template is %d bytes, want %d", len(raw), HalfWords*4)
	}
	for i := range template {
		template[i] = binary.LittleEndian.Uint32(raw[i*4:])
	}

	var g map[string][]int
	if err := json.Unmarshal(gammaJSON, &g); err != nil {
		return err
	}
	for c, name := range []string{"R", "G", "B"} {
		if len(g[name]) != 256 {
			return fmt.Errorf("gamma %s has %d entries", name, len(g[name]))
		}
		for v, w := range g[name] {
			gamma[c][v] = uint16(w)
		}
	}

	var m map[string][2]int
	if err := json.Unmarshal(ledmapJSON, &m); err != nil {
		return err
	}
	if len(m) != Size*Size {
		return fmt.Errorf("ledmap has %d entries", len(m))
	}
	for k, v := range m {
		xy := strings.Split(k, ",")
		x, _ := strconv.Atoi(xy[0])
		y, _ := strconv.Atoi(xy[1])
		slotBase[y][x] = v[0]*FBWidth + v[1]*16
	}
	return nil
}

// Scale is the 8-bit multiplier Govee applies for a brightness percentage (0-100): 100 -> 255, 50 -> 129.
func Scale(brightness int) int {
	if brightness <= 0 {
		return 0
	}
	if brightness > 100 {
		brightness = 100
	}
	return (brightness*255/100*250/255 + 5) & 0xff
}

// Template returns a fresh copy of the constant part of a framebuffer half (all LEDs black).
func Template() []uint32 {
	out := make([]uint32, HalfWords)
	copy(out, template[:])
	return out
}

// Encode writes the frame at the given brightness into dst, which must start as (or be) a template copy.
// Only the LED slots are touched, so dst can be reused frame after frame.
func Encode(dst []uint32, f *Frame, brightness int) {
	s := Scale(brightness)
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			EncodeLED(dst, x, y, f[y][x], s)
		}
	}
}

// EncodeLED writes one LED (for partial updates); scale from Scale().
func EncodeLED(dst []uint32, x, y int, rgb [3]uint8, scale int) {
	r := uint32(gamma[0][int(rgb[0])*scale/255])
	g := uint32(gamma[1][int(rgb[1])*scale/255])
	b := uint32(gamma[2][int(rgb[2])*scale/255])
	slots := dst[slotBase[y][x] : slotBase[y][x]+16]
	for i := range slots {
		bit := uint(15 - i)
		slots[i] = slots[i]&^dataMask | (r>>bit&1)<<21 | (g>>bit&1)<<13 | (b>>bit&1)<<5
	}
}
