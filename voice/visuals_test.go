package voice

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pixeld/cards"
	api "pixeld/esphomeapi"
	"pixeld/panel"
)

func newTestSat() *Satellite { return &Satellite{timers: map[string]*voiceTimer{}} }

func timerEv(typ api.VoiceAssistantTimerEvent, id, name string, total, left uint32, active bool) *api.VoiceAssistantTimerEventResponse {
	return &api.VoiceAssistantTimerEventResponse{EventType: typ, TimerId: id, Name: name,
		TotalSeconds: total, SecondsLeft: left, IsActive: active}
}

const (
	started   = api.VoiceAssistantTimerEvent_VOICE_ASSISTANT_TIMER_STARTED
	updated   = api.VoiceAssistantTimerEvent_VOICE_ASSISTANT_TIMER_UPDATED
	cancelled = api.VoiceAssistantTimerEvent_VOICE_ASSISTANT_TIMER_CANCELLED
)

func lit(f *panel.Frame, y0, y1 int) bool {
	for y := y0; y < y1; y++ {
		for x := 0; x < panel.Size; x++ {
			if f[y][x] != (cards.RGB{}) {
				return true
			}
		}
	}
	return false
}

func TestTimerOverlay(t *testing.T) {
	s := newTestSat()
	t0 := time.Unix(1000, 0)
	var f panel.Frame
	if drawn, _ := s.DrawTimers(&f, t0, false); drawn {
		t.Fatal("drew with no timers")
	}
	s.timerEvent(timerEv(started, "a", "Pasta", 600, 600, true), t0)

	// just set: full screen, back in under a second
	f = panel.Frame{}
	drawn, next := s.DrawTimers(&f, t0.Add(100*time.Millisecond), false)
	if !drawn || !lit(&f, 11, 21) || next.After(t0.Add(time.Second)) {
		t.Fatalf("focus view: drawn %v next %v", drawn, next)
	}

	// later: a line round the edge, blended over the picture, which is otherwise untouched; at 2 of 10
	// minutes 80% of the edge is lit, from the top centre clockwise
	at := t0.Add(2 * time.Minute)
	grey := func() {
		for y := range f {
			for x := range f[y] {
				f[y][x] = cards.RGB{100, 100, 100}
			}
		}
	}
	grey()
	if drawn, _ := s.DrawTimers(&f, at, false); !drawn || f[10][10] != (cards.RGB{100, 100, 100}) {
		t.Fatal("edge view drew inside the picture")
	}
	if c := f[0][16]; c == (cards.RGB{100, 100, 100}) || c == timerColor {
		t.Fatalf("edge start not blended: %v", c)
	}
	if f[0][14] != (cards.RGB{100, 100, 100}) || f[31][16] == (cards.RGB{100, 100, 100}) {
		t.Fatal("edge length wrong")
	}

	// an item that shows the timer itself: nothing drawn over it
	grey()
	if drawn, _ := s.DrawTimers(&f, at, true); drawn || f[0][16] != (cards.RGB{100, 100, 100}) {
		t.Fatal("drew over an item that shows badges")
	}
	if b, ok := s.TimerBadge(at); !ok || b.Text != "8:00" {
		t.Fatalf("badge %+v", b)
	}

	// voice has the panel: overlay steps aside
	s.phase = Listening
	if drawn, _ := s.DrawTimers(&f, at, false); drawn {
		t.Fatal("drew over the satellite")
	}
	s.phase = Idle

	// last seconds: full screen again
	f = panel.Frame{}
	if drawn, _ := s.DrawTimers(&f, t0.Add(595*time.Second), true); !drawn || !lit(&f, 11, 21) {
		t.Fatal("no full-screen countdown at the end")
	}

	// a second timer: its slot in the playlist takes turns
	s.timerEvent(timerEv(started, "b", "", 1200, 1200, true), at)
	if _, next := s.RenderTimers(at); next.After(at.Truncate(timerCycle).Add(timerCycle)) {
		t.Fatalf("no turn-taking wake-up: %v", next)
	}

	s.timerEvent(timerEv(cancelled, "a", "", 0, 0, false), at)
	s.timerEvent(timerEv(cancelled, "b", "", 0, 0, false), at)
	if drawn, _ := s.DrawTimers(&f, at, false); drawn {
		t.Fatal("drew after cancel")
	}

	// lost timer (no finished event): dropped a while after it ran out
	s.timerEvent(timerEv(started, "c", "", 10, 10, true), t0)
	if drawn, _ := s.DrawTimers(&f, t0.Add(10*time.Second+timerStale+time.Second), false); drawn || len(s.timers) != 0 {
		t.Fatal("stale timer kept")
	}
}

func TestTimerText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0:00", 400 * time.Millisecond: "0:01", 59 * time.Second: "0:59", 10 * time.Minute: "10:00",
		time.Hour + 5*time.Minute: "1:05:00",
	} {
		if got := timerText(d); got != want {
			t.Errorf("timerText(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestPreviewSheet writes a PNG of the voice screens for design review when PIXELD_PREVIEW is a directory.
func TestPreviewSheet(t *testing.T) {
	dir := os.Getenv("PIXELD_PREVIEW")
	if dir == "" {
		t.Skip("set PIXELD_PREVIEW=<dir> to write voice-screens.png")
	}
	clock, _ := cards.NewClock(cards.ClockSpec{TZ: "UTC"})
	photo := func() *panel.Frame { // stand-in for a picture in the playlist
		var f panel.Frame
		for y := range f {
			for x := range f[y] {
				f[y][x] = cards.RGB{uint8(40 + 5*y), uint8(60 + 3*x), 140}
			}
		}
		return &f
	}
	t0 := time.Date(2026, 10, 15, 18, 42, 7, 0, time.UTC)
	var frames []*panel.Frame
	timer := func(name string, total, left uint32, active bool, at time.Duration, focus bool, under string) {
		s := newTestSat()
		s.timerEvent(timerEv(started, "a", name, total, left, active), t0)
		if !focus {
			s.timerFocusUntil = time.Time{}
		}
		now := t0.Add(at)
		var c panel.Frame
		switch under {
		case "clock": // a card that shows the timer itself
			clock.SetBadge(s.TimerBadge)
			f, _ := clock.Render(now)
			c = *f
			s.DrawTimers(&c, now, true)
		case "slot":
			f, _ := s.RenderTimers(now)
			c = *f
		default:
			c = *photo()
			s.DrawTimers(&c, now, false)
		}
		frames = append(frames, &c)
	}
	timer("Pasta", 600, 600, true, 0, true, "photo")
	timer("", 600, 600, true, 3*time.Minute+200*time.Millisecond, false, "photo")
	timer("", 600, 600, true, 8*time.Minute, false, "photo")
	timer("Pasta", 600, 300, false, 0, false, "photo")
	timer("Pasta", 600, 7, true, 0, false, "photo")
	timer("Pasta", 600, 600, true, 3*time.Minute+200*time.Millisecond, false, "clock")
	timer("Roast", 5400, 4000, true, 0, false, "slot")
	for _, v := range []view{
		{phase: Thinking, text: "SET A TIMER FOR TEN MINUTES"},
		{phase: Speaking, text: "TIMER SET FOR 10 MINUTES"},
		{phase: Speaking, text: "OK"},
		{phase: TimerRinging, text: "PASTA"},
		{notice: notice{kind: noticeVolume, level: 0.6, on: true}},
		{notice: notice{kind: noticeVolume, level: 0.6}},
		{notice: notice{kind: noticeMic}},
	} {
		frames = append(frames, frame(v, 0.2))
	}
	const scale, gap, cols = 8, 12, 6
	rows := (len(frames) + cols - 1) / cols
	cell := panel.Size*scale + gap
	img := image.NewRGBA(image.Rect(0, 0, cols*cell+gap, rows*cell+gap))
	for i := range img.Pix {
		img.Pix[i] = 40
	}
	for n, f := range frames {
		ox, oy := gap+n%cols*cell, gap+n/cols*cell
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
	w, err := os.Create(filepath.Join(dir, "voice-screens.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := png.Encode(w, img); err != nil {
		t.Fatal(err)
	}
}
