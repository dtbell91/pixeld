// Package voice makes the light a Home Assistant voice satellite over the ESPHome API: wake word detected
// on the light (microWakeWord via mww), audio streamed to Home Assistant, replies and announcements played
// on the speaker, state shown on the panel.
package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	SampleRate  = 16000
	stepSamples = 160 // 10 ms
	captureCh   = 3   // the codec records MIC1..MIC3; only MIC3 is wired
	micChannel  = 2
	ampGPIO     = "/sys/class/gpio/gpio129/value" // speaker amplifier enable
)

// setupMixer switches on all three codec microphone inputs. Only MIC3 is wired, but with MIC1/MIC2 off (Govee's
// boot setting) the 3-channel capture comes out unusable on every channel (wake word probability ~0.005 vs
// 0.976 with all on, measured on the light).
func setupMixer() {
	for _, m := range []string{"MIC1", "MIC2", "MIC3"} {
		if out, err := exec.Command("amixer", "-c", "0", "-q", "sset", m, "on").CombinedOutput(); err != nil {
			log.Printf("voice: amixer %s: %v %s", m, err, out)
		}
	}
}

// Capture runs arecord and calls fn with 10 ms of mono 16-bit PCM from the wired microphone until ctx ends.
func Capture(ctx context.Context, fn func(pcm []byte)) {
	for ctx.Err() == nil {
		setupMixer()
		cmd := exec.CommandContext(ctx, "arecord", "-q", "-D", "hw:0,0", "-c", fmt.Sprint(captureCh), "-r",
			fmt.Sprint(SampleRate), "-f", "S16_LE", "-t", "raw")
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("voice: capture: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		frame := make([]byte, stepSamples*captureCh*2)
		for {
			if _, err := io.ReadFull(out, frame); err != nil {
				break
			}
			mono := make([]byte, stepSamples*2)
			for i := 0; i < stepSamples; i++ {
				copy(mono[i*2:], frame[(i*captureCh+micChannel)*2:(i*captureCh+micChannel)*2+2])
			}
			fn(mono)
		}
		cmd.Wait()
		if ctx.Err() == nil {
			log.Printf("voice: capture stopped; restarting")
			time.Sleep(time.Second)
		}
	}
}

// Speaker plays one sound at a time through aplay, with the amplifier switched on only while playing.
type Speaker struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	volume float64 // 0..1
	muted  bool
}

func NewSpeaker(volume float64) *Speaker { return &Speaker{volume: volume} }

func (s *Speaker) SetVolume(v float64) {
	s.mu.Lock()
	s.volume = math.Max(0, math.Min(1, v))
	s.mu.Unlock()
}

func amp(on bool) {
	v := "0"
	if on {
		v = "1"
	}
	os.WriteFile(ampGPIO, []byte(v), 0)
}

func (s *Speaker) SetMuted(m bool) {
	s.mu.Lock()
	s.muted = m
	s.mu.Unlock()
}

// Stop ends whatever is playing (its done callback still runs).
func (s *Speaker) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Playing reports whether something is playing.
func (s *Speaker) Playing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// PlayPCM plays 16-bit PCM (interrupting anything playing) and calls done (may be nil) when finished.
func (s *Speaker) PlayPCM(pcm []byte, rate, channels int, done func()) {
	s.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	s.mu.Lock()
	s.cancel, s.done = cancel, finished
	vol := s.volume
	if s.muted {
		vol = 0
	}
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			if s.done == finished {
				s.cancel, s.done = nil, nil
			}
			s.mu.Unlock()
			close(finished)
			if done != nil {
				done()
			}
		}()
		scaled := make([]byte, len(pcm))
		gain := vol * vol // perceptual-ish curve
		for i := 0; i+1 < len(pcm); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
			binary.LittleEndian.PutUint16(scaled[i:], uint16(int16(math.Max(-32768, math.Min(32767, v*gain)))))
		}
		amp(true)
		defer func() {
			time.AfterFunc(300*time.Millisecond, func() {
				if !s.Playing() {
					amp(false)
				}
			})
		}()
		cmd := exec.CommandContext(ctx, "aplay", "-q", "-D", "plughw:0,0", "-t", "raw", "-f", "S16_LE",
			"-r", fmt.Sprint(rate), "-c", fmt.Sprint(channels))
		cmd.Stdin = bytes.NewReader(scaled)
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			log.Printf("voice: aplay: %v", err)
		}
	}()
}

// PlayWAV plays a PCM WAV file's contents.
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

// ParseWAV returns the 16-bit PCM samples of a WAV file.
func ParseWAV(b []byte) (pcm []byte, rate, channels int, err error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, 0, errors.New("not a WAV file")
	}
	bits := 0
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8:]
		if size > len(body) || size < 0 {
			size = len(body) // streamed WAVs may carry a placeholder size
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, 0, errors.New("bad fmt chunk")
			}
			channels = int(binary.LittleEndian.Uint16(body[2:]))
			rate = int(binary.LittleEndian.Uint32(body[4:]))
			bits = int(binary.LittleEndian.Uint16(body[14:]))
		case "data":
			if bits != 16 {
				return nil, 0, 0, fmt.Errorf("%d-bit WAV not supported", bits)
			}
			return body[:size], rate, channels, nil
		}
		p += 8 + size + size%2
	}
	return nil, 0, 0, errors.New("WAV has no data")
}

// Tone synthesises notes (Hz, ms) as 16 kHz mono PCM with short fades.
func Tone(notes ...[2]float64) []byte {
	var out []byte
	for _, n := range notes {
		count := int(n[1] * SampleRate / 1000)
		for i := 0; i < count; i++ {
			env := math.Min(1, math.Min(float64(i), float64(count-i))/(SampleRate*0.008))
			v := 0.0
			if n[0] > 0 {
				v = math.Sin(2*math.Pi*n[0]*float64(i)/SampleRate) * 9000 * env
			}
			out = binary.LittleEndian.AppendUint16(out, uint16(int16(v)))
		}
	}
	return out
}
