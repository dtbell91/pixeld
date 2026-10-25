package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"pixeld/bt"
	"pixeld/cards"
	"pixeld/content"
	"pixeld/ddp"
	"pixeld/esphomeapi"
	"pixeld/game"
	"pixeld/hw"
	"pixeld/panel"
	"pixeld/player"
	"pixeld/selfupdate"
	"pixeld/sendspin"
	"pixeld/voice"
)

var reKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

type savedState struct {
	On         bool `json:"on"`
	Brightness int  `json:"brightness"`
}

type playlistMsg struct {
	Items []struct {
		Name     string          `json:"name"`
		Key      string          `json:"key"`  // content (image) item
		Card     json.RawMessage `json:"card"` // or a card drawn here, e.g. {"type":"clock",...}
		Duration float64         `json:"duration"`
	} `json:"items"`
}

type showMsg struct {
	Key  string  `json:"key"`
	Hold float64 `json:"hold"`
	Name string  `json:"name"`
}

type setMsg struct {
	On         *bool `json:"on"`
	Brightness *int  `json:"brightness"`
	Resume     bool  `json:"resume"`
	Next       bool  `json:"next"`
}

type app struct {
	cfg    config
	client mqtt.Client
	player *player.Player

	mu        sync.Mutex
	frames    map[string][]content.Frame // decoded content by key
	playlist  playlistMsg
	pending   *showMsg // show waiting for its content
	missing   map[string]bool
	lastState []byte
	voice     *voice.Satellite
	sat       atomic.Pointer[voice.Satellite] // = voice, for cards drawing on the player's goroutine
	app       selfupdate.Dir                  // where updated builds go ("" = firmware can't run them)
	confirm   sync.Once                       // a trial build confirms itself once
}

const timerSlot = 10 * time.Second // the timers' own turn in the rotation while any are running

// cardEnv is what playlist cards can show besides their spec: the soonest running timer.
func (a *app) cardEnv() cards.Env {
	return cards.Env{Badge: func(now time.Time) (cards.Badge, bool) {
		if s := a.sat.Load(); s != nil {
			return s.TimerBadge(now)
		}
		return cards.Badge{}, false
	}}
}

func (a *app) topic(s string) string { return a.cfg.prefix + s }

func (a *app) path(parts ...string) string { return filepath.Join(append([]string{a.cfg.dataDir}, parts...)...) }

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// frames returns the decoded content for key from memory or the on-disk cache (nil if absent).
func (a *app) load(key string) []content.Frame {
	if f, ok := a.frames[key]; ok {
		return f
	}
	data, err := os.ReadFile(a.path("content", key))
	if err != nil {
		return nil
	}
	f, err := content.Decode(data)
	if err != nil {
		log.Printf("cached %s unreadable: %v", key[:12], err)
		os.Remove(a.path("content", key))
		return nil
	}
	a.frames[key] = f
	return f
}

// applyPlaylist (a.mu held) builds the rotation from the items whose content is present.
func (a *app) applyPlaylist() {
	items := make([]*player.Item, 0, len(a.playlist.Items))
	keep := map[string]bool{}
	for _, it := range a.playlist.Items {
		dur := time.Duration(it.Duration * float64(time.Second))
		if len(it.Card) > 0 {
			card, err := cards.New(it.Card, a.cardEnv())
			if err != nil {
				log.Printf("playlist item %q: %v", it.Name, err)
				continue
			}
			items = append(items, &player.Item{Name: it.Name, Key: "card:" + it.Name, Duration: dur,
				Render: card.Render, ShowsBadges: card.ShowsBadges})
			continue
		}
		if !reKey.MatchString(it.Key) {
			continue
		}
		keep[it.Key] = true
		f := a.load(it.Key)
		if f == nil {
			a.missing[it.Key] = true
			continue
		}
		items = append(items, &player.Item{Name: it.Name, Key: it.Key, Frames: f, Duration: dur})
	}
	if a.voice != nil && a.voice.HasTimers() {
		items = append(items, &player.Item{Name: "timers", Key: "voice:timers", Duration: timerSlot,
			Render: a.voice.RenderTimers, ShowsBadges: true})
	}
	// forget decoded content no longer referenced (the files stay cached on disk)
	for k := range a.frames {
		if !keep[k] && (a.pending == nil || a.pending.Key != k) {
			delete(a.frames, k)
		}
	}
	a.player.SetPlaylist(items)
}

const keepRecent = 32 // cached items kept beyond the playlist (recent one-off images)

// prune (a.mu held) deletes cached content that the playlist doesn't use, keeping the newest keepRecent.
func (a *app) prune() {
	inUse := map[string]bool{}
	for _, it := range a.playlist.Items {
		inUse[it.Key] = true
	}
	if a.pending != nil {
		inUse[a.pending.Key] = true
	}
	entries, err := os.ReadDir(a.path("content"))
	if err != nil {
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	var others []file
	for _, e := range entries {
		if inUse[e.Name()] || !reKey.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil {
			others = append(others, file{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].mod.After(others[j].mod) })
	for _, f := range others[min(len(others), keepRecent):] {
		os.Remove(a.path("content", f.name))
	}
}

func (a *app) requestMissing() {
	a.mu.Lock()
	keys := make([]string, 0, len(a.missing))
	for k := range a.missing {
		keys = append(keys, k)
	}
	a.mu.Unlock()
	if len(keys) == 0 || !a.client.IsConnected() {
		return
	}
	sort.Strings(keys)
	b, _ := json.Marshal(map[string][]string{"keys": keys})
	a.client.Publish(a.topic("px/need"), 1, false, b)
}

func (a *app) onPlaylist(_ mqtt.Client, m mqtt.Message) {
	var pl playlistMsg
	if len(m.Payload()) == 0 { // retained playlist cleared: empty rotation
		pl = playlistMsg{}
	} else if err := json.Unmarshal(m.Payload(), &pl); err != nil {
		log.Printf("bad playlist: %v", err)
		return
	}
	a.mu.Lock()
	a.playlist = pl
	writeFileAtomic(a.path("playlist.json"), m.Payload())
	a.missing = map[string]bool{}
	a.applyPlaylist()
	a.prune()
	a.mu.Unlock()
	a.requestMissing()
}

func (a *app) onContent(_ mqtt.Client, m mqtt.Message) {
	key := filepath.Base(m.Topic())
	sum := sha256.Sum256(m.Payload())
	if !reKey.MatchString(key) || hex.EncodeToString(sum[:]) != key {
		log.Printf("content %q: hash mismatch, ignored", key)
		return
	}
	f, err := content.Decode(m.Payload())
	if err != nil {
		log.Printf("content %s: %v", key[:12], err)
		return
	}
	writeFileAtomic(a.path("content", key), m.Payload())
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frames[key] = f
	if a.missing[key] {
		delete(a.missing, key)
		a.applyPlaylist()
	}
	if a.pending != nil && a.pending.Key == key {
		a.showNow(*a.pending, f)
		a.pending = nil
	}
}

func (a *app) showNow(s showMsg, f []content.Frame) {
	name := s.Name
	if name == "" {
		name = "image"
	}
	a.player.Interrupt(&player.Item{Name: name, Key: s.Key, Frames: f}, time.Duration(s.Hold*float64(time.Second)))
}

func (a *app) onShow(_ mqtt.Client, m mqtt.Message) {
	var s showMsg
	if err := json.Unmarshal(m.Payload(), &s); err != nil || !reKey.MatchString(s.Key) {
		log.Printf("bad show message")
		return
	}
	a.mu.Lock()
	f := a.load(s.Key)
	if f != nil {
		a.showNow(s, f)
		a.mu.Unlock()
		return
	}
	a.pending = &s
	a.missing[s.Key] = true
	a.mu.Unlock()
	a.requestMissing()
}

func (a *app) onSet(_ mqtt.Client, m mqtt.Message) {
	var s setMsg
	if err := json.Unmarshal(m.Payload(), &s); err != nil {
		log.Printf("bad set message: %v", err)
		return
	}
	if s.Brightness != nil {
		a.player.SetBrightness(*s.Brightness)
	}
	if s.On != nil {
		a.player.SetOn(*s.On)
	}
	if s.Resume {
		a.player.Resume()
	}
	if s.Next {
		a.player.Next()
	}
}

// idleLED: green = display on, red = off, amber = microphone muted (voice satellite).
func (a *app) idleLED(st player.Status) {
	sat := a.sat.Load()
	if sat != nil && sat.Phase() != voice.Idle {
		return
	}
	switch {
	case sat != nil && sat.MicMuted():
		hw.SetLED(100, 40, 0)
	case st.On:
		hw.SetLED(0, 100, 0)
	default:
		hw.SetLED(100, 0, 0)
	}
}

func (a *app) startVoice(ctx context.Context, addr, sendspinAddr string, btProxy bool) *voice.Satellite {
	// In the image: /usr/bin/mww + /usr/share/mww; for development also /mnt/UDISK/voice/{mww,models}.
	mww, models := "/usr/bin/mww", "/usr/share/mww"
	if _, err := os.Stat(mww); err != nil {
		mww, models = "/mnt/UDISK/voice/mww", "/mnt/UDISK/voice/models"
	}
	if _, err := os.Stat(mww); err != nil {
		log.Printf("voice: no mww; satellite disabled")
		return nil
	}
	mac := strings.TrimSpace(readFileString("/sys/class/net/wlan0/address"))
	suffix := strings.ReplaceAll(mac, ":", "")
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	sat := voice.NewSatellite(models, mww)
	sat.Name, sat.FriendlyName, sat.MAC, sat.Version = "h8630-"+suffix, "Gaming Pixel Light", strings.ToUpper(mac), version
	sat.ConfigPath = a.path("voice.json")
	sat.OnPhase = func(p voice.Phase) {
		switch p {
		case voice.Listening:
			hw.SetLED(0, 30, 100)
		case voice.Thinking:
			hw.SetLED(60, 0, 100)
		case voice.Speaking:
			hw.SetLED(0, 100, 60)
		case voice.Error:
			hw.SetLED(100, 0, 0)
		case voice.TimerRinging:
			hw.SetLED(100, 50, 0)
		default:
			a.idleLED(a.player.Status())
		}
		a.player.Refresh() // the timer overlay steps aside while the satellite has the panel
	}
	sat.OnTimers = func() {
		a.mu.Lock()
		a.applyPlaylist() // timers get a turn in the rotation while any are running
		a.mu.Unlock()
		a.player.Refresh()
	}
	a.mu.Lock()
	a.voice = sat
	a.mu.Unlock()
	a.sat.Store(sat)
	a.player.SetOverlay(sat.DrawTimers)
	if sendspinAddr != "" {
		startSendspin(ctx, sat, a.path("sendspin.json"), sendspinAddr, strings.ToLower(mac))
	}
	go sat.Run(ctx)
	go voice.Show(ctx, sat, a.player.Stream, a.player.EndStream)
	if btProxy {
		go func() {
			p, err := bt.Start(ctx)
			if err != nil {
				log.Printf("bluetooth: %v", err)
				return
			}
			sat.SetBluetooth(p)
		}()
	}
	go func() {
		if err := (&esphomeapi.Server{Name: sat.Name, Handler: sat}).ListenAndServe(addr); err != nil {
			log.Printf("voice: %v", err)
		}
	}()
	go advertise(ctx, sat.Name, addr, strings.ToLower(mac))
	return sat
}

// startSendspin makes the speaker a Sendspin player too (Music Assistant's synchronised playback): the
// same speaker, volume and mute as the voice satellite's media player.
func startSendspin(ctx context.Context, sat *voice.Satellite, statePath, addr, mac string) {
	var ss *sendspin.Client
	ss, err := sendspin.New(sendspin.Config{
		StatePath: statePath, Name: sat.FriendlyName, Product: "H8630 Gaming Pixel Light",
		Manufacturer: "Govee (custom firmware)", Version: version, MAC: mac,
		Volume: func() (int, bool) {
			v, m := sat.Speaker()
			return int(math.Round(v * 100)), m
		},
		SetVolume:     func(v int) { sat.SetVolume(float64(v) / 100) },
		SetMuted:      sat.SetMuted,
		OnPairingCode: sat.ShowPairingCode,
		OnPlaying:     sat.SyncedMusic,
	})
	if err != nil {
		log.Printf("sendspin: %v", err)
		return
	}
	sat.SetTimed(ss.Player())
	sat.OnSpeaker = ss.StateChanged
	sat.OnMusic = ss.Leave
	go func() {
		if err := ss.Serve(ctx, addr); err != nil {
			log.Printf("sendspin: %v", err)
		}
	}()
	go advertiseSendspin(ctx, sat.FriendlyName, addr)
}

func readFileString(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// onStatus runs whenever what's shown, power or brightness changes.
func (a *app) onStatus(st player.Status) {
	if !a.cfg.sim {
		a.idleLED(st)
	}
	b, _ := json.Marshal(st)
	a.mu.Lock()
	a.lastState = b
	a.mu.Unlock()
	if a.client != nil && a.client.IsConnected() {
		a.client.Publish(a.topic("px/state"), 1, true, b)
	}
	sb, _ := json.Marshal(savedState{On: st.On, Brightness: st.Brightness})
	writeFileAtomic(a.path("state.json"), sb)
}

func (a *app) publishJSON(topic string, retain bool, v any) {
	if !a.client.IsConnected() {
		return
	}
	b, _ := json.Marshal(v)
	a.client.Publish(a.topic(topic), 1, retain, b)
}

func run(ctx context.Context, cfg config) error {
	if err := os.MkdirAll(filepath.Join(cfg.dataDir, "content"), 0o755); err != nil {
		return err
	}
	a := &app{cfg: cfg, frames: map[string][]content.Frame{}, missing: map[string]bool{}}
	if !cfg.sim {
		a.app = selfupdate.FromEnv()
	}

	var sink player.Sink
	if cfg.sim {
		p := newPreview(cfg.preview)
		sink = p
	} else {
		d, err := panel.OpenDisplay("/dev/fb0")
		if err != nil {
			return fmt.Errorf("panel: %w", err)
		}
		defer d.Close()
		sink = d
	}

	st := savedState{On: true, Brightness: 100}
	if b, err := os.ReadFile(a.path("state.json")); err == nil {
		json.Unmarshal(b, &st)
	}
	a.player = player.New(sink, st.On, st.Brightness, a.onStatus)
	if b, err := os.ReadFile(a.path("playlist.json")); err == nil && json.Unmarshal(b, &a.playlist) == nil {
		a.mu.Lock()
		a.applyPlaylist() // show the last playlist straight away, even before MQTT connects
		a.mu.Unlock()
	}
	go a.player.Run(ctx, func(err error) { log.Printf("draw: %v", err) })
	var sat *voice.Satellite
	if cfg.voice != "" && !cfg.sim {
		sat = a.startVoice(ctx, cfg.voice, cfg.sendspin, cfg.bluetooth)
	}
	if cfg.game != "" && !cfg.sim {
		go game.NewServer(a.player.Stream).ListenAndServe(cfg.game)
	}
	if cfg.ddp != "" {
		if err := ddp.Listen(cfg.ddp, a.player.Stream); err != nil {
			log.Printf("ddp: %v", err)
		}
	}

	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%s", cfg.host, cfg.port)).
		SetClientID("pixeld-" + filepath.Base(filepath.Clean(cfg.prefix))).
		SetUsername(cfg.user).SetPassword(cfg.pass).
		SetCleanSession(true).SetAutoReconnect(true).SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).SetMaxReconnectInterval(60 * time.Second).
		SetKeepAlive(30 * time.Second).
		SetWill(cfg.prefix+"px/status", "offline", 1, true)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Printf("mqtt connected")
		c.Publish(a.topic("px/status"), 1, true, "online")
		a.publishInfo()
		a.mu.Lock()
		if a.lastState != nil {
			c.Publish(a.topic("px/state"), 1, true, a.lastState)
		}
		a.mu.Unlock()
		c.SubscribeMultiple(map[string]byte{
			a.topic("px/set"): 1, a.topic("px/show"): 1, a.topic("px/playlist"): 1, a.topic("px/content/+"): 1,
			a.topic("px/update/+"): 1,
		}, func(_ mqtt.Client, m mqtt.Message) {
			switch t := m.Topic(); {
			case strings.HasPrefix(t, a.topic("px/update/")):
				go a.onUpdate(strings.TrimPrefix(t, a.topic("px/update/")), m)
			case t == a.topic("px/set"):
				a.onSet(nil, m)
			case t == a.topic("px/show"):
				a.onShow(nil, m)
			case t == a.topic("px/playlist"):
				a.onPlaylist(nil, m)
			default:
				a.onContent(nil, m)
			}
		})
		go a.requestMissing()
		a.updateStarted()
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) { log.Printf("mqtt connection lost: %v", err) })
	a.client = mqtt.NewClient(opts)
	a.client.Connect() // retries in the background (SetConnectRetry)

	buttons := make(chan hw.ButtonEvent, 8)
	jack := make(chan hw.Jack, 2)
	if !cfg.sim {
		hw.WatchButtons(buttons)
		hw.WatchJack(jack)
	}
	var lastJack *hw.Jack
	telemetry := time.NewTimer(10 * time.Second)
	need := time.NewTicker(30 * time.Second)
	defer need.Stop()
	for {
		select {
		case <-ctx.Done():
			if a.client.IsConnected() {
				a.client.Publish(a.topic("px/status"), 1, true, "offline").WaitTimeout(2 * time.Second)
				a.client.Disconnect(500)
			}
			return nil
		case b := <-buttons:
			a.publishJSON("button", false, b)
			switch {
			case b.Button == "power" && b.Action == "press":
				a.player.SetOn(!a.player.Status().On)
			case b.Button == "mode" && b.Action == "press":
				a.player.Next()
			case b.Button == "customize" && b.Action == "press" && sat != nil:
				sat.PushToTalk()
			case b.Button == "customize" && b.Action == "long_press" && sat != nil:
				sat.ToggleMic()
				a.onStatus(a.player.Status())
			}
		case j := <-jack:
			lastJack = &j
			a.publishJSON("jack", true, j)
		case <-need.C:
			a.requestMissing()
			if lastJack != nil {
				a.publishJSON("jack", true, *lastJack)
			}
		case <-telemetry.C:
			telemetry.Reset(60 * time.Second)
			if !cfg.sim {
				a.publishJSON("telemetry", true, hw.ReadTelemetry(version))
			}
		}
	}
}


// deviceInfo identifies the light for Home Assistant (published retained on px/info).
func deviceInfo(sim bool) map[string]any {
	info := map[string]any{"sku": "H8630", "size": panel.Size, "pixeld": version, "formats": content.Formats}
	if mac, err := os.ReadFile("/sys/class/net/wlan0/address"); err == nil {
		info["mac"] = strings.TrimSpace(string(mac))
	} else if sim {
		info["mac"] = "sim"
	}
	t := hw.ReadTelemetry(version)
	info["firmware"], info["base"] = t.Firmware, t.Base
	return info
}

// ---- updates (see selfupdate) ----

// updateMsg is published retained on px/update/state.
type updateMsg struct {
	State   string `json:"state"` // installing, restarting, trial, ok, rolled_back, failed
	Version string `json:"version"`
	Detail  string `json:"detail,omitempty"`
}

func (a *app) updateState(state, detail string) {
	if state == "failed" || state == "rolled_back" {
		log.Printf("update: %s %s", state, detail)
	} else {
		log.Printf("update: %s", state)
	}
	a.publishJSON("px/update/state", true, updateMsg{State: state, Version: version, Detail: detail})
}

// publishInfo announces the light (px/info, retained), incl. which build is running.
func (a *app) publishInfo() {
	info := deviceInfo(a.cfg.sim)
	info["updates"], info["slot"] = a.app != "", a.slot()
	a.publishJSON("px/info", true, info)
}

// slot is which build is running: trial, current or builtin.
func (a *app) slot() string {
	if s := a.app.Running(); s != "" {
		return s
	}
	return "builtin"
}

// updateStarted (on each MQTT connect) reports a trial the launcher rolled back, and lets a trial build
// confirm itself once it has stayed connected for a minute.
func (a *app) updateStarted() {
	if a.app.TakeRejected() {
		a.updateState("rolled_back", "the new build failed its trial; back on "+version)
	}
	if a.app.Running() != selfupdate.Trial {
		return
	}
	a.confirm.Do(func() {
		a.updateState("trial", "")
		go func() {
			time.Sleep(time.Minute)
			for !a.client.IsConnected() {
				time.Sleep(5 * time.Second)
			}
			if err := a.app.Confirm(); err != nil {
				a.updateState("failed", "confirm: "+err.Error())
				return
			}
			a.publishInfo() // now running as current
			a.updateState("ok", "")
		}()
	})
}

// installMsg (px/update/install): where to download a build (Home Assistant serves it) and its SHA-256.
type installMsg struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// onUpdate: px/update/install fetches and installs a build; px/update/rollback goes back to the previous
// one. Either way pixeld then restarts into it (the launcher runs it on trial). Builds are fetched over
// HTTP rather than sent over MQTT, as brokers commonly limit message size.
func (a *app) onUpdate(what string, m mqtt.Message) {
	if m.Retained() { // only act on fresh requests
		return
	}
	switch what {
	case "rollback":
		if err := a.app.Rollback(); err != nil {
			a.updateState("failed", err.Error())
			return
		}
	case "install":
		var req installMsg
		if err := json.Unmarshal(m.Payload(), &req); err != nil || req.URL == "" || !reKey.MatchString(req.SHA256) {
			a.updateState("failed", "bad install request")
			return
		}
		if sum, err := selfupdate.Sum(); err == nil && sum == req.SHA256 {
			a.updateState("ok", "already running this build")
			return
		}
		a.updateState("installing", "")
		bin, err := selfupdate.Download(req.URL)
		if err == nil {
			err = a.app.Install(bin, req.SHA256)
		}
		if err != nil {
			a.updateState("failed", err.Error())
			return
		}
	default: // e.g. our own px/update/state
		return
	}
	a.updateState("restarting", "")
	time.Sleep(time.Second) // let the state go out
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
