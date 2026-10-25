package content

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/draw"
	"image/png"
	"time"
)

// APNG: a PNG with an acTL chunk. Each frame is an fcTL (size, offset, delay, dispose and blend ops) followed by
// its image data: the IDAT chunks for the first frame (when its fcTL comes before them), fdAT chunks after that.
// Each frame is decoded by wrapping its data in a PNG of its own and composited onto the full-size canvas.

var pngSig = []byte("\x89PNG\r\n\x1a\n")

const (
	apngDisposeNone = iota
	apngDisposeBackground
	apngDisposePrevious
)

const apngBlendSource = 0

const minAPNGDelay = 10 * time.Millisecond

type pngChunk struct {
	typ  string
	data []byte
}

type apngFrame struct {
	w, h, x, y     int
	delay          time.Duration
	dispose, blend byte
	data           [][]byte // IDAT / fdAT payloads (fdAT without its sequence number)
}

// isAPNG reports whether data is a PNG with an animation control chunk before its image data.
func isAPNG(data []byte) bool {
	chunks, err := pngChunks(data)
	if err != nil {
		return false
	}
	for _, c := range chunks {
		switch c.typ {
		case "acTL":
			return true
		case "IDAT":
			return false
		}
	}
	return false
}

func pngChunks(data []byte) ([]pngChunk, error) {
	if !bytes.HasPrefix(data, pngSig) {
		return nil, errors.New("not a PNG")
	}
	var out []pngChunk
	for p := len(pngSig); p < len(data); {
		if len(data)-p < 12 {
			return nil, errors.New("png: truncated chunk")
		}
		n := int(binary.BigEndian.Uint32(data[p:]))
		if n < 0 || n > len(data)-p-12 {
			return nil, errors.New("png: truncated chunk")
		}
		c := pngChunk{typ: string(data[p+4 : p+8]), data: data[p+8 : p+8+n]}
		out = append(out, c)
		p += 12 + n
		if c.typ == "IEND" {
			break
		}
	}
	return out, nil
}

func decodeAPNG(data []byte) ([]Frame, error) {
	chunks, err := pngChunks(data)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 || chunks[0].typ != "IHDR" || len(chunks[0].data) != 13 {
		return nil, errors.New("apng: no IHDR")
	}
	ihdr := chunks[0].data
	width, height := int(binary.BigEndian.Uint32(ihdr[0:])), int(binary.BigEndian.Uint32(ihdr[4:]))
	if width <= 0 || height <= 0 || width > 4096 || height > 4096 {
		return nil, fmt.Errorf("apng: %dx%d not supported", width, height)
	}

	var shared []pngChunk // PLTE, tRNS and the like: needed to decode every frame
	var frames []*apngFrame
	var cur *apngFrame
	seenIDAT := false
	for _, c := range chunks[1:] {
		switch c.typ {
		case "fcTL":
			if len(c.data) != 26 {
				return nil, errors.New("apng: bad fcTL")
			}
			d := c.data
			num, den := binary.BigEndian.Uint16(d[20:]), binary.BigEndian.Uint16(d[22:])
			if den == 0 {
				den = 100
			}
			cur = &apngFrame{
				w: int(binary.BigEndian.Uint32(d[4:])), h: int(binary.BigEndian.Uint32(d[8:])),
				x: int(binary.BigEndian.Uint32(d[12:])), y: int(binary.BigEndian.Uint32(d[16:])),
				delay:   max(minAPNGDelay, time.Duration(num)*time.Second/time.Duration(den)),
				dispose: d[24], blend: d[25],
			}
			if cur.w <= 0 || cur.h <= 0 || cur.x < 0 || cur.y < 0 || cur.x+cur.w > width || cur.y+cur.h > height {
				return nil, errors.New("apng: frame outside the image")
			}
			frames = append(frames, cur)
			if len(frames) > MaxFrames {
				return nil, fmt.Errorf("apng has more than %d frames", MaxFrames)
			}
		case "IDAT":
			seenIDAT = true
			if cur != nil { // else the default image isn't part of the animation
				cur.data = append(cur.data, c.data)
			}
		case "fdAT":
			if cur == nil || len(c.data) < 4 {
				return nil, errors.New("apng: fdAT without fcTL")
			}
			cur.data = append(cur.data, c.data[4:])
		case "acTL", "IEND":
		case "PLTE", "tRNS":
			if !seenIDAT {
				shared = append(shared, c)
			}
		}
	}
	if len(frames) == 0 {
		return nil, errors.New("apng has no frames")
	}

	bounds := image.Rect(0, 0, width, height)
	canvas := image.NewRGBA(bounds)
	out := make([]Frame, 0, len(frames))
	for i, f := range frames {
		if len(f.data) == 0 {
			return nil, fmt.Errorf("apng: frame %d has no data", i)
		}
		img, err := png.Decode(bytes.NewReader(framePNG(ihdr, f, shared)))
		if err != nil {
			return nil, fmt.Errorf("apng frame %d: %w", i, err)
		}
		r := image.Rect(f.x, f.y, f.x+f.w, f.y+f.h)
		dispose := f.dispose
		if i == 0 && dispose == apngDisposePrevious {
			dispose = apngDisposeBackground
		}
		var restore *image.RGBA
		if dispose == apngDisposePrevious {
			restore = image.NewRGBA(bounds)
			copy(restore.Pix, canvas.Pix)
		}
		op := draw.Over
		if f.blend == apngBlendSource {
			op = draw.Src
		}
		draw.Draw(canvas, r, img, img.Bounds().Min, op)
		out = append(out, Frame{Pixels: toFrame(canvas), Duration: f.delay})
		switch dispose {
		case apngDisposeBackground:
			draw.Draw(canvas, r, image.Transparent, image.Point{}, draw.Src)
		case apngDisposePrevious:
			canvas = restore
		}
	}
	if len(out) == 1 {
		out[0].Duration = 0
	}
	return out, nil
}

// framePNG wraps one frame's image data in a standalone PNG.
func framePNG(ihdr []byte, f *apngFrame, shared []pngChunk) []byte {
	var b bytes.Buffer
	b.Write(pngSig)
	hdr := append([]byte(nil), ihdr...)
	binary.BigEndian.PutUint32(hdr[0:], uint32(f.w))
	binary.BigEndian.PutUint32(hdr[4:], uint32(f.h))
	writeChunk(&b, "IHDR", hdr)
	for _, c := range shared {
		writeChunk(&b, c.typ, c.data)
	}
	writeChunk(&b, "IDAT", bytes.Join(f.data, nil))
	writeChunk(&b, "IEND", nil)
	return b.Bytes()
}

func writeChunk(b *bytes.Buffer, typ string, data []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	b.Write(n[:])
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	b.WriteString(typ)
	b.Write(data)
	binary.BigEndian.PutUint32(n[:], crc.Sum32())
	b.Write(n[:])
}
