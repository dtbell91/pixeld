package player

import (
	"testing"
	"time"

	"pixeld/content"
	"pixeld/panel"
)

type rec struct{ shown []*panel.Frame }

func (r *rec) Show(f *panel.Frame, _ int) error { r.shown = append(r.shown, f); return nil }

func still(name string, v uint8, d time.Duration) *Item {
	return &Item{Name: name, Key: name, Duration: d, Frames: []content.Frame{{Pixels: content.Solid(v, v, v)}}}
}

func setup() (*Player, *rec, *time.Time) {
	r := &rec{}
	t := time.Unix(1000, 0)
	p := New(r, true, 100, nil)
	p.now = func() time.Time { return t }
	return p, r, &t
}

func TestRotationAndKeepPosition(t *testing.T) {
	p, _, now := setup()
	a, b, c := still("a", 1, 10*time.Second), still("b", 2, 10*time.Second), still("c", 3, 10*time.Second)
	p.SetPlaylist([]*Item{a, b, c})
	if next, _ := p.step(); p.Status().Item != "a" || !next.Equal(now.Add(10*time.Second)) {
		t.Fatalf("start: item %q next %v", p.Status().Item, next)
	}
	*now = now.Add(12 * time.Second)
	p.step()
	if p.Status().Item != "b" {
		t.Fatalf("after 12s: %q", p.Status().Item)
	}
	// edit the playlist (c replaced by d) while b is showing: b stays, timing kept
	p.SetPlaylist([]*Item{a, b, still("d", 4, 10*time.Second)})
	next, _ := p.step()
	if p.Status().Item != "b" || !next.Equal(time.Unix(1020, 0)) {
		t.Fatalf("after edit: item %q next %v", p.Status().Item, next)
	}
}

func TestInterruptHoldAndResume(t *testing.T) {
	p, r, now := setup()
	p.SetPlaylist([]*Item{still("a", 1, 30*time.Second)})
	p.step()
	p.Interrupt(still("msg", 9, 0), 5*time.Second)
	p.step()
	if st := p.Status(); st.Showing != ShowingImage || st.Paused {
		t.Fatalf("interrupt: %+v", st)
	}
	*now = now.Add(6 * time.Second)
	p.step()
	if p.Status().Showing != ShowingPlaylist {
		t.Fatalf("after hold: %+v", p.Status())
	}
	p.Interrupt(still("pin", 8, 0), 0)
	*now = now.Add(time.Hour)
	p.step()
	if st := p.Status(); st.Showing != ShowingImage || !st.Paused {
		t.Fatalf("indefinite: %+v", st)
	}
	p.Resume()
	p.step()
	if p.Status().Showing != ShowingPlaylist {
		t.Fatalf("resume: %+v", p.Status())
	}
	if len(r.shown) != 5 {
		t.Fatalf("draws: %d (want 5: a, msg, a, pin, a — one per change)", len(r.shown))
	}
}

func TestAnimationFrames(t *testing.T) {
	p, r, now := setup()
	it := &Item{Name: "anim", Key: "anim", Frames: []content.Frame{
		{Pixels: content.Solid(1, 1, 1), Duration: 100 * time.Millisecond},
		{Pixels: content.Solid(2, 2, 2), Duration: 300 * time.Millisecond}}}
	p.SetPlaylist([]*Item{it})
	next, _ := p.step()
	if !next.Equal(now.Add(100 * time.Millisecond)) {
		t.Fatalf("first frame ends %v", next)
	}
	*now = now.Add(150 * time.Millisecond)
	next, _ = p.step()
	if r.shown[len(r.shown)-1] != &it.Frames[1].Pixels || !next.Equal(time.Unix(1000, 400000000)) {
		t.Fatalf("second frame, next %v", next)
	}
	p.SetOn(false)
	p.step()
	if p.Status().Showing != ShowingOff {
		t.Fatal("off")
	}
}

func TestStreamOverridesThenTimesOut(t *testing.T) {
	p, r, now := setup()
	p.SetPlaylist([]*Item{still("a", 1, 30*time.Second)})
	p.step()
	live := content.Solid(9, 9, 9)
	p.Stream(&live)
	next, _ := p.step()
	if p.Status().Showing != ShowingStream || r.shown[len(r.shown)-1] != &live || !next.Equal(now.Add(StreamTimeout)) {
		t.Fatalf("stream: %+v next %v", p.Status(), next)
	}
	*now = now.Add(StreamTimeout)
	p.step()
	if p.Status().Showing != ShowingPlaylist {
		t.Fatalf("after timeout: %+v", p.Status())
	}
	p.SetOn(false)
	p.Stream(&live)
	p.step()
	if p.Status().Showing != ShowingOff {
		t.Fatalf("stream while off: %+v", p.Status())
	}
}
