package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

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
	mpState      api.MediaPlayerState
}

func NewSatellite(modelDir, mwwBin string) *Satellite {
	s := &Satellite{models: LoadModels(modelDir), cfg: Config{Volume: 0.8}, mpState: api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE}
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

func (s *Satellite) setPhase(p Phase) {
	s.mu.Lock()
	changed := s.phase != p
	s.phase = p
	s.mu.Unlock()
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
		}
		s.setPhase(Thinking)
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_PROGRESS:
		if data["tts_start_streaming"] == "1" {
			s.playTTS()
		}
	case api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_END:
		if data["continue_conversation"] == "1" {
			s.mu.Lock()
			s.continueConv = true
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
	switch ev.EventType {
	case api.VoiceAssistantTimerEvent_VOICE_ASSISTANT_TIMER_FINISHED:
		log.Printf("voice: timer %q finished", ev.Name)
		s.mu.Lock()
		s.ringing = true
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
	return &api.MediaPlayerStateResponse{Key: mediaKey, State: s.mpState, Volume: float32(s.cfg.Volume), Muted: s.cfg.SpeakerMuted}
}

func (s *Satellite) setMediaState(st api.MediaPlayerState) {
	s.mu.Lock()
	s.mpState = st
	c := s.conn
	s.mu.Unlock()
	if c != nil {
		c.Send(s.mediaState())
	}
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
			{Format: "wav", SampleRate: SampleRate, NumChannels: 1, SampleBytes: 2, Purpose: api.MediaPlayerFormatPurpose_MEDIA_PLAYER_FORMAT_PURPOSE_DEFAULT},
			{Format: "wav", SampleRate: SampleRate, NumChannels: 1, SampleBytes: 2, Purpose: api.MediaPlayerFormatPurpose_MEDIA_PLAYER_FORMAT_PURPOSE_ANNOUNCEMENT},
		}
		c.Send(&api.ListEntitiesMediaPlayerResponse{
			ObjectId: "speaker", Key: mediaKey, Name: "Speaker", SupportsPause: true, SupportedFormats: formats,
			FeatureFlags: mpPause | mpVolumeSet | mpVolumeMute | mpPlayMedia | mpStop | mpPlay | mpAnnounce,
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
	s.setMediaState(api.MediaPlayerState_MEDIA_PLAYER_STATE_ANNOUNCING)
	after := func() {
		s.setMediaState(api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE)
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
		s.mu.Lock()
		s.cfg.Volume = float64(req.Volume)
		s.saveConfig()
		s.mu.Unlock()
		s.speaker.SetVolume(float64(req.Volume))
		s.setMediaState(s.mediaState().State)
	}
	if req.HasMediaUrl && req.MediaUrl != "" {
		url := req.MediaUrl
		if !strings.Contains(strings.ToLower(url), ".wav") && !strings.Contains(url, "tts_proxy") {
			log.Printf("voice: only WAV media is supported for now (%s)", url)
		}
		st := api.MediaPlayerState_MEDIA_PLAYER_STATE_PLAYING
		if req.HasAnnouncement && req.Announcement {
			st = api.MediaPlayerState_MEDIA_PLAYER_STATE_ANNOUNCING
		}
		s.setMediaState(st)
		s.playURL(url, func() { s.setMediaState(api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE) })
	}
	if req.HasCommand {
		switch req.Command {
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_STOP, api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_PAUSE:
			s.speaker.Stop()
			s.setMediaState(api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE)
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_MUTE, api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_UNMUTE:
			s.mu.Lock()
			s.cfg.SpeakerMuted = req.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_MUTE
			s.saveConfig()
			s.mu.Unlock()
			s.speaker.SetMuted(s.cfg.SpeakerMuted)
			s.setMediaState(s.mediaState().State)
		}
	}
}
