package voice

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testSpeaker has no output goroutine: tests call mix() themselves.
func testSpeaker() *Speaker {
	return &Speaker{volume: 1, duck: 1, gain: 1, wake: make(chan struct{}, 1)}
}

// wavStream is a streamed WAV (placeholder sizes, as ffmpeg writes to a pipe) of a constant level.
func wavStream(rate, channels, frames int, level int16) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(0xFFFFFFFF))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(channels), uint32(rate),
		uint32(rate * channels * 2), uint16(channels * 2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("LIST")
	binary.Write(&b, binary.LittleEndian, uint32(4))
	b.WriteString("INFO")
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(0xFFFFFFFF))
	for i := 0; i < frames*channels; i++ {
		binary.Write(&b, binary.LittleEndian, level)
	}
	return b.Bytes()
}

func TestReadWAVHeaderStreamed(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader(wavStream(44100, 2, 10, 1000)))
	rate, ch, err := readWAVHeader(r)
	if err != nil || rate != 44100 || ch != 2 {
		t.Fatalf("header: %d Hz %d ch %v", rate, ch, err)
	}
	var v int16
	if binary.Read(r, binary.LittleEndian, &v); v != 1000 {
		t.Fatalf("not at the samples: %d", v)
	}
}

func TestResampler(t *testing.T) {
	for _, rate := range []int{16000, 22050, 44100, 48000} {
		in := make([]int16, rate) // one second, fed in uneven chunks
		for i := range in {
			in[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		}
		r := newResampler(rate)
		var out []int16
		for p := 0; p < len(in); p += 777 {
			out = append(out, r.push(in[p:min(p+777, len(in))])...)
		}
		if d := len(out) - OutRate; d < -4 || d > 1 { // the very end can't be interpolated
			t.Errorf("%d Hz: %d samples out for 1 s", rate, len(out))
		}
		// still a 440 Hz sine of the same level
		for i := 100; i < len(out); i += 997 {
			want := 10000 * math.Sin(2*math.Pi*440*float64(i)/OutRate)
			if math.Abs(float64(out[i])-want) > 400 {
				t.Errorf("%d Hz: sample %d = %d, want ~%.0f", rate, i, out[i], want)
				break
			}
		}
	}
}

func TestMixVoiceOverDuckedMusic(t *testing.T) {
	s := testSpeaker()
	st := &stream{blocks: make(chan []int16, 200)}
	for i := 0; i < 200; i++ {
		b := make([]int16, blockSamples)
		for j := range b {
			b[j] = 10000
		}
		st.blocks <- b
	}
	s.music = st
	out, active := s.mix()
	if !active || out[0] != 0 || out[blockSamples-1] < 3000 {
		t.Fatalf("music doesn't fade in: %d..%d %v", out[0], out[blockSamples-1], active)
	}
	for i := 0; i < 3; i++ {
		out, _ = s.mix()
	}
	if out[0] != 10000 {
		t.Fatalf("music alone: %d", out[0])
	}
	done := make(chan struct{})
	s.PlayPCM(Tone([2]float64{0, 300}), SampleRate, 1, func() { close(done) }) // 300 ms of silence as the voice
	for i := 0; i < 15; i++ {
		out, _ = s.mix()
	}
	if want := int16(10000 * voiceDuck); math.Abs(float64(out[0]-want)) > 50 {
		t.Fatalf("music under a voice sound: %d, want ~%d", out[0], want)
	}
	for i := 0; i < 40; i++ {
		out, _ = s.mix()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("voice sound's done not called")
	}
	if out[0] != 10000 {
		t.Fatalf("music not back up after the voice: %d", out[0])
	}
	s.SetDuck(0.1)
	for i := 0; i < 30; i++ {
		out, _ = s.mix()
	}
	if math.Abs(float64(out[0])-1000) > 50 {
		t.Fatalf("music while listening: %d, want ~1000", out[0])
	}
	s.SetVolume(0.5)
	s.SetDuck(1)
	for i := 0; i < 30; i++ {
		out, _ = s.mix()
	}
	if math.Abs(float64(out[0])-2500) > 50 { // gain = volume²
		t.Fatalf("at half volume: %d, want ~2500", out[0])
	}
}

func TestStreamPlaysPausesAndEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(wavStream(16000, 2, 1600, 3000)) // 100 ms, stereo, 16 kHz
	}))
	defer srv.Close()
	s := testSpeaker()
	ended := make(chan error, 1)
	s.PlayStream(srv.URL, func(err error) { ended <- err })
	time.Sleep(200 * time.Millisecond) // let it fetch and decode
	s.PauseMusic(true)
	if _, active := s.mix(); active {
		t.Fatal("paused music still playing")
	}
	s.PauseMusic(false)
	heard, full := 0, 0
	for i := 0; i < 50; i++ {
		out, _ := s.mix()
		if out[blockSamples/2] > 100 {
			heard++
		}
		if math.Abs(float64(out[blockSamples/2])-3000) < 60 {
			full++
		}
		if !s.MusicActive() {
			break
		}
	}
	if heard != 10 || full < 7 {
		t.Fatalf("%d blocks of music (%d at full level) for 100 ms", heard, full)
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("end of stream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("done not called at the end")
	}

	s.PlayStream(srv.URL, func(err error) { ended <- err })
	s.StopMusic()
	if err := <-ended; !errors.Is(err, ErrStopped) {
		t.Fatalf("stop: %v", err)
	}
}

func TestStreamNotWAV(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ID3 an mp3"))
	}))
	defer srv.Close()
	s := testSpeaker()
	ended := make(chan error, 1)
	s.PlayStream(srv.URL, func(err error) { ended <- err })
	deadline := time.Now().Add(time.Second)
	for s.MusicActive() && time.Now().Before(deadline) {
		s.mix()
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-ended; err == nil || errors.Is(err, ErrStopped) {
		t.Fatalf("non-WAV stream: %v", err)
	}
}

func TestStopAndPauseFade(t *testing.T) {
	s := testSpeaker()
	st := &stream{blocks: make(chan []int16, 200), level: 1, cancel: func() {}}
	for i := 0; i < 200; i++ {
		b := make([]int16, blockSamples)
		for j := range b {
			b[j] = 10000
		}
		st.blocks <- b
	}
	cancelled := false
	st.cancel = func() { cancelled = true }
	s.music = st
	s.mix()
	s.PauseMusic(true)
	out, active := s.mix()
	if !active || out[0] < 9000 || out[blockSamples-1] > 7000 {
		t.Fatalf("pause doesn't fade: %d..%d", out[0], out[blockSamples-1])
	}
	for i := 0; i < 3; i++ {
		s.mix()
	}
	held := len(st.blocks)
	if _, active := s.mix(); active || len(st.blocks) != held {
		t.Fatal("paused stream still read")
	}
	s.PauseMusic(false)
	for i := 0; i < 3; i++ {
		s.mix()
	}
	s.StopMusic()
	if s.MusicActive() || cancelled {
		t.Fatal("stop: still active, or cut off instead of fading")
	}
	out, active = s.mix()
	if !active || out[0] < 9000 || out[blockSamples-1] > 7000 {
		t.Fatalf("stop doesn't fade: %d..%d", out[0], out[blockSamples-1])
	}
	for i := 0; i < 3; i++ {
		s.mix()
	}
	if !cancelled || s.active() {
		t.Fatal("faded-out stream not dropped")
	}
}

func TestVoiceStreamStartsBeforeDownloadEnds(t *testing.T) {
	wav := wavStream(16000, 1, 16000, 5000) // 1 s of a reply
	head, body := wav[:44+12], wav[44+12:]  // RIFF + fmt + LIST/INFO + data headers
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(head)
		for p := 0; p < len(body); p += 3200 { // 100 ms of audio every 50 ms: faster than real time, but not all at once
			w.Write(body[p:min(p+3200, len(body))])
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()
	s := testSpeaker()
	done := make(chan struct{})
	start := time.Now()
	s.PlayVoiceStream(srv.URL, func() { close(done) })
	var firstSound time.Duration
	heard := 0
	for i := 0; i < 400; i++ {
		out, active := s.mix()
		if !active {
			break
		}
		if out[blockSamples/2] > 1000 {
			if heard == 0 {
				firstSound = time.Since(start)
			}
			heard++
		}
		time.Sleep(10 * time.Millisecond) // real time, like the output
	}
	if firstSound == 0 || firstSound > 400*time.Millisecond {
		t.Fatalf("first sound after %v; the download takes ~500 ms", firstSound)
	}
	if heard < 95 || heard > 102 {
		t.Fatalf("%d blocks of a 1 s reply", heard)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("done not called")
	}
}
