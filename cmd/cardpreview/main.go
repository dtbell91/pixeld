// cardpreview renders card specs to a PNG sheet (LED-style, scaled) for design checks on a PC.
//
//	cardpreview -out sheet.png -at 2026-10-14T19:07:42+11:00 '{"type":"clock",...}' ...
package main

import (
	"encoding/json"
	"flag"
	"image"
	"image/color"
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
	cell := panel.Size*scale + 20
	img := image.NewRGBA(image.Rect(0, 0, cell*len(specs)+20, cell+20))
	for i := range img.Pix {
		img.Pix[i] = 30
	}
	for n, spec := range specs {
		card, err := cards.New(json.RawMessage(spec), cards.Env{})
		if err != nil {
			log.Fatalf("%s: %v", spec, err)
		}
		f, _ := card.Render(now)
		ox, oy := 20+n*cell, 20
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
	w, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()
	png.Encode(w, img)
}
