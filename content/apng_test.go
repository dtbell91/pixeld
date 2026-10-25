package content

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fixtures come from testdata/make_fixtures.py (Pillow, as Home Assistant encodes): each <name>.png and the
// frames Pillow reads back from it in <name>.rgb.
func TestDecodeAPNGMatchesPillow(t *testing.T) {
	for _, c := range []struct {
		name   string
		delays []time.Duration
	}{
		{"rgb", []time.Duration{50, 75, 120}},
		{"rgba", []time.Duration{80, 80, 80}},
		{"previous", []time.Duration{90, 90, 90}},
		{"palette", []time.Duration{60, 60, 60}},
	} {
		t.Run(c.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", c.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", c.name+".rgb"))
			if err != nil {
				t.Fatal(err)
			}
			frames, err := Decode(data)
			if err != nil {
				t.Fatal(err)
			}
			if len(frames) != len(c.delays) {
				t.Fatalf("got %d frames, want %d", len(frames), len(c.delays))
			}
			for i, f := range frames {
				if f.Duration != c.delays[i]*time.Millisecond {
					t.Errorf("frame %d lasts %v, want %v", i, f.Duration, c.delays[i]*time.Millisecond)
				}
				for y := range f.Pixels {
					for x, got := range f.Pixels[y] {
						o := ((i*32+y)*32 + x) * 3
						if w := [3]uint8{want[o], want[o+1], want[o+2]}; got != w {
							t.Fatalf("frame %d (%d,%d) = %v, want %v", i, x, y, got, w)
						}
					}
				}
			}
		})
	}
}

func TestPlainPNGIsAStill(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	img.Set(3, 4, color.RGBA{10, 20, 30, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if isAPNG(b.Bytes()) {
		t.Fatal("plain PNG taken for an APNG")
	}
	frames, err := Decode(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Duration != 0 || frames[0].Pixels[4][3] != [3]uint8{10, 20, 30} {
		t.Fatalf("got %d frames, first %v", len(frames), frames[0].Duration)
	}
}

// Damaged files must fail cleanly, never panic.
func TestDecodeAPNGTruncated(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rgb.png"))
	if err != nil {
		t.Fatal(err)
	}
	for n := range len(data) {
		Decode(data[:n])
	}
	for i := len(pngSig); i < len(data); i++ {
		bad := append([]byte(nil), data...)
		bad[i] ^= 0xFF
		Decode(bad)
	}
}
