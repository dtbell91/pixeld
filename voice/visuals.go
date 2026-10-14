package voice

import (
	"context"
	"math"
	"time"

	"pixeld/cards"
	"pixeld/panel"
)

// Show animates the panel while the satellite is active (listening, thinking, speaking, error, timer),
// sending frames to show (e.g. player.Stream) at 20 fps; nothing is sent while idle.
func Show(ctx context.Context, phase func() Phase, show func(*panel.Frame)) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	var since time.Time
	last := Idle
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			p := phase()
			if p != last {
				since, last = now, p
			}
			if p == Idle {
				continue
			}
			show(frame(p, now.Sub(since).Seconds()))
		}
	}
}

func frame(p Phase, t float64) *panel.Frame {
	f := new(panel.Frame)
	switch p {
	case Listening: // a breathing ring
		r := 10 + 2*math.Sin(t*5)
		ring(f, r, cards.RGB{0, 170, 255})
		ring(f, r-1.2, cards.RGB{0, 60, 120})
	case Thinking: // dots chasing round the circle
		for i := 0; i < 8; i++ {
			a := float64(i)/8*2*math.Pi + t*4
			b := 0.15 + 0.85*math.Mod(float64(8-i)/8+0.0, 1)
			dot(f, 16+9*math.Cos(a), 16+9*math.Sin(a), cards.RGB{uint8(170 * b), uint8(80 * b), uint8(255 * b)})
		}
	case Speaking: // bars like a level meter
		for x := 4; x < 28; x += 3 {
			h := int(4 + 9*(0.5+0.5*math.Sin(t*9+float64(x)*0.7)*math.Sin(t*3.1+float64(x))))
			for y := 16 - h; y <= 16+h; y++ {
				c := cards.RGB{40, 220, 140}
				f[y][x], f[y][x+1] = c, c
			}
		}
	case Error:
		if int(t*4)%2 == 0 {
			ring(f, 11, cards.RGB{255, 30, 30})
			for i := -5; i <= 5; i++ {
				f[16+i][16+i], f[16+i][16-i] = cards.RGB{255, 30, 30}, cards.RGB{255, 30, 30}
			}
		}
	case TimerRinging:
		if int(t*3)%2 == 0 {
			ring(f, 12, cards.RGB{255, 160, 0})
			for y := 9; y <= 16; y++ {
				f[y][16] = cards.RGB{255, 220, 120}
			}
			for x := 16; x <= 21; x++ {
				f[16][x] = cards.RGB{255, 220, 120}
			}
		}
	}
	return f
}

func ring(f *panel.Frame, r float64, c cards.RGB) {
	for a := 0.0; a < 2*math.Pi; a += 0.02 {
		dot(f, 15.5+r*math.Cos(a), 15.5+r*math.Sin(a), c)
	}
}

func dot(f *panel.Frame, x, y float64, c cards.RGB) {
	xi, yi := int(math.Round(x)), int(math.Round(y))
	if xi >= 0 && yi >= 0 && xi < panel.Size && yi < panel.Size {
		f[yi][xi] = c
	}
}
