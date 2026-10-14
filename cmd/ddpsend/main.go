// ddpsend streams frames to a DDP display (pixeld on UDP 4048): a GIF, or a built-in demo.
//
//	ddpsend -host <light> -gif party.gif [-fps 30] [-seconds 20]
//	ddpsend -host <light> -demo plasma|fire|life
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"time"

	"pixeld/content"
	"pixeld/panel"
)

// chunk: bytes of pixel data per packet. 1200 keeps packets under a 1280 MTU (e.g. WSL2), which matters:
// fragmented UDP is often dropped on such paths.
var chunk = flag.Int("chunk", 1200, "pixel bytes per packet (multiple of 3)")

// send splits the frame into packets; push on the last.
func send(conn net.Conn, seq *byte, f *panel.Frame) error {
	data := make([]byte, 0, panel.Size*panel.Size*3)
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			data = append(data, f[y][x][:]...)
		}
	}
	*seq = (*seq)%15 + 1
	size := max(3, *chunk/3*3)
	for off := 0; off < len(data); off += size {
		chunk := data[off:min(off+size, len(data))]
		pkt := make([]byte, 10, 10+len(chunk))
		pkt[0], pkt[1], pkt[2], pkt[3] = 0x40, *seq, 0x0b, 1 // v1, seq, RGB 8-bit, display
		if off+len(chunk) == len(data) {
			pkt[0] |= 0x01 // push
		}
		binary.BigEndian.PutUint32(pkt[4:], uint32(off))
		binary.BigEndian.PutUint16(pkt[8:], uint16(len(chunk)))
		if _, err := conn.Write(append(pkt, chunk...)); err != nil {
			return err
		}
	}
	return nil
}

func plasma(t float64) *panel.Frame {
	f := new(panel.Frame)
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			v := math.Sin(float64(x)/4+t*2) + math.Sin(float64(y)/5-t*1.5) + math.Sin(float64(x+y)/6+t)
			f[y][x] = [3]uint8{uint8(127 + 120*math.Sin(v)), uint8(127 + 120*math.Sin(v+2.1)), uint8(127 + 120*math.Sin(v+4.2))}
		}
	}
	return f
}

type fire struct{ heat [panel.Size + 2][panel.Size]float64 }

func (s *fire) frame() *panel.Frame {
	for x := 0; x < panel.Size; x++ {
		s.heat[panel.Size+1][x] = rand.Float64()
		s.heat[panel.Size][x] = rand.Float64()
	}
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			l, r := s.heat[y+1][max(0, x-1)], s.heat[y+1][min(panel.Size-1, x+1)]
			s.heat[y][x] = max(0, (s.heat[y+1][x]+s.heat[y+2][x]+l+r)/4-0.022) // cool as it rises: black at the top
		}
	}
	f := new(panel.Frame)
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			h := math.Min(1, s.heat[y][x]*1.6)
			f[y][x] = [3]uint8{uint8(255 * math.Min(1, h*1.5)), uint8(255 * math.Max(0, h*1.5-0.5)), uint8(255 * math.Max(0, h*3-2.2))}
		}
	}
	return f
}

type life struct{ cells [panel.Size][panel.Size]bool }

func (s *life) frame() *panel.Frame {
	alive := 0
	var next [panel.Size][panel.Size]bool
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			n := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if (dx != 0 || dy != 0) && s.cells[(y+dy+panel.Size)%panel.Size][(x+dx+panel.Size)%panel.Size] {
						n++
					}
				}
			}
			next[y][x] = n == 3 || (n == 2 && s.cells[y][x])
			if next[y][x] {
				alive++
			}
		}
	}
	if alive < 20 { // reseed when it dies out
		for i := 0; i < 300; i++ {
			next[rand.Intn(panel.Size)][rand.Intn(panel.Size)] = true
		}
	}
	s.cells = next
	f := new(panel.Frame)
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			if s.cells[y][x] {
				f[y][x] = [3]uint8{60, 255, 120}
			}
		}
	}
	return f
}

func main() {
	host := flag.String("host", "", "light address")
	gifPath := flag.String("gif", "", "GIF (or PNG/JPEG) to stream")
	demo := flag.String("demo", "plasma", "plasma, fire or life (when no -gif)")
	fps := flag.Float64("fps", 30, "frames per second (demos; GIFs use their own timing)")
	seconds := flag.Float64("seconds", 15, "how long to stream")
	flag.Parse()
	if *host == "" {
		log.Fatal("-host is required")
	}
	conn, err := net.Dial("udp", fmt.Sprintf("%s:4048", *host))
	if err != nil {
		log.Fatal(err)
	}
	var seq byte
	end := time.Now().Add(time.Duration(*seconds * float64(time.Second)))
	if *gifPath != "" {
		data, err := os.ReadFile(*gifPath)
		if err != nil {
			log.Fatal(err)
		}
		frames, err := content.Decode(data)
		if err != nil {
			log.Fatal(err)
		}
		for i := 0; time.Now().Before(end); i = (i + 1) % len(frames) {
			if err := send(conn, &seq, &frames[i].Pixels); err != nil {
				log.Fatal(err)
			}
			d := frames[i].Duration
			if d == 0 {
				d = 100 * time.Millisecond
			}
			time.Sleep(d)
		}
		return
	}
	step := time.Duration(float64(time.Second) / *fps)
	start := time.Now()
	f, l := &fire{}, &life{}
	for tick := time.NewTicker(step); time.Now().Before(end); <-tick.C {
		var frame *panel.Frame
		switch *demo {
		case "fire":
			frame = f.frame()
		case "life":
			frame = l.frame()
		default:
			frame = plasma(time.Since(start).Seconds())
		}
		if err := send(conn, &seq, frame); err != nil {
			log.Fatal(err)
		}
	}
}
