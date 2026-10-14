// panelshot: what the LED panel is showing right now, decoded from /dev/fb0 (works under pixeld and under
// Govee's govee_app). Writes a PNG to stdout: one frame, or a contact sheet of -n frames -every apart.
//
//	panelshot [-n 1] [-every 250ms] [-scale 8] > shot.png
package main

import (
	"bufio"
	"flag"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"pixeld/panel"
)

func shownHalf(mem []uint32) []uint32 {
	y := 0
	if b, err := os.ReadFile("/sys/class/graphics/fb0/pan"); err == nil {
		if _, s, ok := strings.Cut(strings.TrimSpace(string(b)), ","); ok {
			y, _ = strconv.Atoi(s)
		}
	}
	half := 0
	if y >= panel.FBLines {
		half = 1
	}
	out := make([]uint32, panel.HalfWords)
	copy(out, mem[half*panel.HalfWords:(half+1)*panel.HalfWords])
	return out
}

func main() {
	n := flag.Int("n", 1, "frames")
	every := flag.Duration("every", 250*time.Millisecond, "interval between frames")
	scale := flag.Int("scale", 8, "pixels per LED")
	flag.Parse()
	fd, err := syscall.Open("/dev/fb0", syscall.O_RDONLY, 0)
	if err != nil {
		log.Fatal(err)
	}
	b, err := syscall.Mmap(fd, 0, 2*panel.HalfWords*4, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		log.Fatal(err)
	}
	mem := unsafe.Slice((*uint32)(unsafe.Pointer(&b[0])), 2*panel.HalfWords)
	cols := min(*n, 6)
	rows := (*n + cols - 1) / cols
	cell := panel.Size**scale + 4
	img := image.NewRGBA(image.Rect(0, 0, cols*cell+4, rows*cell+4))
	for i := range img.Pix {
		img.Pix[i] = 40
	}
	for k := 0; k < *n; k++ {
		if k > 0 {
			time.Sleep(*every)
		}
		f := panel.Decode(shownHalf(mem))
		ox, oy := 4+(k%cols)*cell, 4+(k/cols)*cell
		for y := 0; y < panel.Size**scale; y++ {
			for x := 0; x < panel.Size**scale; x++ {
				c := f[y / *scale][x / *scale]
				if *scale >= 4 && (x%*scale == 0 || y%*scale == 0) {
					continue // LED grid
				}
				img.Set(ox+x, oy+y, color.RGBA{c[0], c[1], c[2], 255})
			}
		}
	}
	w := bufio.NewWriter(os.Stdout)
	png.Encode(w, img)
	w.Flush()
}
