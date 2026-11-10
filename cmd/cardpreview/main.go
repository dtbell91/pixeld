// cardpreview renders card specs to a PNG sheet (LED-style, scaled) for design checks on a PC.
//
//	cardpreview -out sheet.png -at 2026-10-14T19:07:42+11:00 '{"type":"clock",...}' ...
//
// With -frames n, each card is drawn n times, -step apart (one row per card), to check animated cards; with
// -gif, as an animated GIF instead (one card).
package main

import (
	"encoding/json"
	"flag"
	"image"
	"image/color"
	"image/color/palette"
	stddraw "image/draw"
	"image/gif"
	"image/png"
	"log"
	"os"
	"time"

	"pixeld/cards"
	"pixeld/panel"
)

const scale = 10

func main() {
	out := flag.String("out", "cards.png", "output PNG")
	at := flag.String("at", "", "render time (RFC3339; default now)")
	frames := flag.Int("frames", 1, "frames per card")
	step := flag.Duration("step", 500*time.Millisecond, "time between frames")
	gifOut := flag.String("gif", "", "write the first card as an animated GIF here (-frames, -step)")
	flag.Parse()
	now := time.Now()
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			log.Fatal(err)
		}
		now = t
	}
	specs := flag.Args()
	if *gifOut != "" {
		writeGIF(*gifOut, specs[0], now, *frames, *step)
		return
	}
	cell := panel.Size*scale + 20
	img := image.NewRGBA(image.Rect(0, 0, cell*max(len(specs), *frames)+20, cell*min(len(specs), *frames)+20))
	if *frames > 1 {
		img = image.NewRGBA(image.Rect(0, 0, cell**frames+20, cell*len(specs)+20))
	}
	for i := range img.Pix {
		img.Pix[i] = 30
	}
	for n, spec := range specs {
		card, err := cards.New(json.RawMessage(spec), cards.Env{})
		if err != nil {
			log.Fatalf("%s: %v", spec, err)
		}
		for k := 0; k < *frames; k++ {
			f, _ := card.Render(now.Add(time.Duration(k) * *step))
			ox, oy := 20+n*cell, 20
			if *frames > 1 {
				ox, oy = 20+k*cell, 20+n*cell
			}
			draw(img, f, ox, oy)
		}
	}
	w, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()
	png.Encode(w, img)
}

func draw(img *image.RGBA, f *panel.Frame, ox, oy int) {
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			c := f[y][x]
			for dy := 1; dy < scale-1; dy++ {
				for dx := 1; dx < scale-1; dx++ {
					img.Set(ox+x*scale+dx, oy+y*scale+dy, color.RGBA{c[0], c[1], c[2], 255})
				}
			}
		}
	}
}

func writeGIF(out, spec string, now time.Time, frames int, step time.Duration) {
	card, err := cards.New(json.RawMessage(spec), cards.Env{})
	if err != nil {
		log.Fatalf("%s: %v", spec, err)
	}
	g := &gif.GIF{}
	for k := 0; k < frames; k++ {
		f, _ := card.Render(now.Add(time.Duration(k) * step))
		img := image.NewRGBA(image.Rect(0, 0, panel.Size*scale, panel.Size*scale))
		for i := range img.Pix {
			img.Pix[i] = 30
		}
		draw(img, f, 0, 0)
		p := image.NewPaletted(img.Bounds(), nil)
		p.Palette = palette.Plan9
		stddraw.FloydSteinberg.Draw(p, img.Bounds(), img, image.Point{})
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, int(step/(10*time.Millisecond)))
	}
	w, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()
	if err := gif.EncodeAll(w, g); err != nil {
		log.Fatal(err)
	}
}
