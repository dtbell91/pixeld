// Package player decides what the panel shows: the playlist (rotated here, on the light), an interruption
// (an image shown on top, optionally for a while), or nothing; plus power and brightness. The loop sleeps
// until the next frame change or command, and only redraws when the picture actually changes.
package player

import (
	"context"
	"sync"
	"time"

	"pixeld/content"
	"pixeld/panel"
)

// Sink is where frames go: the panel on the light, a preview in development.
type Sink interface {
	Show(f *panel.Frame, brightness int) error
}

const DefaultItemDuration = 30 * time.Second

type Item struct {
	Name     string
	Key      string        // content hash
	Duration time.Duration // how long the item stays in the rotation (0 = DefaultItemDuration)
	Frames   []content.Frame
	// Render, if set, draws the item live (e.g. a clock) instead of Frames: the frame for now and when it
	// next changes (zero: not until something else changes).
	Render func(now time.Time) (*panel.Frame, time.Time)
	// ShowsBadges: the item makes room for what the overlay shows (e.g. a clock showing a running timer).
	ShowsBadges bool
}

// Overlay draws on top of whatever is showing (playlist, image or stream), e.g. a running timer. It gets a
// copy of the frame to draw into and reports whether it drew anything and when it next changes (zero: not
// until something else changes). quiet: the item showing makes room for it already (Item.ShowsBadges), so
// only something urgent should be drawn. It runs with the player locked, so it must not call back into the
// player.
type Overlay func(f *panel.Frame, now time.Time, quiet bool) (drawn bool, next time.Time)

const (
	ShowingOff      = "off"
	ShowingPlaylist = "playlist"
	ShowingImage    = "image"
	ShowingIdle     = "idle"
	ShowingStream   = "stream"

	// StreamTimeout: how long after the last streamed frame the panel goes back to the playlist.
	StreamTimeout = 2 * time.Second
)

type Status struct {
	On         bool   `json:"on"`
	Brightness int    `json:"brightness"`
	Showing    string `json:"showing"`
	Item       string `json:"item,omitempty"`
	Items      int    `json:"playlist_items"`
	Paused     bool   `json:"paused"` // playlist held by an interruption without a time limit
}

type Player struct {
	mu         sync.Mutex
	sink       Sink
	on         bool
	brightness int
	items      []*Item
	idx        int
	itemStart  time.Time
	intr       *Item
	intrStart  time.Time
	intrUntil  time.Time // zero: until Resume
	live       *panel.Frame // latest streamed frame (DDP), shown over everything until liveUntil
	liveUntil  time.Time
	overlay    Overlay
	wake       chan struct{}
	onStatus   func(Status)
	last       Status
	shown      *panel.Frame
	shownBri   int
	blank      panel.Frame
	now        func() time.Time
}

func New(sink Sink, on bool, brightness int, onStatus func(Status)) *Player {
	return &Player{sink: sink, on: on, brightness: clamp(brightness), wake: make(chan struct{}, 1),
		onStatus: onStatus, now: time.Now}
}

func clamp(b int) int {
	if b < 1 {
		return 1
	}
	if b > 100 {
		return 100
	}
	return b
}

func (p *Player) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Player) SetOn(on bool) {
	p.mu.Lock()
	p.on = on
	p.mu.Unlock()
	p.poke()
}

func (p *Player) SetBrightness(b int) {
	p.mu.Lock()
	p.brightness = clamp(b)
	p.mu.Unlock()
	p.poke()
}

// SetPlaylist replaces the rotation. If the item on screen is still in the new list, rotation carries on
// from it without restarting.
func (p *Player) SetPlaylist(items []*Item) {
	p.mu.Lock()
	cur := ""
	if p.idx < len(p.items) {
		cur = p.items[p.idx].Key
	}
	p.items = items
	p.idx = 0
	keep := false
	for i, it := range items {
		if it.Key == cur && cur != "" {
			p.idx, keep = i, true
			break
		}
	}
	if !keep {
		p.itemStart = p.now()
	}
	p.mu.Unlock()
	p.poke()
}

// Interrupt shows an item over the playlist; hold 0 keeps it until Resume.
func (p *Player) Interrupt(it *Item, hold time.Duration) {
	p.mu.Lock()
	p.intr, p.intrStart = it, p.now()
	p.intrUntil = time.Time{}
	if hold > 0 {
		p.intrUntil = p.intrStart.Add(hold)
	}
	p.mu.Unlock()
	p.poke()
}

func (p *Player) Resume() {
	p.mu.Lock()
	p.intr = nil
	p.mu.Unlock()
	p.poke()
}

// Stream shows a live frame (e.g. from DDP) over everything else; the panel returns to its normal
// content StreamTimeout after the last one. f must not be modified afterwards.
func (p *Player) Stream(f *panel.Frame) {
	p.mu.Lock()
	p.live, p.liveUntil = f, p.now().Add(StreamTimeout)
	p.mu.Unlock()
	p.poke()
}

// EndStream drops the streamed frame now rather than StreamTimeout after the last one.
func (p *Player) EndStream() {
	p.mu.Lock()
	p.live = nil
	p.mu.Unlock()
	p.poke()
}

// SetOverlay sets (nil: removes) the layer drawn over everything shown while the panel is on.
func (p *Player) SetOverlay(o Overlay) {
	p.mu.Lock()
	p.overlay = o
	p.mu.Unlock()
	p.poke()
}

// Refresh redraws now, e.g. because what the overlay draws has changed.
func (p *Player) Refresh() { p.poke() }

// Next skips to the next playlist item (or resumes the playlist if interrupted).
func (p *Player) Next() {
	p.mu.Lock()
	if p.intr != nil {
		p.intr = nil
	} else if len(p.items) > 0 {
		p.idx = (p.idx + 1) % len(p.items)
		p.itemStart = p.now()
	}
	p.mu.Unlock()
	p.poke()
}

func (p *Player) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status()
}

func (p *Player) status() Status {
	s := Status{On: p.on, Brightness: p.brightness, Items: len(p.items)}
	switch {
	case !p.on:
		s.Showing = ShowingOff
	case p.live != nil:
		s.Showing = ShowingStream
	case p.intr != nil:
		s.Showing, s.Item, s.Paused = ShowingImage, p.intr.Name, p.intrUntil.IsZero()
	case len(p.items) > 0:
		s.Showing, s.Item = ShowingPlaylist, p.items[p.idx].Name
	default:
		s.Showing = ShowingIdle
	}
	return s
}

// frameAt returns the frame of an item that started at start, and when that frame ends (zero: never).
func frameAt(it *Item, start, now time.Time) (*panel.Frame, time.Time) {
	if it.Render != nil {
		return it.Render(now)
	}
	if len(it.Frames) == 0 {
		return nil, time.Time{}
	}
	if len(it.Frames) == 1 || it.Frames[0].Duration == 0 {
		return &it.Frames[0].Pixels, time.Time{}
	}
	var total time.Duration
	for _, f := range it.Frames {
		total += f.Duration
	}
	el := now.Sub(start)
	loopStart := start.Add(el - el%total)
	t := el % total
	for i := range it.Frames {
		if t < it.Frames[i].Duration {
			return &it.Frames[i].Pixels, loopStart.Add(it.Frames[i].Duration)
		}
		t -= it.Frames[i].Duration
		loopStart = loopStart.Add(it.Frames[i].Duration)
	}
	return &it.Frames[0].Pixels, now.Add(it.Frames[0].Duration)
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// step advances state to now, draws if needed, and returns when it next needs to run (zero: on command only).
func (p *Player) step() (time.Time, error) {
	p.mu.Lock()
	now := p.now()
	var next time.Time
	if p.intr != nil && !p.intrUntil.IsZero() && !now.Before(p.intrUntil) {
		p.intr = nil
	}
	if len(p.items) > 1 {
		d := p.items[p.idx].Duration
		if d <= 0 {
			d = DefaultItemDuration
		}
		for !now.Before(p.itemStart.Add(d)) {
			p.itemStart = p.itemStart.Add(d)
			if now.Sub(p.itemStart) > d { // long gap (e.g. was off): don't replay every missed switch
				p.itemStart = now
			}
			p.idx = (p.idx + 1) % len(p.items)
			d = p.items[p.idx].Duration
			if d <= 0 {
				d = DefaultItemDuration
			}
		}
		next = p.itemStart.Add(d)
	}
	if p.live != nil && !now.Before(p.liveUntil) {
		p.live = nil
	}
	var frame *panel.Frame
	quiet := false
	switch {
	case !p.on:
		frame = &p.blank
	case p.live != nil:
		frame, next = p.live, p.liveUntil
	case p.intr != nil:
		var end time.Time
		frame, end = frameAt(p.intr, p.intrStart, now)
		next = earliest(end, p.intrUntil)
	case len(p.items) > 0:
		var end time.Time
		frame, end = frameAt(p.items[p.idx], p.itemStart, now)
		next = earliest(next, end)
		quiet = p.items[p.idx].ShowsBadges
	default:
		frame = &p.blank
	}
	if frame == nil {
		frame = &p.blank
	}
	if p.on && p.overlay != nil {
		c := *frame
		drawn, end := p.overlay(&c, now, quiet)
		if drawn {
			frame = &c // a new frame each time, so it is always redrawn
		}
		next = earliest(next, end)
	}
	bri := p.brightness
	st := p.status()
	changed := st != p.last
	p.last = st
	draw := frame != p.shown || bri != p.shownBri
	p.shown, p.shownBri = frame, bri
	cb := p.onStatus
	p.mu.Unlock()

	var err error
	if draw {
		err = p.sink.Show(frame, bri)
	}
	if changed && cb != nil {
		cb(st)
	}
	return next, err
}

// Run drives the panel until ctx ends. Draw errors are reported to onErr and retried on the next change.
func (p *Player) Run(ctx context.Context, onErr func(error)) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		next, err := p.step()
		if err != nil && onErr != nil {
			onErr(err)
		}
		wait := time.Hour
		if !next.IsZero() {
			wait = time.Until(next)
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-p.wake: // Go >= 1.23: Reset needs no drain
		case <-timer.C:
		}
	}
}
