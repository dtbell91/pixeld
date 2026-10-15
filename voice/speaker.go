package voice

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

const (
	OutRate      = 48000         // what the speaker is driven at (mono)
	MusicRate    = 48000         // what Home Assistant is asked to convert music to
	blockSamples = OutRate / 100 // 10 ms
	idleBlocks   = 30            // keep the output open this long after the last sound
	musicBuffer  = 200           // blocks (2 s) of decoded music kept ahead of the output
	duckDown     = 0.1           // music gain change per block when ducking (fast: ~70 ms to a voice level)
	duckUp       = 0.04          // ... and coming back (~200 ms)
	voiceDuck    = 0.3           // music level under a voice sound
)

// ErrStopped is passed to a music stream's done callback when it was stopped (not at its end).
var ErrStopped = errors.New("stopped")

// Speaker drives the light's speaker: one output (opened while anything plays, amplifier on meanwhile)
// mixing a music stream with voice sounds (replies, chimes, announcements, alarms). Music is ducked under
// voice sounds and while the satellite listens or speaks (SetDuck).
type Speaker struct {
	mu     sync.Mutex
	volume float64 // 0..1
	muted  bool
	duck   float64 // music level the satellite asks for (1 = full)
	gain   float64 // current music duck level (ramps towards the target)
	voice  *clip
	music  *stream
	wake   chan struct{}
	device string // ALSA device ("" = the codec)
}

type clip struct {
	pcm  []int16 // mono at OutRate
	pos  int
	done func()
}

type stream struct {
	blocks chan []int16 // decoded music, closed at the end
	paused bool
	cancel context.CancelFunc
	done   func(error)
	err    error // set before blocks is closed
}

func NewSpeaker(volume float64) *Speaker {
	s := &Speaker{volume: volume, duck: 1, gain: 1, wake: make(chan struct{}, 1)}
	go s.run()
	return s
}

func (s *Speaker) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Speaker) SetVolume(v float64) {
	s.mu.Lock()
	s.volume = math.Max(0, math.Min(1, v))
	s.mu.Unlock()
}

func (s *Speaker) SetMuted(m bool) {
	s.mu.Lock()
	s.muted = m
	s.mu.Unlock()
}

// SetDuck sets the music level while the satellite is busy (1 = full, e.g. 0.1 while listening).
func (s *Speaker) SetDuck(level float64) {
	s.mu.Lock()
	s.duck = math.Max(0, math.Min(1, level))
	s.mu.Unlock()
}

// ---- voice sounds ----

// PlayPCM plays 16-bit PCM as a voice sound (replacing the current one, whose done still runs) and calls
// done (may be nil) when it has finished.
func (s *Speaker) PlayPCM(pcm []byte, rate, channels int, done func()) {
	c := &clip{pcm: convert(pcm, rate, channels), done: done}
	s.mu.Lock()
	prev := s.voice
	s.voice = c
	s.mu.Unlock()
	if prev != nil && prev.done != nil {
		go prev.done()
	}
	s.poke()
}

// PlayWAV plays a PCM WAV file's contents as a voice sound.
func (s *Speaker) PlayWAV(data []byte, done func()) error {
	pcm, rate, ch, err := ParseWAV(data)
	if err != nil {
		if done != nil {
			done()
		}
		return err
	}
	s.PlayPCM(pcm, rate, ch, done)
	return nil
}

// Stop ends the voice sound (its done callback still runs); music carries on.
func (s *Speaker) Stop() {
	s.mu.Lock()
	v := s.voice
	s.voice = nil
	s.mu.Unlock()
	if v != nil && v.done != nil {
		go v.done()
	}
}

// Playing reports whether a voice sound is playing.
func (s *Speaker) Playing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.voice != nil
}

// ---- music ----

var streamClient = &http.Client{Transport: &http.Transport{ // no overall timeout: music streams run for hours
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	ResponseHeaderTimeout: 20 * time.Second,
}}

// PlayStream streams a WAV (e.g. Home Assistant's conversion of a Music Assistant stream) as music,
// replacing any; done gets nil at the end of the stream, ErrStopped when stopped, or the error.
func (s *Speaker) PlayStream(url string, done func(error)) {
	ctx, cancel := context.WithCancel(context.Background())
	st := &stream{blocks: make(chan []int16, musicBuffer), cancel: cancel, done: done}
	s.StopMusic()
	s.mu.Lock()
	s.music = st
	s.mu.Unlock()
	go st.decode(ctx, url)
	s.poke()
}

// PauseMusic pauses or resumes the music (the stream is held, not dropped).
func (s *Speaker) PauseMusic(paused bool) {
	s.mu.Lock()
	if s.music != nil {
		s.music.paused = paused
	}
	s.mu.Unlock()
	s.poke()
}

// StopMusic ends the music (its done gets ErrStopped).
func (s *Speaker) StopMusic() {
	s.mu.Lock()
	st := s.music
	s.music = nil
	s.mu.Unlock()
	if st != nil {
		st.cancel()
		if st.done != nil {
			go st.done(ErrStopped)
		}
	}
}

// MusicActive reports whether music is loaded (playing or paused).
func (s *Speaker) MusicActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.music != nil
}

func (st *stream) decode(ctx context.Context, url string) {
	defer close(st.blocks)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		st.err = err
		return
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		st.err = err
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		st.err = fmt.Errorf("HTTP %d", resp.StatusCode)
		return
	}
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	rate, channels, err := readWAVHeader(br)
	if err != nil {
		st.err = err
		return
	}
	rs := newResampler(rate)
	frame := channels * 2
	buf := make([]byte, 1024*frame)
	var pending []int16
	for {
		n, rerr := io.ReadFull(br, buf)
		n -= n % frame
		mono := make([]int16, n/frame)
		for i := range mono {
			sum := 0
			for c := 0; c < channels; c++ {
				sum += int(int16(binary.LittleEndian.Uint16(buf[i*frame+c*2:])))
			}
			mono[i] = int16(sum / channels)
		}
		pending = append(pending, rs.push(mono)...)
		for len(pending) >= blockSamples {
			b := make([]int16, blockSamples)
			copy(b, pending)
			pending = pending[blockSamples:]
			select {
			case st.blocks <- b: // blocks while the buffer is full (or paused): the network waits too
			case <-ctx.Done():
				return
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) && ctx.Err() == nil {
				st.err = rerr
			}
			return
		}
	}
}

// ---- output ----

// run keeps the output open while there is something to play, mixing 10 ms at a time.
func (s *Speaker) run() {
	for range s.wake {
		if !s.active() {
			continue
		}
		s.playOut()
	}
}

func (s *Speaker) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.voice != nil || (s.music != nil && !s.music.paused)
}

func (s *Speaker) playOut() {
	dev := s.device
	if dev == "" {
		dev = "plughw:0,0"
	}
	cmd := exec.Command("aplay", "-q", "-D", dev, "-t", "raw", "-f", "S16_LE", "-r", fmt.Sprint(OutRate), "-c", "1",
		"--buffer-time=150000")
	in, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		log.Printf("voice: aplay: %v", err)
		s.Stop() // don't leave a sound (and whatever waits for it) hanging
		time.Sleep(time.Second)
		return
	}
	amp(true)
	out := make([]byte, blockSamples*2)
	for idle := 0; idle < idleBlocks; {
		block, active := s.mix()
		if active {
			idle = 0
		} else {
			idle++
		}
		for i, v := range block {
			binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
		}
		if _, err := in.Write(out); err != nil {
			log.Printf("voice: aplay: %v", err)
			break
		}
	}
	in.Close()
	cmd.Wait()
	if !s.active() {
		amp(false)
	} else {
		s.poke() // something started while closing: open again
	}
}

// mix produces the next 10 ms: the voice sound plus the (ducked) music.
func (s *Speaker) mix() ([]int16, bool) {
	acc := make([]float64, blockSamples)
	s.mu.Lock()
	gain := s.volume * s.volume // perceptual-ish curve
	if s.muted {
		gain = 0
	}
	target := s.duck
	var finished *clip
	active := s.voice != nil
	if v := s.voice; v != nil {
		target = math.Min(target, voiceDuck)
		for i := 0; i < blockSamples && v.pos < len(v.pcm); i++ {
			acc[i] += float64(v.pcm[v.pos])
			v.pos++
		}
		if v.pos >= len(v.pcm) {
			finished, s.voice = v, nil
		}
	}
	s.gain += math.Max(-duckDown, math.Min(duckUp, target-s.gain))
	duck := s.gain
	m := s.music
	playing := m != nil && !m.paused
	s.mu.Unlock()

	if playing {
		active = true
		select {
		case b, ok := <-m.blocks:
			if !ok {
				s.mu.Lock()
				if s.music == m {
					s.music = nil
				}
				s.mu.Unlock()
				if m.done != nil {
					go m.done(m.err)
				}
				break
			}
			for i, v := range b {
				acc[i] += float64(v) * duck
			}
		default: // the stream hasn't caught up: a gap rather than holding up voice sounds
		}
	}
	if finished != nil && finished.done != nil {
		go finished.done()
	}
	out := make([]int16, blockSamples)
	for i, v := range acc {
		out[i] = int16(math.Max(-32768, math.Min(32767, v*gain)))
	}
	return out, active
}

// ---- formats ----

// convert turns 16-bit PCM at any rate and channel count into mono at OutRate.
func convert(pcm []byte, rate, channels int) []int16 {
	if channels < 1 {
		channels = 1
	}
	frame := channels * 2
	mono := make([]int16, len(pcm)/frame)
	for i := range mono {
		sum := 0
		for c := 0; c < channels; c++ {
			sum += int(int16(binary.LittleEndian.Uint16(pcm[i*frame+c*2:])))
		}
		mono[i] = int16(sum / channels)
	}
	return newResampler(rate).push(mono)
}

// resampler converts a mono stream to OutRate by linear interpolation, chunk by chunk.
type resampler struct {
	step float64 // input samples per output sample
	t    float64 // position of the next output, from prev (0) through the chunk (1..n)
	prev float64
}

func newResampler(rate int) *resampler {
	if rate <= 0 {
		rate = OutRate
	}
	return &resampler{step: float64(rate) / OutRate, t: 1} // the first output is the first input
}

func (r *resampler) push(in []int16) []int16 {
	if r.step == 1 {
		return in
	}
	n := len(in)
	if n == 0 {
		return nil
	}
	at := func(k int) float64 {
		if k == 0 {
			return r.prev
		}
		return float64(in[k-1])
	}
	out := make([]int16, 0, int(float64(n)/r.step)+2)
	for r.t < float64(n) {
		k := int(r.t)
		a, b := at(k), at(k+1)
		out = append(out, int16(a+(b-a)*(r.t-float64(k))))
		r.t += r.step
	}
	r.t -= float64(n)
	r.prev = float64(in[n-1])
	return out
}

// readWAVHeader reads a WAV header up to the start of its samples (streamed WAVs carry placeholder sizes).
func readWAVHeader(r *bufio.Reader) (rate, channels int, err error) {
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, 0, err
	}
	if string(head[:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return 0, 0, errors.New("not a WAV stream")
	}
	bits := 0
	for {
		ch := make([]byte, 8)
		if _, err := io.ReadFull(r, ch); err != nil {
			return 0, 0, err
		}
		id, size := string(ch[:4]), binary.LittleEndian.Uint32(ch[4:])
		switch id {
		case "fmt ":
			body := make([]byte, size+size%2)
			if size < 16 || size > 1024 {
				return 0, 0, errors.New("bad fmt chunk")
			}
			if _, err := io.ReadFull(r, body); err != nil {
				return 0, 0, err
			}
			channels = int(binary.LittleEndian.Uint16(body[2:]))
			rate = int(binary.LittleEndian.Uint32(body[4:]))
			bits = int(binary.LittleEndian.Uint16(body[14:]))
		case "data":
			if bits != 16 || channels < 1 {
				return 0, 0, fmt.Errorf("%d-bit %d-channel WAV not supported", bits, channels)
			}
			return rate, channels, nil
		default:
			if size > 1<<20 {
				return 0, 0, errors.New("bad WAV chunk")
			}
			if _, err := r.Discard(int(size + size%2)); err != nil {
				return 0, 0, err
			}
		}
	}
}
