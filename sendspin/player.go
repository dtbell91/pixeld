package sendspin

import (
	"encoding/binary"
	"log"
	"sync"
	"time"
)

const (
	outRate   = 48000
	deadBand  = 100  // µs: closer than this to the timeline needs no correction
	snapError = 1000 // µs: further than this (or starting) jumps straight to the right place
)

type streamFormat struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	BitDepth   int    `json:"bit_depth"`
}

type chunk struct {
	ts  int64   // server time of the first sample (µs)
	pcm []int16 // mono, outRate
}

func (c chunk) end() int64 { return c.ts + int64(len(c.pcm))*1e6/outRate }

// player keeps the audio a server has sent ahead and hands the speaker exactly what is due at each moment
// on the server's timeline (Fill), correcting drift a sample at a time (inaudible) and jumping only on
// start, after a gap, or if it ever falls more than a millisecond out.
type player struct {
	mu        sync.Mutex
	sess      *session // whose stream is playing (nil: none)
	format    streamFormat
	ok        bool // the format is one the light plays
	chunks    []chunk
	pos       int  // next sample in chunks[0]
	synced    bool // pos follows the timeline
	delayUs   int64
	onPlaying func(bool)
	stats     fillStats
}

type fillStats struct {
	corrections, snaps, late, underruns int
	last                                time.Time
}

func newPlayer(onPlaying func(bool)) *player {
	if onPlaying == nil {
		onPlaying = func(bool) {}
	}
	return &player{onPlaying: onPlaying}
}

func (p *player) setOutputDelay(ms int) {
	p.mu.Lock()
	p.delayUs = int64(ms) * 1000
	p.mu.Unlock()
}

func (p *player) start(s *session, f streamFormat) {
	p.mu.Lock()
	starting := p.sess != s
	p.sess, p.format = s, f
	p.ok = f.Codec == "pcm" && f.SampleRate == outRate && f.BitDepth == 16 && (f.Channels == 1 || f.Channels == 2)
	if starting {
		p.chunks, p.synced = nil, false
	}
	p.mu.Unlock()
	if !p.ok {
		log.Printf("sendspin: unsupported stream format %+v", f)
	}
	if starting {
		log.Printf("sendspin: stream started (%s %d Hz, %d ch)", f.Codec, f.SampleRate, f.Channels)
		p.onPlaying(true)
	}
}

func (p *player) chunk(s *session, ts int64, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sess != s || !p.ok {
		return
	}
	ch := p.format.Channels
	n := len(data) / (2 * ch)
	pcm := make([]int16, n)
	for i := range pcm {
		sum := 0
		for c := 0; c < ch; c++ {
			sum += int(int16(binary.LittleEndian.Uint16(data[(i*ch+c)*2:])))
		}
		pcm[i] = int16(sum / ch)
	}
	p.chunks = append(p.chunks, chunk{ts: ts, pcm: pcm})
}

// clear drops what's buffered (a seek or skip); the stream carries on.
func (p *player) clear(s *session) {
	p.mu.Lock()
	if p.sess == s {
		p.chunks, p.pos, p.synced = nil, 0, false
	}
	p.mu.Unlock()
}

// end stops the stream.
func (p *player) end(s *session) { p.detach(s) }

// detach stops s's stream (if it is the one playing).
func (p *player) detach(s *session) {
	p.mu.Lock()
	was := p.sess != nil && p.sess == s
	if was {
		p.sess, p.chunks, p.pos, p.synced = nil, nil, 0, false
	}
	p.mu.Unlock()
	if was {
		log.Printf("sendspin: stream ended")
		p.onPlaying(false)
	}
}

// stop ends whatever is playing (something else took the speaker).
func (p *player) stop() {
	p.mu.Lock()
	s := p.sess
	p.mu.Unlock()
	if s != nil {
		p.detach(s)
	}
}

// Fill implements voice.TimedSource: dst gets the samples due to be heard from at onward.
func (p *player) Fill(dst []int16, at time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sess == nil || len(p.chunks) == 0 {
		if p.synced {
			p.synced = false
			p.stats.underruns++
		}
		return false
	}
	due, ok := p.sess.serverTime(at.Sub(epoch).Microseconds() + p.delayUs)
	if !ok {
		return false
	}
	p.logStats()
	out := dst
	if p.synced {
		cur := p.chunks[0].ts + int64(p.pos)*1e6/outRate
		switch e := cur - due; {
		case e > snapError || e < -snapError:
			p.synced = false
			p.stats.snaps++
		case e > deadBand: // ahead of the timeline: hold back a sample
			out[0] = p.peek()
			out = out[1:]
			p.stats.corrections++
		case e < -deadBand: // behind: skip one
			p.take(1)
			p.stats.corrections++
		}
	}
	if !p.synced {
		for len(p.chunks) > 0 && p.chunks[0].end() <= due { // already past
			p.chunks = p.chunks[1:]
			p.stats.late++
		}
		p.pos = 0
		if len(p.chunks) == 0 {
			return false
		}
		if c := p.chunks[0]; c.ts > due { // starts later: silence until then
			gap := int((c.ts - due) * outRate / 1e6)
			if gap >= len(out) {
				return false
			}
			clear(out[:gap])
			out = out[gap:]
		} else {
			p.pos = int((due - c.ts) * outRate / 1e6)
		}
		p.synced = true
	}
	n := p.take(len(out), out)
	if n < len(out) { // ran out: the server will start us again
		clear(out[n:])
		p.synced = false
		p.stats.underruns++
	}
	return true
}

// peek is the next sample (chunks non-empty).
func (p *player) peek() int16 {
	if c := p.chunks[0]; p.pos < len(c.pcm) {
		return c.pcm[p.pos]
	}
	return 0
}

// take consumes up to n samples (into dst, if given) and returns how many there were.
func (p *player) take(n int, dst ...[]int16) int {
	got := 0
	for got < n && len(p.chunks) > 0 {
		c := p.chunks[0]
		k := min(n-got, len(c.pcm)-p.pos)
		if len(dst) > 0 {
			copy(dst[0][got:], c.pcm[p.pos:p.pos+k])
		}
		got += k
		p.pos += k
		if p.pos >= len(c.pcm) {
			// on into the next chunk (a gap before it shows up as a timeline error at the next Fill)
			p.chunks, p.pos = p.chunks[1:], 0
		}
	}
	return got
}

func (p *player) logStats() {
	if time.Since(p.stats.last) < time.Minute {
		return
	}
	if !p.stats.last.IsZero() && p.stats != (fillStats{last: p.stats.last}) {
		log.Printf("sendspin: last minute: %d corrections, %d resyncs, %d late chunks, %d underruns",
			p.stats.corrections, p.stats.snaps, p.stats.late, p.stats.underruns)
	}
	p.stats = fillStats{last: time.Now()}
}
