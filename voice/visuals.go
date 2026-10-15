package voice

import (
	"context"
	"fmt"
	"math"
	"time"

	"pixeld/cards"
	"pixeld/panel"
)

const (
	noticeFor   = 1500 * time.Millisecond
	scrollSpeed = 24 // px/s
)

type noticeKind int

const (
	noticeNone   noticeKind = iota
	noticeVolume            // speaker volume (level) or mute (!on)
	noticeMic               // microphone on / off (privacy)
)

// notice is a brief on-panel confirmation of a setting change.
type notice struct {
	kind  noticeKind
	level float64
	on    bool
	at    time.Time
}

// noticeActive (s.mu held) reports whether a notice is on the panel at now.
func (s *Satellite) noticeActive(now time.Time) bool {
	return s.notice.kind != noticeNone && now.Before(s.notice.at.Add(noticeFor))
}

// noticeUntil (s.mu held) is when the current notice ends (zero: none).
func (s *Satellite) noticeUntil() time.Time {
	if s.notice.kind == noticeNone {
		return time.Time{}
	}
	return s.notice.at.Add(noticeFor)
}

// view is what the satellite wants on the panel.
type view struct {
	phase       Phase
	text        string // what was heard (thinking), what's said (speaking), which timer (ringing)
	notice      notice // kind noticeNone: none
	afterSpeech bool   // the reply's text staying up after the speech, until it has been read
}

func (v view) idle() bool { return v.phase == Idle && v.notice.kind == noticeNone }

// readTime is how long the reply text needs on the panel: one full pass of the scroll (or 2 s if it fits).
func readTime(text string) time.Duration {
	w := cards.TextWidth(text, 1)
	if w <= panel.Size {
		return 2 * time.Second
	}
	return time.Duration((0.8 + float64(w+2)/scrollSpeed) * float64(time.Second))
}

func (s *Satellite) view(now time.Time) view {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := view{phase: s.phase}
	switch s.phase {
	case Thinking:
		v.text = s.heard
	case Speaking:
		v.text = s.reply
	case TimerRinging:
		v.text = s.ringName
	}
	if s.noticeActive(now) {
		v.notice = s.notice
	}
	return v
}

// Show animates the panel while the satellite has something to show (listening, thinking, speaking, error,
// a ringing timer, a notice), sending frames to show (e.g. player.Stream) at 20 fps, and calls done when
// it's over so the panel goes straight back to what it was showing.
func Show(ctx context.Context, s *Satellite, show func(*panel.Frame), done func()) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	var since, lingerUntil time.Time
	var last view
	var linger *view // the reply text, kept up after the speech ends until it has been read
	drawing := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			v := s.view(now)
			if v != last {
				if last.phase == Speaking && last.text != "" && v.idle() {
					if until := since.Add(readTime(last.text)); now.Before(until) {
						l := last
						l.afterSpeech = true
						linger, lingerUntil = &l, until // since stays: the text carries on where it was
					}
				}
				if !v.idle() {
					linger = nil
				}
				if linger == nil {
					since = now
				}
				last = v
			}
			cur := v
			if v.idle() {
				if linger == nil || !now.Before(lingerUntil) {
					linger = nil
					if drawing {
						drawing = false
						s.visualsBusy.Store(false)
						done()
					}
					continue
				}
				cur = *linger
			}
			drawing = true
			s.visualsBusy.Store(true)
			show(frame(cur, now.Sub(since).Seconds()))
		}
	}
}

func frame(v view, t float64) *panel.Frame {
	f := new(panel.Frame)
	if v.notice.kind != noticeNone {
		drawNotice(f, v.notice)
		return f
	}
	switch v.phase {
	case Listening: // a breathing ring
		r := 10 + 2*math.Sin(t*5)
		ring(f, r, cards.RGB{0, 170, 255})
		ring(f, r-1.2, cards.RGB{0, 60, 120})
	case Thinking: // dots chasing round the circle, what was heard along the bottom
		for i := 0; i < 8; i++ {
			a := float64(i)/8*2*math.Pi + t*4
			b := 0.15 + 0.85*math.Mod(float64(8-i)/8+0.0, 1)
			dot(f, 16+9*math.Cos(a), 16+9*math.Sin(a), cards.RGB{uint8(170 * b), uint8(80 * b), uint8(255 * b)})
		}
		if v.text != "" {
			scrollText(f, v.text, 27, t, cards.RGB{150, 110, 220})
		}
	case Speaking: // bars like a level meter; with the reply scrolling through the middle
		if v.afterSpeech {
			scrollText(f, v.text, 13, t, cards.RGB{230, 240, 235})
			break
		}
		if v.text == "" {
			meter(f, t, 16, 13)
			break
		}
		meter(f, t, 6, 4)
		meter(f, t, 25, 4)
		scrollText(f, v.text, 13, t, cards.RGB{230, 240, 235})
	case Error:
		if int(t*4)%2 == 0 {
			ring(f, 11, cards.RGB{255, 30, 30})
			for i := -5; i <= 5; i++ {
				f[16+i][16+i], f[16+i][16-i] = cards.RGB{255, 30, 30}, cards.RGB{255, 30, 30}
			}
		}
	case TimerRinging: // flashing ring round the timer's name (or a clock face)
		if int(t*3)%2 != 0 {
			break
		}
		ring(f, 12, timerColor)
		if v.text != "" {
			scrollTextIn(f, v.text, 13, t, 5, 26, cards.RGB{255, 220, 120})
			break
		}
		for y := 9; y <= 16; y++ {
			f[y][16] = cards.RGB{255, 220, 120}
		}
		for x := 16; x <= 21; x++ {
			f[16][x] = cards.RGB{255, 220, 120}
		}
	}
	return f
}

// meter: level-meter bars centred on row cy, up to h rows either side.
func meter(f *panel.Frame, t float64, cy, h int) {
	for x := 4; x < 28; x += 3 {
		n := int(float64(h) * (0.3 + 0.7*(0.5+0.5*math.Sin(t*9+float64(x)*0.7)*math.Sin(t*3.1+float64(x)))))
		for y := cy - n; y <= cy+n; y++ {
			c := cards.RGB{40, 220, 140}
			f[y][x], f[y][x+1] = c, c
		}
	}
}

func drawNotice(f *panel.Frame, n notice) {
	switch n.kind {
	case noticeVolume:
		white := cards.RGB{220, 230, 255}
		cards.Text(f, "VOL", (panel.Size-cards.TextWidth("VOL", 2))/2, 3, 2, white)
		if !n.on {
			red := cards.RGB{255, 40, 30}
			cards.Text(f, "MUTED", (panel.Size-cards.TextWidth("MUTED", 1))/2, 19, 1, red)
			return
		}
		blue := cards.RGB{0, 170, 255}
		cards.Fill(f, 3, 17, 26, 5, cards.Dim(blue, 0.15))
		cards.Fill(f, 3, 17, int(math.Round(26*n.level)), 5, blue)
		pc := fmt.Sprintf("%d%%", int(math.Round(100*n.level)))
		cards.Text(f, pc, (panel.Size-cards.TextWidth(pc, 1))/2, 25, 1, cards.Dim(white, 0.6))
	case noticeMic:
		c, word := cards.RGB{40, 220, 140}, "ON"
		if !n.on {
			c, word = cards.RGB{255, 100, 0}, "OFF"
		}
		cards.Text(f, "MIC", (panel.Size-cards.TextWidth("MIC", 2))/2, 4, 2, c)
		cards.Text(f, word, (panel.Size-cards.TextWidth(word, 2))/2, 18, 2, c)
	}
}

// scrollText draws s across the panel on row y: centred if it fits, else scrolling (t seconds in).
func scrollText(f *panel.Frame, s string, y int, t float64, c cards.RGB) {
	scrollTextIn(f, s, y, t, 0, panel.Size, c)
}

// scrollTextIn is scrollText within columns [x0, x1): it starts readable at the left, scrolls off, then
// comes round again from the right.
func scrollTextIn(f *panel.Frame, s string, y int, t float64, x0, x1 int, c cards.RGB) {
	w, span := cards.TextWidth(s, 1), x1-x0
	if w <= span {
		cards.Text(f, s, x0+(span-w)/2, y, 1, c)
		return
	}
	var line panel.Frame
	x := 1 - int(math.Max(0, t-0.8)*scrollSpeed) // hold the start a moment
	if x+w < 0 {
		x = span - (-(x + w))%(w+span)
	}
	cards.Text(&line, s, x, y, 1, c)
	for r := y; r < y+5 && r < panel.Size; r++ {
		copy(f[r][x0:x1], line[r][:span])
	}
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
