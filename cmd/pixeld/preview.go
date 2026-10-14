package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"sync"

	"pixeld/panel"
)

// preview is the -sim sink: the latest frame as a scaled PNG, viewable in a browser.
type preview struct {
	mu  sync.Mutex
	png []byte
	seq int
}

const previewScale = 12

func newPreview(addr string) *preview {
	p := &preview{}
	mux := http.NewServeMux()
	mux.HandleFunc("/frame.png", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		b := p.png
		p.mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!doctype html><title>pixeld preview</title><body style="background:#111;margin:0;display:grid;place-items:center;height:100vh">
<img id=f src="/frame.png" style="image-rendering:pixelated;width:384px;height:384px">
<script>setInterval(()=>{document.getElementById('f').src='/frame.png?'+Date.now()},50)</script>`))
	})
	go func() { log.Printf("preview: http://%s/", addr); log.Print(http.ListenAndServe(addr, mux)) }()
	return p
}

// Show renders with brightness applied linearly (the panel's own curve is in the encoder).
func (p *preview) Show(f *panel.Frame, brightness int) error {
	s := panel.Scale(brightness)
	img := image.NewRGBA(image.Rect(0, 0, panel.Size*previewScale, panel.Size*previewScale))
	for y := 0; y < panel.Size*previewScale; y++ {
		for x := 0; x < panel.Size*previewScale; x++ {
			c := f[y/previewScale][x/previewScale]
			if x%previewScale == 0 || y%previewScale == 0 { // LED grid
				img.Set(x, y, color.RGBA{8, 8, 8, 255})
				continue
			}
			img.Set(x, y, color.RGBA{uint8(int(c[0]) * s / 255), uint8(int(c[1]) * s / 255), uint8(int(c[2]) * s / 255), 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	p.mu.Lock()
	p.png = buf.Bytes()
	p.seq++
	p.mu.Unlock()
	return nil
}
