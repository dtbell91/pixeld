package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"pixeld/cards"
	api "pixeld/esphomeapi"
)

// Feature flags (aioesphomeapi model.VoiceAssistantFeature / MediaPlayerEntityFeature).
const (
	featVoiceAssistant    = 1 << 0
	featAPIAudio          = 1 << 2
	featTimers            = 1 << 3
	featAnnounce          = 1 << 4
	featStartConversation = 1 << 5

	mpPause       = 1 << 0
	mpVolumeSet   = 1 << 2
	mpVolumeMute  = 1 << 3
	mpPlayMedia   = 1 << 9
	mpStop        = 1 << 12
	mpPlay        = 1 << 14
	mpAnnounce    = 1 << 20
	mpEnqueue     = 1 << 21
	mpVolumeStep  = 1 << 10
	mediaKey      = 1
	maxActiveWake = 2
	timerRingMax  = 30 * time.Second
)

// Phase is what the satellite is doing (drives the panel and status LED).
type Phase int

const (
	Idle Phase = iota
	Listening
	Thinking
	Speaking
	Error
	TimerRinging
)

// Config persisted on the light.
type Config struct {
	ActiveWakeWords []string `json:"active_wake_words"`
	Volume          float64  `json:"volume"`
	SpeakerMuted    bool     `json:"speaker_muted"` // media player mute
	MicMuted        bool     `json:"mic_muted"`     // privacy: not listening at all
}

type Satellite struct {
	Name, FriendlyName, MAC, Version string
	ConfigPath                       string
	OnPhase                          func(Phase) // panel / LED feedback
	OnTimers                         func()      // the running timers changed (redraw the overlay)

	models   map[string]WakeModel
	detector *Detector
	speaker  *Speaker

	mu           sync.Mutex
	conn         *api.Conn
	cfg          Config
	phase        Phase
	streaming    bool
	pipeline     bool
	ttsURL       string
	ttsPlayed    bool
	continueConv bool
	ringing      bool
	ringName     string               // the timer that's ringing
	musicState   api.MediaPlayerState // idle, playing or paused
	announcing   int                  // announcements playing (shown as the media player's state meanwhile)
	queue        []string             // music URLs enqueued after the current one
	musicGen     int                  // which playMusic is current

	// shown on the panel
	heard, reply    string // what was understood / is being said back (or announced)
	notice          notice
	timers          map[string]*voiceTimer
	visualsBusy     atomic.Bool // Show is drawing (the timer overlay steps aside)
	timerFocus      string      // the timer just set or changed: full screen until timerFocusUntil
	timerFocusUntil time.Time
}

func NewSatellite(modelDir, mwwBin string) *Satellite {
	s := &Satellite{models: LoadModels(modelDir), cfg: Config{Volume: 0.8}, musicState: api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE,
		timers: map[string]*voiceTimer{}}
	s.detector = NewDetector(mwwBin, s.models, s.onWake)
	return s
}

func (s *Satellite) loadConfig() {
	if b, err := os.ReadFile(s.ConfigPath); err == nil {
		json.Unmarshal(b, &s.cfg)
	}
	if len(s.cfg.ActiveWakeWords) == 0 {
		if _, ok := s.models["okay_nabu"]; ok {
			s.cfg.ActiveWakeWords = []string{"okay_nabu"}
		}
	}
}

func (s *Satellite) saveConfig() {
	b, _ := json.Marshal(s.cfg)
	tmp := s.ConfigPath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.ConfigPath)
	}
}

// Run starts audio capture and wake word detection; serve the API separately (Handler).
func (s *Satellite) Run(ctx context.Context) {
	s.loadConfig()
	s.speaker = NewSpeaker(s.cfg.Volume)
	s.speaker.SetMuted(s.cfg.SpeakerMuted)
	s.detector.SetActive(s.cfg.ActiveWakeWords)
	Capture(ctx, s.onAudio)
}

func (s *Satellite) onAudio(pcm []byte) {
	s.mu.Lock()
	muted, streaming, conn := s.cfg.MicMuted, s.streaming, s.conn
	s.mu.Unlock()
	if muted {
		return
	}
	if streaming && conn != nil {
		conn.Send(&api.VoiceAssistantAudio{Data: pcm})
		return
	}
	s.detector.Feed(pcm)
}

// Phase is what the satellite is doing now.
func (s *Satellite) Phase() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// duckFor is how loud music stays while the satellite is busy.
func duckFor(p Phase) float64 {
	switch p {
	case Listening:
		return 0.1
	case Thinking, TimerRinging:
		return 0.2
	case Speaking, Error:
		return 0.3
	}
	return 1
}

func (s *Satellite) setPhase(p Phase) {
	s.mu.Lock()
	changed := s.phase != p
	s.phase = p
	s.mu.Unlock()
	if s.speaker != nil {
		s.speaker.SetDuck(duckFor(p))
	}
	if changed && s.OnPhase != nil {
		s.OnPhase(p)
	}
}

// ---- pipeline ----

func (s *Satellite) onWake(m WakeModel) {
	log.Printf("voice: wake word %q", m.Phrase)
	s.start(m.Phrase, true)
}

// ToggleMic switches listening off (privacy) or back on; returns the new muted state.
func (s *Satellite) ToggleMic() bool {
	s.mu.Lock()
	s.cfg.MicMuted = !s.cfg.MicMuted
	muted := s.cfg.MicMuted
	s.streaming = false
	s.saveConfig()
	s.notice = notice{kind: noticeMic, on: !muted, at: time.Now()}
	s.mu.Unlock()
	log.Printf("voice: microphone %s", map[bool]string{true: "muted", false: "on"}[muted])
	if muted {
		s.speaker.PlayPCM(Tone([2]float64{660, 90}, [2]float64{440, 140}), SampleRate, 1, nil)
	} else {
		s.speaker.PlayPCM(Tone([2]float64{440, 90}, [2]float64{660, 140}), SampleRate, 1, nil)
	}
	return muted
}

// MicMuted reports the privacy mute.
func (s *Satellite) MicMuted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.MicMuted
}

// PushToTalk starts a conversation without a wake word (button), or stops a ringing timer.
func (s *Satellite) PushToTalk() {
	if s.stopRinging() {
		return
	}
	s.start("", true)
}

func (s *Satellite) start(phrase string, chime bool) {
	s.mu.Lock()
	if s.pipeline || s.conn == nil || s.cfg.MicMuted {
		busy, noConn := s.pipeline, s.conn == nil
		s.mu.Unlock()
		if noConn && !busy {
			log.Printf("voice: not connected to Home Assistant")
			s.speaker.PlayPCM(Tone([2]float64{300, 120}, [2]float64{0, 60}, [2]float64{300, 120}), SampleRate, 1, nil)
		}
		return
	}
	s.pipeline, s.ttsURL, s.ttsPlayed, s.continueConv = true, "", false, false
	s.heard, s.reply = "", ""
	conn := s.conn
	s.mu.Unlock()
	s.setPhase(Listening)
	begin := func() {
		s.mu.Lock()
		s.streaming = true
		s.mu.Unlock()
		conn.Send(&api.VoiceAssistantRequest{Start: true, WakeWordPhrase: phrase})
	}
	if chime {
		s.speaker.PlayPCM(Tone([2]float64{880, 70}, [2]float64{1320, 90}), SampleRate, 1, begin)
	} else {
		begin()
	}
}

func (s *Satellite) finish() {
	s.mu.Lock()
	cont := s.continueConv
	s.continueConv, s.streaming, s.pipeline = false, false, false
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		conn.Send(&api.VoiceAssistantAnnounceFinished{})
	}
	if cont {
		time.AfterFunc(300*time.Millisecond, func() { s.start("", false) })
		return
	}
	s.setPhase(Idle)
}

// playURL fetches a WAV from Home Assistant and plays it, then calls done.
func (s *Satellite) playURL(url string, done func()) {
	go func() {
		data, err := fetch(url)
		if err == nil {
			err = s.speaker.PlayWAV(data, done)
		} else if done != nil {
			done()
		}
		if err != nil {
			log.Printf("voice: play %s: %v", url, err)
		}
	}()
}

func fetch(url string) ([]byte, error) {
	c := http.Client{Timeout: 30 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	return io.ReadAll(io.LimitReader(r.Body, 16<<20))
}

func (s *Satellite) playTTS() {
	s.mu.Lock()
	url, played := s.ttsURL, s.ttsPlayed
	if url == "" || played {
		s.mu.Unlock()
		return
	}
	s.ttsPlayed = true
	s.mu.Unlock()
	s.setPhase(Speaking)
	s.playURL(url, s.finish)
}

func (s *Satellite) event(ev *api.VoiceAssistantEventResponse) {
	data := map[string]string{}
	for _, d := range ev.Data {
		data[d.Name] = d.Value
	}
	switch ev.EventType {
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_START:
		s.mu.Lock()
		s.ttsURL, s.ttsPlayed, s.pipeline = data["url"], false, true
		s.mu.Unlock()
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_END, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END:
		s.mu.Lock()
		s.streaming = false
		s.mu.Unlock()
		if t := data["text"]; t != "" {
			log.Printf("voice: heard %q", t)
			s.mu.Lock()
			s.heard = cards.Printable(t)
			s.mu.Unlock()
		}
		s.setPhase(Thinking)
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_PROGRESS:
		if data["tts_start_streaming"] == "1" {
			s.playTTS()
		}
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_END:
		s.mu.Lock()
		if data["continue_conversation"] == "1" {
			s.continueConv = true
		}
		if t := data["speech"]; t != "" { // the reply (also in TTS_START's text; order varies when streaming)
			s.reply = cards.Printable(t)
		}
		s.mu.Unlock()
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START:
		if t := data["text"]; t != "" {
			s.mu.Lock()
			s.reply = cards.Printable(t)
			s.mu.Unlock()
		}
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END:
		if u := data["url"]; u != "" {
			s.mu.Lock()
			s.ttsURL = u
			s.mu.Unlock()
		}
		s.playTTS()
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END:
		s.mu.Lock()
		s.streaming = false
		played := s.ttsPlayed
		s.mu.Unlock()
		if !played {
			s.finish()
		}
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR:
		log.Printf("voice: pipeline error %s: %s", data["code"], data["message"])
		s.mu.Lock()
		s.streaming = false
		s.mu.Unlock()
		s.setPhase(Error)
		s.speaker.PlayPCM(Tone([2]float64{440, 150}, [2]float64{330, 220}), SampleRate, 1, nil)
	}
}

func (s *Satellite) timer(ev *api.VoiceAssistantTimerEventResponse) {
	s.timerEvent(ev, time.Now())
	switch ev.EventType {
	case api.VoiceAssistantTimerEvent_VOICE_ASSISTANT_TIMER_FINISHED:
		log.Printf("voice: timer %q finished", ev.Name)
		s.mu.Lock()
		s.ringing, s.ringName = true, cards.Printable(ev.Name)
		s.mu.Unlock()
		s.setPhase(TimerRinging)
		deadline := time.Now().Add(timerRingMax)
		var ring func()
		ring = func() {
			s.mu.Lock()
			still := s.ringing && time.Now().Before(deadline)
			s.mu.Unlock()
			if !still {
				s.stopRinging()
				return
			}
			s.speaker.PlayPCM(Tone([2]float64{1046, 120}, [2]float64{0, 60}, [2]float64{1046, 120}, [2]float64{0, 600}),
				SampleRate, 1, func() { time.AfterFunc(10*time.Millisecond, ring) })
		}
		ring()
	}
}

// stopRinging ends a ringing timer; reports whether one was ringing.
func (s *Satellite) stopRinging() bool {
	s.mu.Lock()
	was := s.ringing
	s.ringing = false
	s.mu.Unlock()
	if was {
		s.speaker.Stop()
		s.setPhase(Idle)
	}
	return was
}

// ---- API handler ----

func (s *Satellite) Connected(c *api.Conn) {
	s.mu.Lock()
	if s.conn != nil && s.conn != c {
		s.conn.Close() // one Home Assistant at a time
	}
	s.conn = c
	s.mu.Unlock()
	log.Printf("voice: Home Assistant connected")
}

func (s *Satellite) Disconnected(c *api.Conn) {
	s.mu.Lock()
	if s.conn == c {
		s.conn, s.streaming, s.pipeline = nil, false, false
	}
	s.mu.Unlock()
	s.setPhase(Idle)
}

func (s *Satellite) mediaState() *api.MediaPlayerStateResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.musicState
	if s.announcing > 0 {
		st = api.MediaPlayerState_MEDIA_PLAYER_STATE_ANNOUNCING
	}
	return &api.MediaPlayerStateResponse{Key: mediaKey, State: st, Volume: float32(s.cfg.Volume), Muted: s.cfg.SpeakerMuted}
}

// sendMediaState tells Home Assistant the media player's state.
func (s *Satellite) sendMediaState() {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c != nil {
		c.Send(s.mediaState())
	}
}

func (s *Satellite) setMusicState(st api.MediaPlayerState) {
	s.mu.Lock()
	s.musicState = st
	s.mu.Unlock()
	s.sendMediaState()
}

// setAnnouncing counts an announcement starting (+1) or ending (-1).
func (s *Satellite) setAnnouncing(d int) {
	s.mu.Lock()
	s.announcing = max(0, s.announcing+d)
	s.mu.Unlock()
	s.sendMediaState()
}

func (s *Satellite) Message(c *api.Conn, m proto.Message) {
	switch req := m.(type) {
	case *api.DeviceInfoRequest:
		c.Send(&api.DeviceInfoResponse{
			Name: s.Name, FriendlyName: s.FriendlyName, MacAddress: s.MAC, EsphomeVersion: "2025.9.0",
			Model: "H8630 Gaming Pixel Light", Manufacturer: "Govee (custom firmware)",
			ProjectName: "pixeld.voice", ProjectVersion: s.Version,
			VoiceAssistantFeatureFlags: featVoiceAssistant | featAPIAudio | featTimers | featAnnounce | featStartConversation,
		})
	case *api.ListEntitiesRequest:
		formats := []*api.MediaPlayerSupportedFormat{
			{Format: "wav", SampleRate: MusicRate, NumChannels: 1, SampleBytes: 2, Purpose: api.MediaPlayerFormatPurpose_MEDIA_PLAYER_FORMAT_PURPOSE_DEFAULT},
			{Format: "wav", SampleRate: SampleRate, NumChannels: 1, SampleBytes: 2, Purpose: api.MediaPlayerFormatPurpose_MEDIA_PLAYER_FORMAT_PURPOSE_ANNOUNCEMENT},
		}
		c.Send(&api.ListEntitiesMediaPlayerResponse{
			ObjectId: "speaker", Key: mediaKey, Name: "Speaker", SupportsPause: true, SupportedFormats: formats,
			FeatureFlags: mpPause | mpVolumeSet | mpVolumeMute | mpPlayMedia | mpStop | mpPlay | mpAnnounce | mpEnqueue | mpVolumeStep,
		}, &api.ListEntitiesDoneResponse{})
	case *api.SubscribeStatesRequest:
		c.Send(s.mediaState())
	case *api.MediaPlayerCommandRequest:
		s.mediaCommand(req)
	case *api.VoiceAssistantConfigurationRequest:
		var avail []*api.VoiceAssistantWakeWord
		for _, w := range s.models {
			avail = append(avail, &api.VoiceAssistantWakeWord{Id: w.ID, WakeWord: w.Phrase, TrainedLanguages: w.Languages})
		}
		c.Send(&api.VoiceAssistantConfigurationResponse{AvailableWakeWords: avail,
			ActiveWakeWords: s.detector.Active(), MaxActiveWakeWords: maxActiveWake})
	case *api.VoiceAssistantSetConfiguration:
		ids := req.ActiveWakeWords
		if len(ids) > maxActiveWake {
			ids = ids[:maxActiveWake]
		}
		s.detector.SetActive(ids)
		s.mu.Lock()
		s.cfg.ActiveWakeWords = s.detector.Active()
		s.saveConfig()
		s.mu.Unlock()
	case *api.VoiceAssistantResponse:
		if req.Error {
			log.Printf("voice: Home Assistant refused the pipeline")
			s.finish()
		}
	case *api.VoiceAssistantEventResponse:
		s.event(req)
	case *api.VoiceAssistantAnnounceRequest:
		s.mu.Lock()
		s.reply = cards.Printable(req.Text)
		s.mu.Unlock()
		s.announce(req.PreannounceMediaId, req.MediaId, req.StartConversation)
	case *api.VoiceAssistantTimerEventResponse:
		s.timer(req)
	case *api.SubscribeVoiceAssistantRequest, *api.SubscribeHomeassistantServicesRequest,
		*api.SubscribeHomeAssistantStatesRequest, *api.SubscribeLogsRequest:
		// nothing to send
	default:
		log.Printf("voice: unhandled %s", m.ProtoReflect().Descriptor().Name())
	}
}

func (s *Satellite) announce(pre, url string, startConversation bool) {
	s.setPhase(Speaking)
	s.setAnnouncing(1)
	after := func() {
		s.setAnnouncing(-1)
		s.mu.Lock()
		s.continueConv = startConversation
		s.mu.Unlock()
		s.finish()
	}
	if pre != "" {
		s.playURL(pre, func() { s.playURL(url, after) })
		return
	}
	s.playURL(url, after)
}

func (s *Satellite) mediaCommand(req *api.MediaPlayerCommandRequest) {
	if req.HasVolume {
		s.setVolume(float64(req.Volume))
	}
	if req.HasMediaUrl && req.MediaUrl != "" {
		url := req.MediaUrl
		switch {
		case req.HasAnnouncement && req.Announcement: // over the music (ducked), like a reply
			s.setAnnouncing(1)
			s.playURL(url, func() { s.setAnnouncing(-1) })
		case req.HasCommand && req.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_ENQUEUE && s.speaker.MusicActive():
			s.mu.Lock()
			s.queue = append(s.queue, url)
			s.mu.Unlock()
		default:
			s.mu.Lock()
			s.queue = nil
			s.mu.Unlock()
			s.playMusic(url)
		}
		return
	}
	if !req.HasCommand {
		return
	}
	switch req.Command {
	case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_PLAY:
		s.pauseMusic(false)
	case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_PAUSE:
		s.pauseMusic(true)
	case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_TOGGLE:
		s.mu.Lock()
		playing := s.musicState == api.MediaPlayerState_MEDIA_PLAYER_STATE_PLAYING
		s.mu.Unlock()
		s.pauseMusic(playing)
	case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_STOP:
		s.mu.Lock()
		s.queue = nil
		s.musicGen++
		s.mu.Unlock()
		s.speaker.StopMusic()
		s.speaker.Stop()
		s.setMusicState(api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE)
	case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_CLEAR_PLAYLIST:
		s.mu.Lock()
		s.queue = nil
		s.mu.Unlock()
	case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_VOLUME_UP, api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_VOLUME_DOWN:
		s.mu.Lock()
		v := s.cfg.Volume
		s.mu.Unlock()
		if req.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_VOLUME_UP {
			s.setVolume(v + 0.1)
		} else {
			s.setVolume(v - 0.1)
		}
	case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_MUTE, api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_UNMUTE:
		s.mu.Lock()
		s.cfg.SpeakerMuted = req.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_MUTE
		s.saveConfig()
		s.notice = notice{kind: noticeVolume, level: s.cfg.Volume, on: !s.cfg.SpeakerMuted, at: time.Now()}
		s.mu.Unlock()
		s.speaker.SetMuted(s.cfg.SpeakerMuted)
		s.sendMediaState()
	}
}

func (s *Satellite) setVolume(v float64) {
	v = math.Max(0, math.Min(1, v))
	s.mu.Lock()
	s.cfg.Volume = v
	s.saveConfig()
	s.notice = notice{kind: noticeVolume, level: v, on: !s.cfg.SpeakerMuted, at: time.Now()}
	s.mu.Unlock()
	s.speaker.SetVolume(v)
	s.sendMediaState()
}

// playMusic streams url (Home Assistant converts it to the WAV format we asked for), then whatever was
// enqueued after it.
func (s *Satellite) playMusic(url string) {
	s.mu.Lock()
	s.musicGen++
	gen := s.musicGen
	s.mu.Unlock()
	log.Printf("voice: playing music")
	s.setMusicState(api.MediaPlayerState_MEDIA_PLAYER_STATE_PLAYING)
	s.speaker.PlayStream(url, func(err error) {
		if errors.Is(err, ErrStopped) {
			return // stopped or replaced: that set the state
		}
		if err != nil {
			log.Printf("voice: music stream: %v", err)
		}
		s.mu.Lock()
		if gen != s.musicGen {
			s.mu.Unlock()
			return
		}
		var next string
		if len(s.queue) > 0 {
			next, s.queue = s.queue[0], s.queue[1:]
		}
		s.mu.Unlock()
		if next != "" {
			s.playMusic(next)
			return
		}
		s.setMusicState(api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE)
	})
}

func (s *Satellite) pauseMusic(paused bool) {
	if !s.speaker.MusicActive() {
		return
	}
	s.speaker.PauseMusic(paused)
	if paused {
		s.setMusicState(api.MediaPlayerState_MEDIA_PLAYER_STATE_PAUSED)
	} else {
		s.setMusicState(api.MediaPlayerState_MEDIA_PLAYER_STATE_PLAYING)
	}
}
