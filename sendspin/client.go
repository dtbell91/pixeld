// Package sendspin makes the light a Sendspin player (Music Assistant's synchronised multi-room protocol):
// servers find it over mDNS and connect to it; the connection is end-to-end encrypted (Noise), paired with
// a code shown on the panel, and the audio it carries plays in sync with the server's other speakers.
package sendspin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	Path            = "/sendspin"
	roleNamePlayer  = "player@v1"
	bufferCapacity  = 480000 // bytes of audio chunks outstanding (~5 s of 48 kHz mono)
	leadTimeMs      = 200    // startup lead the light asks for
	minBufferMs     = 150    // ongoing buffer it asks for (WiFi jitter)
	provisionalWait = 30 * time.Second
)

// epoch is the light's clock for Sendspin: µs since start, monotonic.
var epoch = time.Now()

func localNow() int64 { return time.Since(epoch).Microseconds() }

// Config wires the client to the rest of pixeld.
type Config struct {
	StatePath                                 string // identity, pairings, settings
	Name, Product, Manufacturer, Version, MAC string
	Volume                                    func() (volume int, muted bool) // 0-100
	SetVolume                                 func(int)
	SetMuted                                  func(bool)
	OnPairingCode                             func(code string) // show a pairing code ("" when done)
	OnPlaying                                 func(bool)        // a stream started / ended
}

type Client struct {
	cfg    Config
	store  *store
	player *player

	mu       sync.Mutex
	sessions map[*session]bool
	admitted *session
}

func New(cfg Config) (*Client, error) {
	st, err := openStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg, store: st, sessions: map[*session]bool{}}
	c.player = newPlayer(cfg.OnPlaying)
	c.player.setOutputDelay(st.get().OutputDelayMs)
	return c, nil
}

// Player is the synchronised audio, for the speaker to mix (voice.TimedSource).
func (c *Client) Player() *player { return c.player }

// ID is the light's client_id (its public key).
func (c *Client) ID() string { return c.store.clientID() }

// Serve accepts server connections on addr until ctx ends.
func (c *Client) Serve(ctx context.Context, addr string) error {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc(Path, func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.serve(ws)
	})
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		for s := range c.sessions {
			go s.goodbye("shutdown")
		}
		c.mu.Unlock()
		srv.Close()
	}()
	log.Printf("sendspin: listening on %s%s (client_id %s)", addr, Path, c.store.clientID())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// StateChanged reports a local volume or mute change to the server.
func (c *Client) StateChanged() {
	if s := c.current(); s != nil {
		go s.sendState()
	}
}

// Leave takes the light out of its group while something else (Home Assistant's own media player) plays.
func (c *Client) Leave() {
	s := c.current()
	if s == nil {
		return
	}
	s.mu.Lock()
	playing := s.groupPlaying
	s.mu.Unlock()
	if playing {
		go s.sendJSON("client/leave", nil)
	}
	c.player.stop()
}

func (c *Client) current() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.admitted
}

// session is one server's connection and what it has declared.
type session struct {
	*conn
	filter *timeFilter // under fmu
	fmu    sync.Mutex

	mu           sync.Mutex
	activated    bool
	activities   []string
	roles        []string
	pairingIndex int
	pairCh       chan envelope // the running pairing attempt's messages (nil: none)
	pairStop     chan struct{}
	stateSent    bool
	groupPlaying bool
	syncedSent   bool
}

func (c *Client) serve(ws *websocket.Conn) {
	k := newConn(c, ws)
	if err := k.handshake(); err != nil {
		log.Printf("sendspin: handshake with %s: %v", ws.RemoteAddr(), err)
		ws.Close()
		return
	}
	s := &session{conn: k, filter: newTimeFilter()}
	k.logf("connected from %s (%s PSK)", ws.RemoteAddr(), k.psk)
	c.mu.Lock()
	c.sessions[s] = true
	c.mu.Unlock()
	timer := time.AfterFunc(provisionalWait, func() {
		s.mu.Lock()
		a := s.activated
		s.mu.Unlock()
		if !a {
			s.close()
		}
	})
	err := s.run()
	timer.Stop()
	s.close()
	s.stopPairing()
	c.mu.Lock()
	delete(c.sessions, s)
	wasAdmitted := c.admitted == s
	if wasAdmitted {
		c.admitted = nil
	}
	c.mu.Unlock()
	if wasAdmitted {
		c.player.detach(s)
	}
	k.logf("disconnected: %v", err)
}

func (s *session) run() error {
	// server/hello comes first; client/hello answers it.
	typ, body, _, err := s.next()
	if err != nil {
		return err
	}
	var env envelope
	if typ != msgJSON || json.Unmarshal(body, &env) != nil || env.Type != "server/hello" {
		return errSilent
	}
	var hello struct {
		Name string `json:"name"`
	}
	json.Unmarshal(env.Payload, &hello)
	s.logf("server %q", hello.Name)
	if err := s.sendHello(); err != nil {
		return err
	}
	for {
		typ, body, at, err := s.next()
		if err != nil {
			return err
		}
		switch typ {
		case msgJSON:
			var env envelope
			if json.Unmarshal(body, &env) != nil || env.Payload == nil {
				return errSilent
			}
			if err := s.handle(env, at); err != nil {
				return err
			}
		case msgAudio:
			if len(body) >= 12 && s.playerActive() {
				s.c.player.chunk(s, be64(body[0:8]), body[12:])
			}
		}
	}
}

func (s *session) sendHello() error {
	cfg := s.c.cfg
	st := s.c.store.get()
	dev := map[string]any{"product_name": cfg.Product, "manufacturer": cfg.Manufacturer, "software_version": cfg.Version}
	if cfg.MAC != "" {
		dev["mac_address"] = cfg.MAC
	}
	return s.sendRawJSON("client/hello", map[string]any{
		"name":            cfg.Name,
		"device_info":     dev,
		"supported_roles": []string{roleNamePlayer},
		roleNamePlayer + "_support": map[string]any{
			"supported_formats": []map[string]any{{"codec": "pcm", "channels": 1, "sample_rate": 48000, "bit_depth": 16}},
			"buffer_capacity":   bufferCapacity,
		},
		"supported_pair_methods": map[string]any{
			"pairing_psk":          map[string]any{},
			"dynamic_pairing_code": map[string]any{"out_channels": []string{"display"}, "formats": []string{"digits"}},
		},
		"unpaired_access": map[string]bool{"enabled": st.UnpairedAccess},
	})
}

// sendRawJSON sends without waiting for the end of a re-handshake (handshake-phase messages).
func (s *session) sendRawJSON(typ string, payload any) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.sendRaw(append([]byte{msgJSON}, encode(typ, payload)...))
}

func (s *session) handle(env envelope, at time.Time) error {
	s.mu.Lock()
	activated := s.activated
	s.mu.Unlock()
	if env.Type == "server/activate" {
		return s.activate(env.Payload)
	}
	if env.Type == "noise/handshake" { // re-handshake: new keys, then a server/activate
		s.stopPairing()
		if err := s.rehandshake(mustJSON(env)); err != nil {
			return err
		}
		s.mu.Lock()
		s.pairingIndex = 0
		s.mu.Unlock()
		return nil
	}
	if !activated {
		return nil // nothing else is valid before the first activation
	}
	switch env.Type {
	case "server/time":
		var t struct {
			ClientTransmitted int64 `json:"client_transmitted"`
			ServerReceived    int64 `json:"server_received"`
			ServerTransmitted int64 `json:"server_transmitted"`
		}
		if json.Unmarshal(env.Payload, &t) != nil {
			return errSilent
		}
		recv := at.Sub(epoch).Microseconds()
		offset := ((t.ServerReceived - t.ClientTransmitted) + (t.ServerTransmitted - recv)) / 2
		delay := ((recv - t.ClientTransmitted) - (t.ServerTransmitted - t.ServerReceived)) / 2
		s.fmu.Lock()
		was := s.filter.synced()
		s.filter.update(offset, delay, recv)
		now := s.filter.synced()
		s.fmu.Unlock()
		if !was && now && s.playerActive() {
			go s.sendState()
		}
	case "server/command":
		var cmd struct {
			Player *struct {
				Command       string `json:"command"`
				Volume        *int   `json:"volume"`
				Mute          *bool  `json:"mute"`
				OutputDelayMs *int   `json:"output_delay_ms"`
			} `json:"player"`
		}
		if json.Unmarshal(env.Payload, &cmd) != nil || cmd.Player == nil || !s.playerActive() {
			return nil
		}
		switch p := cmd.Player; p.Command {
		case "volume":
			if p.Volume != nil {
				s.c.cfg.SetVolume(max(0, min(100, *p.Volume)))
			}
		case "mute":
			if p.Mute != nil {
				s.c.cfg.SetMuted(*p.Mute)
			}
		case "set_output_delay":
			if p.OutputDelayMs != nil {
				d := max(0, min(5000, *p.OutputDelayMs))
				s.c.store.update(func(st *state) { st.OutputDelayMs = d })
				s.c.player.setOutputDelay(d)
				go s.sendState()
			}
		}
	case "stream/start":
		var p struct {
			Player *streamFormat `json:"player"`
		}
		json.Unmarshal(env.Payload, &p)
		if p.Player != nil && s.playerActive() {
			s.c.player.start(s, *p.Player)
		}
	case "stream/clear", "stream/end":
		var p struct {
			Roles []string `json:"roles"`
		}
		json.Unmarshal(env.Payload, &p)
		if (p.Roles == nil || slices.Contains(p.Roles, "player")) && s.playerActive() {
			if env.Type == "stream/clear" {
				s.c.player.clear(s)
			} else {
				s.c.player.end(s)
			}
		}
	case "group/update":
		var g struct {
			PlaybackState string `json:"playback_state"`
			GroupName     string `json:"group_name"`
		}
		json.Unmarshal(env.Payload, &g)
		s.mu.Lock()
		s.groupPlaying = g.PlaybackState == "playing"
		s.mu.Unlock()
		s.logf("group %q: %s", g.GroupName, g.PlaybackState)
	case "server/unpair":
		if s.psk == pskLongTerm {
			s.c.store.removeRecord(s.serverID)
			s.logf("unpaired by the server")
			s.goodbye("unpaired")
		}
	case "server/pair-init", "server/pair-auth", "server/pair-confirm", "server/pair-finalize", "pair/abort":
		s.mu.Lock()
		ch := s.pairCh
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- env:
			default:
				return errSilent
			}
		}
	}
	return nil
}

func mustJSON(env envelope) []byte {
	b, _ := json.Marshal(env)
	return b
}

func (s *session) playerActive() bool {
	s.c.mu.Lock()
	admitted := s.c.admitted == s
	s.c.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return admitted && slices.Contains(s.roles, roleNamePlayer)
}

// allowed reports whether a set of activities may be declared on a session keyed as this one is.
func (s *session) allowed(acts []string, unpaired bool) bool {
	playback, pairing := slices.Contains(acts, "playback"), slices.Contains(acts, "pairing")
	for _, a := range acts {
		if a != "playback" && a != "pairing" {
			return false
		}
	}
	if s.psk == pskLongTerm {
		return !pairing
	}
	return !playback || unpaired
}

func (s *session) activate(raw json.RawMessage) error {
	var a struct {
		Activities  []string  `json:"activities"`
		ActiveRoles *[]string `json:"active_roles"`
		Pairing     *struct {
			Method string `json:"method"`
			Format string `json:"format"`
		} `json:"pairing"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return errSilent
	}
	s.endRehandshake()
	unpaired := s.c.store.get().UnpairedAccess
	s.mu.Lock()
	roles := s.roles
	if a.ActiveRoles != nil {
		roles = *a.ActiveRoles
	} else if !s.activated {
		roles = nil
	}
	first := !s.activated
	s.mu.Unlock()

	capable := func(u bool) bool { return s.allowed(append(slices.Clone(a.Activities), "playback"), u) }
	admissible := func(u bool) bool {
		return s.allowed(a.Activities, u) && (len(roles) == 0 || capable(u) || a.ActiveRoles == nil)
	}
	if !admissible(unpaired) {
		if s.psk != pskLongTerm && !unpaired && admissible(true) {
			s.logf("refusing: pairing required")
			s.goodbye("pairing_required")
		} else {
			s.goodbye("unauthorized")
		}
		return errors.New("activation refused")
	}
	if !capable(unpaired) {
		roles = nil // persisted roles lapse when the connection can no longer play
	}
	pairing := slices.Contains(a.Activities, "pairing")
	if pairing {
		ok := a.Pairing != nil
		if ok {
			switch a.Pairing.Method {
			case "pairing_psk":
				ok = s.psk == pskPairing
			case "dynamic_pairing_code":
				ok = s.psk != pskPairing && a.Pairing.Format == "digits"
			default:
				ok = false
			}
		}
		if !ok {
			s.sendJSON("pair/abort", map[string]string{"reason": "method_not_supported"})
			return nil
		}
	}

	s.mu.Lock()
	s.activated = true
	s.activities = a.Activities
	hadPlayer := slices.Contains(s.roles, roleNamePlayer)
	s.roles = roles
	hasPlayer := slices.Contains(roles, roleNamePlayer)
	if pairing {
		s.pairingIndex++
	}
	index := s.pairingIndex
	s.mu.Unlock()
	s.logf("activities %v, roles %v", a.Activities, roles)

	if first {
		if !s.c.admit(s) {
			return errors.New("not admitted")
		}
		go s.timeSync()
	}
	if hadPlayer && !hasPlayer {
		s.c.player.detach(s)
	}
	s.stopPairing()
	if pairing {
		s.startPairing(a.Pairing.Method, index)
	}
	if slices.Contains(a.Activities, "playback") && s.c.current() == s {
		if s.c.store.get().LastPlayback != s.serverID {
			s.c.store.update(func(st *state) { st.LastPlayback = s.serverID })
		}
	}
	s.mu.Lock()
	due := (hasPlayer && !hadPlayer) || (len(roles) > 0 && !s.stateSent)
	s.mu.Unlock()
	if due {
		go s.sendState()
	}
	return nil
}

func rank(acts []string) int {
	switch {
	case slices.Contains(acts, "playback"):
		return 2
	case slices.Contains(acts, "pairing"):
		return 1
	}
	return 0
}

// admit decides between a newly activated session and the one the light holds (one at a time; the
// spec's multiple-server rules).
func (c *Client) admit(s *session) bool {
	c.mu.Lock()
	cur := c.admitted
	last := c.store.get().LastPlayback
	ok := false
	switch {
	case cur == nil:
		ok = true
	case cur.pairingRunning():
	case rank(s.acts()) == 0 && rank(cur.acts()) == 0:
		ok = s.serverID == last && cur.serverID != last
	default:
		ok = rank(s.acts()) >= rank(cur.acts())
	}
	if ok {
		c.admitted = s
	}
	c.mu.Unlock()
	if !ok {
		s.logf("refused: another server holds the light")
		if slices.Contains(s.acts(), "pairing") {
			s.sendJSON("pair/abort", map[string]string{"reason": "concurrent_attempt"})
			s.close()
		} else {
			s.goodbye("concurrent_attempt")
		}
		return false
	}
	if cur != nil {
		cur.logf("displaced by another server")
		c.player.detach(cur)
		if cur.pairingRunning() {
			cur.sendJSON("pair/abort", map[string]string{"reason": "concurrent_attempt"})
			cur.close()
		} else {
			go cur.goodbye("another_server")
		}
	}
	return true
}

func (s *session) acts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activities
}

// timeSync keeps the clock mapping fresh: every 200 ms until synchronised, then less often the better it is.
func (s *session) timeSync() {
	for {
		if err := s.sendJSON("client/time", map[string]int64{"client_transmitted": localNow()}); err != nil {
			return
		}
		s.fmu.Lock()
		synced, e := s.filter.synced(), s.filter.errorUs()
		s.fmu.Unlock()
		d := 200 * time.Millisecond
		switch {
		case !synced:
		case e < 1000:
			d = 3 * time.Second
		case e < 2000:
			d = time.Second
		case e < 5000:
			d = 500 * time.Millisecond
		}
		time.Sleep(d)
	}
}

// serverTime converts a local time (µs) with the session's clock mapping.
func (s *session) serverTime(local int64) (int64, bool) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return s.filter.serverTime(local), s.filter.synced()
}

// sendState reports availability and (with the player role) the player's state.
func (s *session) sendState() {
	active := s.playerActive()
	s.fmu.Lock()
	synced := s.filter.synced()
	s.fmu.Unlock()
	p := map[string]any{"available": !active || synced}
	if active {
		vol, muted := s.c.cfg.Volume()
		p["player"] = map[string]any{
			"volume":                vol,
			"muted":                 muted,
			"output_delay_ms":       s.c.store.get().OutputDelayMs,
			"required_lead_time_ms": leadTimeMs,
			"min_buffer_ms":         minBufferMs,
			"supported_commands":    []string{"volume", "mute", "set_output_delay"},
		}
	}
	if s.sendJSON("client/state", p) == nil {
		s.mu.Lock()
		s.stateSent = true
		s.mu.Unlock()
	}
}
