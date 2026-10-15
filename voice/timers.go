package voice

import (
	"fmt"
	"sort"
	"time"

	"pixeld/cards"
	api "pixeld/esphomeapi"
	"pixeld/panel"
)

const (
	timerFocusFor = 5 * time.Second  // full-screen countdown after a timer is set or changed
	timerBigLast  = 10 * time.Second // ... and for its last seconds
	timerCycle    = 3 * time.Second  // the strip takes turns between several timers
	timerStale    = time.Minute      // forget a timer this long after it should have finished
)

var (
	timerColor  = cards.RGB{255, 160, 0}
	timerUrgent = cards.RGB{255, 60, 20}
)

// voiceTimer is a timer Home Assistant runs for this satellite, as of its last event.
type voiceTimer struct {
	id, name string
	total    time.Duration
	left     time.Duration // at `at`
	active   bool          // false: paused
	at       time.Time
}

func (t *voiceTimer) remaining(now time.Time) time.Duration {
	if !t.active {
		return t.left
	}
	return max(0, t.left-now.Sub(t.at))
}

// nextTick is when the whole seconds shown for t next change (zero: paused, nothing changes).
func (t *voiceTimer) nextTick(now time.Time) time.Time {
	r := t.remaining(now)
	if !t.active || r == 0 {
		return time.Time{}
	}
	d := r % time.Second
	if d == 0 {
		d = time.Second
	}
	return now.Add(d)
}

// timerText formats what's left as the countdown reads it (rounded up: 0:01 until it finishes).
func timerText(d time.Duration) string {
	s := int((d + time.Second - 1) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func (s *Satellite) timerEvent(ev *api.VoiceAssistantTimerEventResponse, now time.Time) {
	s.mu.Lock()
	switch ev.EventType {
	case api.VoiceAssistantTimerEvent_VOICE_ASSISTANT_TIMER_STARTED, api.VoiceAssistantTimerEvent_VOICE_ASSISTANT_TIMER_UPDATED:
		s.timers[ev.TimerId] = &voiceTimer{id: ev.TimerId, name: cards.Printable(ev.Name),
			total: time.Duration(ev.TotalSeconds) * time.Second, left: time.Duration(ev.SecondsLeft) * time.Second,
			active: ev.IsActive, at: now}
		s.timerFocus, s.timerFocusUntil = ev.TimerId, now.Add(timerFocusFor)
	default: // cancelled or finished
		delete(s.timers, ev.TimerId)
		if s.timerFocus == ev.TimerId {
			s.timerFocus = ""
		}
	}
	s.mu.Unlock()
	if s.OnTimers != nil {
		s.OnTimers()
	}
}

// expireTimers (s.mu held) drops timers long run out with no event (Home Assistant ends its timers with
// one; without it the timer was lost, e.g. HA restarted) and returns when the next would be dropped.
func (s *Satellite) expireTimers(now time.Time) time.Time {
	var expire time.Time
	dropped := false
	for id, t := range s.timers {
		if !t.active {
			continue
		}
		if end := t.at.Add(t.left + timerStale); now.Before(end) {
			expire = earliest(expire, end)
		} else {
			delete(s.timers, id)
			dropped = true
		}
	}
	if dropped && s.OnTimers != nil {
		go s.OnTimers() // not under s.mu (or the player's lock, when drawing)
	}
	return expire
}

// sortedTimers (s.mu held): least time left first.
func (s *Satellite) sortedTimers(now time.Time) []*voiceTimer {
	ts := make([]*voiceTimer, 0, len(s.timers))
	for _, t := range s.timers {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool {
		ri, rj := ts[i].remaining(now), ts[j].remaining(now)
		if ri != rj {
			return ri < rj
		}
		return ts[i].id < ts[j].id
	})
	return ts
}

// HasTimers reports whether any timers are running (or paused).
func (s *Satellite) HasTimers() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timers) > 0
}

// DrawTimers is the panel overlay for running timers (a player.Overlay). Full screen for a few seconds
// when a timer is set or changed, and for its last seconds; otherwise a faint line round the edge of
// whatever is showing, shrinking as the time runs out (none over items that show the timer themselves).
// It steps aside while the satellite has the panel (listening, speaking, ringing, a notice).
func (s *Satellite) DrawTimers(f *panel.Frame, now time.Time, quiet bool) (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expire := s.expireTimers(now)
	if len(s.timers) == 0 {
		return false, time.Time{}
	}
	if s.phase != Idle || s.noticeActive(now) || s.visualsBusy.Load() {
		return false, s.noticeUntil() // look again once a notice ends (a phase change refreshes anyway)
	}
	if t := s.timers[s.timerFocus]; t != nil && now.Before(s.timerFocusUntil) {
		drawTimerBig(f, t, now)
		return true, earliest(t.nextTick(now), s.timerFocusUntil, halfSecond(now), expire)
	}
	t := s.sortedTimers(now)[0]
	r := t.remaining(now)
	if t.active && r <= timerBigLast {
		drawTimerBig(f, t, now)
		return true, earliest(t.nextTick(now), halfSecond(now), expire)
	}
	var big time.Time // when it turns full screen
	if t.active {
		big = now.Add(r - timerBigLast)
	}
	if quiet {
		return false, earliest(big, expire)
	}
	drawTimerEdge(f, t, now)
	return true, earliest(t.nextTick(now), big, expire)
}

// RenderTimers draws the timers' own playlist slot: each in turn, full screen.
func (s *Satellite) RenderTimers(now time.Time) (*panel.Frame, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := new(panel.Frame)
	ts := s.sortedTimers(now)
	if len(ts) == 0 {
		return f, time.Time{}
	}
	i := 0
	var cycle time.Time
	if len(ts) > 1 {
		i = int(now.UnixNano()/int64(timerCycle)) % len(ts)
		cycle = now.Truncate(timerCycle).Add(timerCycle)
	}
	t := ts[i]
	drawTimerBig(f, t, now)
	var paused time.Time
	if !t.active {
		paused = halfSecond(now)
	}
	var scroll time.Time
	if cards.TextWidth(t.name, 1) > panel.Size {
		scroll = now.Add(time.Second / scrollSpeed)
	}
	return f, earliest(t.nextTick(now), cycle, paused, scroll)
}

// TimerBadge is the soonest timer for cards that make room for it (cards.Env.Badge).
func (s *Satellite) TimerBadge(now time.Time) (cards.Badge, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.timers) == 0 {
		return cards.Badge{}, false
	}
	t := s.sortedTimers(now)[0]
	r := t.remaining(now)
	c := timerColor
	if t.active && r <= timerBigLast {
		c = timerUrgent
	}
	return cards.Badge{Text: timerText(r), Color: c, Paused: !t.active}, true
}

func halfSecond(now time.Time) time.Time {
	return now.Truncate(500 * time.Millisecond).Add(500 * time.Millisecond)
}

func earliest(ts ...time.Time) time.Time {
	var e time.Time
	for _, t := range ts {
		if !t.IsZero() && (e.IsZero() || t.Before(e)) {
			e = t
		}
	}
	return e
}

// blinkOff: paused timers blink.
func blinkOff(t *voiceTimer, now time.Time) bool {
	return !t.active && now.UnixMilli()/500%2 == 1
}

// edge is the panel's outer ring of pixels clockwise from the top centre.
var edge = func() [][2]int {
	var ring [][2]int
	for x := 0; x < panel.Size; x++ {
		ring = append(ring, [2]int{x, 0})
	}
	for y := 1; y < panel.Size; y++ {
		ring = append(ring, [2]int{panel.Size - 1, y})
	}
	for x := panel.Size - 2; x >= 0; x-- {
		ring = append(ring, [2]int{x, panel.Size - 1})
	}
	for y := panel.Size - 2; y > 0; y-- {
		ring = append(ring, [2]int{0, y})
	}
	h := panel.Size / 2
	return append(ring[h:], ring[:h]...)
}()

// edgeAlpha: how much the edge line covers what's under it.
const edgeAlpha = 0.6

// drawTimerEdge: the time left as a line round the edge, blended over the picture: full when set,
// shrinking back anticlockwise to the top centre.
func drawTimerEdge(f *panel.Frame, t *voiceTimer, now time.Time) {
	n := len(edge)
	if t.total > 0 {
		r := t.remaining(now)
		n = int((int64(len(edge))*int64(r) + int64(t.total) - 1) / int64(t.total))
	}
	c := timerColor
	if !t.active {
		c = cards.Dim(c, 0.4)
	}
	for _, p := range edge[:min(n, len(edge))] {
		px := &f[p[1]][p[0]]
		for k := range px {
			px[k] = uint8(float64(px[k])*(1-edgeAlpha) + float64(c[k])*edgeAlpha)
		}
	}
}

// drawTimerBig: name on top, big countdown, progress bar.
func drawTimerBig(f *panel.Frame, t *voiceTimer, now time.Time) {
	*f = panel.Frame{}
	r := t.remaining(now)
	name := t.name
	if name == "" {
		name = "TIMER"
	}
	scrollText(f, name, 3, now.Sub(t.at).Seconds(), cards.Dim(timerColor, 0.6))

	c := timerColor
	if t.active && r <= timerBigLast {
		c = timerUrgent
	}
	if !blinkOff(t, now) {
		s := int((r + time.Second - 1) / time.Second)
		var a, b string
		colon := true
		if s >= 3600 { // 1H05
			a, b, colon = fmt.Sprint(s/3600), fmt.Sprintf("H%02d", s/60%60), false
		} else {
			a, b = fmt.Sprint(s/60), fmt.Sprintf("%02d", s%60)
		}
		// 6x10 digits (3x5 doubled) with a 2 px colon, like the clock card
		w := cards.TextWidth(a, 2) + cards.TextWidth(b, 2) + 2
		if colon {
			w += 2
		}
		x, y := (panel.Size-w)/2, 11
		x = cards.Text(f, a, x, y, 2, c)
		if colon {
			x--
			cards.Fill(f, x, y+2, 2, 2, c)
			cards.Fill(f, x, y+6, 2, 2, c)
			x += 3
		}
		cards.Text(f, b, x, y, 2, c)
	}
	progress(f, t, r, 3, 26, panel.Size-6, 2)
}

// progress draws the part of the bar still to run (full = just set), on a dim track.
func progress(f *panel.Frame, t *voiceTimer, r time.Duration, x, y, w, h int) {
	cards.Fill(f, x, y, w, h, cards.Dim(timerColor, 0.12))
	if t.total > 0 {
		n := int((int64(w)*int64(r) + int64(t.total) - 1) / int64(t.total))
		cards.Fill(f, x, y, min(n, w), h, cards.Dim(timerColor, 0.7))
	}
}
