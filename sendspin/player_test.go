package sendspin

import (
	"encoding/binary"
	"testing"
	"time"
)

// syncedSession has a clock mapping where server time = local time + offset.
func syncedSession(offset int64) *session {
	s := &session{filter: newTimeFilter()}
	s.filter.update(offset, 100, 1_000_000)
	s.filter.update(offset, 100, 2_000_000)
	return s
}

// ramp is a mono PCM chunk whose samples count up from first.
func ramp(first, n int) []byte {
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(first+i)))
	}
	return b
}

func localAt(us int64) time.Time { return epoch.Add(time.Duration(us) * time.Microsecond) }

func TestFillFollowsTimeline(t *testing.T) {
	s := syncedSession(5000)
	p := newPlayer(nil)
	p.start(s, streamFormat{Codec: "pcm", SampleRate: 48000, Channels: 1, BitDepth: 16})
	// 3 chunks of 20 ms (960 samples) starting at server time 10 s
	for i := 0; i < 3; i++ {
		p.chunk(s, 10_000_000+int64(i)*20_000, ramp(i*960, 960))
	}
	dst := make([]int16, 480)
	// due at server 10.005 s = local 10.000 s: sample 240 of the first chunk
	if !p.Fill(dst, localAt(10_000_000)) {
		t.Fatal("nothing filled")
	}
	if dst[0] != 240 || dst[479] != 719 {
		t.Fatalf("got %d..%d, want 240..719", dst[0], dst[479])
	}
	// the next block continues seamlessly, across the chunk boundary
	p.Fill(dst, localAt(10_010_000))
	if dst[0] != 720 || dst[479] != 1199 {
		t.Fatalf("got %d..%d, want 720..1199", dst[0], dst[479])
	}
}

func TestFillStartsWithSilenceBeforeTheFirstChunk(t *testing.T) {
	s := syncedSession(0)
	p := newPlayer(nil)
	p.start(s, streamFormat{Codec: "pcm", SampleRate: 48000, Channels: 1, BitDepth: 16})
	p.chunk(s, 10_005_000, ramp(1, 960)) // starts 5 ms (240 samples) into the block
	dst := make([]int16, 480)
	for i := range dst {
		dst[i] = -1
	}
	if !p.Fill(dst, localAt(10_000_000)) {
		t.Fatal("nothing filled")
	}
	if dst[239] != 0 || dst[240] != 1 {
		t.Fatalf("got %d, %d at the start", dst[239], dst[240])
	}
}

func TestFillCorrectsDriftASampleAtATime(t *testing.T) {
	s := syncedSession(0)
	p := newPlayer(nil)
	p.start(s, streamFormat{Codec: "pcm", SampleRate: 48000, Channels: 1, BitDepth: 16})
	p.chunk(s, 10_000_000, ramp(0, 48000))
	dst := make([]int16, 480)
	p.Fill(dst, localAt(10_000_000))
	// the output runs 0.3 ms fast: the next block is due 0.3 ms before where the cursor is
	p.Fill(dst, localAt(10_009_700))
	if dst[0] != 480 || dst[1] != 480 {
		t.Fatalf("expected a repeated sample, got %d %d", dst[0], dst[1])
	}
	// and 0.3 ms slow: one sample skipped (the cursor is at 959 after 1 + 479 samples)
	p.Fill(dst, localAt(10_020_300))
	if want := int16(960); dst[0] != want {
		t.Fatalf("expected a skipped sample: got %d, want %d", dst[0], want)
	}
}

func TestFillJumpsOnLargeErrors(t *testing.T) {
	s := syncedSession(0)
	p := newPlayer(nil)
	p.start(s, streamFormat{Codec: "pcm", SampleRate: 48000, Channels: 1, BitDepth: 16})
	p.chunk(s, 10_000_000, ramp(0, 48000))
	dst := make([]int16, 480)
	p.Fill(dst, localAt(10_000_000))
	p.Fill(dst, localAt(10_110_000)) // 100 ms later than the cursor: straight to sample 5280
	if dst[0] != 5280 {
		t.Fatalf("got %d, want 5280", dst[0])
	}
}

func TestFillDropsLateChunksAndStereo(t *testing.T) {
	s := syncedSession(0)
	p := newPlayer(nil)
	p.start(s, streamFormat{Codec: "pcm", SampleRate: 48000, Channels: 2, BitDepth: 16})
	st := make([]byte, 4*960)
	for i := 0; i < 960; i++ {
		binary.LittleEndian.PutUint16(st[4*i:], uint16(100))
		binary.LittleEndian.PutUint16(st[4*i+2:], uint16(300))
	}
	p.chunk(s, 9_000_000, st)  // long past
	p.chunk(s, 10_000_000, st) // due
	dst := make([]int16, 480)
	if !p.Fill(dst, localAt(10_000_000)) || dst[0] != 200 {
		t.Fatalf("got %d (want the downmix, 200)", dst[0])
	}
}

func TestOutputDelayPlaysEarlier(t *testing.T) {
	s := syncedSession(0)
	p := newPlayer(nil)
	p.start(s, streamFormat{Codec: "pcm", SampleRate: 48000, Channels: 1, BitDepth: 16})
	p.setOutputDelay(10) // the sound leaves the device 10 ms after the port: send it 10 ms early
	p.chunk(s, 10_000_000, ramp(0, 48000))
	dst := make([]int16, 480)
	p.Fill(dst, localAt(9_990_000))
	if dst[0] != 0 {
		t.Fatalf("got %d, want 0", dst[0])
	}
}

func TestPairingTokenMatchesSpec(t *testing.T) {
	payload := make([]byte, 64)
	for i := range payload {
		payload[i] = byte(i)
		if i >= 32 {
			payload[i] = byte(0xe0 + i - 32)
		}
	}
	want := "SP:0AAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYP6BYPC4PSOLZXH5DU6V97M5XXO74HR6LZ7J5PW674PT6X37T6757Y"
	if got := encodeToken('0', payload); got != want {
		t.Fatalf("token\n got %s\nwant %s", got, want)
	}
}
