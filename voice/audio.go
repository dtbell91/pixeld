// Package voice makes the light a Home Assistant voice satellite over the ESPHome API: wake word detected
// on the light (microWakeWord via mww), audio streamed to Home Assistant, replies and announcements played
// on the speaker, state shown on the panel.
package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
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
		// 10 ms periods: arecord's default (a quarter of up to 0.5 s) delivered speech, and its end, in
		// ~125 ms bursts
		cmd := exec.CommandContext(ctx, "arecord", "-q", "-D", "hw:0,0", "-c", fmt.Sprint(captureCh), "-r",
			fmt.Sprint(SampleRate), "-f", "S16_LE", "-t", "raw", "--period-time=10000", "--buffer-time=200000")
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

func amp(on bool) {
	v := "0"
	if on {
		v = "1"
	}
	os.WriteFile(ampGPIO, []byte(v), 0)
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
