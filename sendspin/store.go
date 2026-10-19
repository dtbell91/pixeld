package sendspin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
)

const (
	maxRecords      = 5 // pairing records kept (the spec's minimum)
	pairRoundLimit  = 20
	pskIDLabel      = "sendspin-psk-id-v1"
	sentinelPSKSeed = "sendspin-sentinel-psk-v1"
)

var sentinelPSK = func() []byte { h := sha256.Sum256([]byte(sentinelPSKSeed)); return h[:] }()

var b64u = base64.RawURLEncoding

// state is what the light keeps across reboots: its identity, pairing PSK, the servers it is paired
// with, and its player settings.
type state struct {
	PrivateKey     []byte   `json:"private_key"` // X25519; client_id is its public key
	PairingPSK     []byte   `json:"pairing_psk"`
	Records        []record `json:"records,omitempty"`
	OutputDelayMs  int      `json:"output_delay_ms"`
	LastPlayback   string   `json:"last_playback_server,omitempty"` // server_id
	PairRounds     int      `json:"pair_rounds"`                    // dynamic-code rounds since the last verified one
	UnpairedAccess bool     `json:"unpaired_access"`                // admit servers without pairing (off: pair first)
}

type record struct {
	ServerID string    `json:"server_id"`
	PSK      []byte    `json:"psk"` // long-term
	Used     time.Time `json:"used"`
}

type store struct {
	mu   sync.Mutex
	path string
	st   state
	pub  []byte
}

// openStore loads (or on first use creates) the state at path.
func openStore(path string) (*store, error) {
	s := &store{path: path}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, err
		}
	}
	created := false
	if len(s.st.PrivateKey) != 32 {
		s.st.PrivateKey = randBytes(32)
		created = true
	}
	if len(s.st.PairingPSK) != 32 {
		s.st.PairingPSK = randBytes(32)
		created = true
	}
	pub, err := curve25519.X25519(s.st.PrivateKey, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	s.pub = pub
	if created {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func (s *store) saveLocked() error {
	b, _ := json.MarshalIndent(s.st, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *store) clientID() string { return b64u.EncodeToString(s.pub) }

func pskID(psk []byte) string {
	h := sha256.Sum256(append([]byte(pskIDLabel), psk...))
	return b64u.EncodeToString(h[:])
}

type pskKind string

const (
	pskLongTerm pskKind = "lt"
	pskPairing  pskKind = "pr"
	pskSentinel pskKind = "sn"
)

// resolve finds the PSK a server's Noise message 1 refers to (nil: not one the light holds under that
// category), and for a long-term PSK the server it is bound to.
func (s *store) resolve(id string, kind pskKind) (psk []byte, serverID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case pskLongTerm:
		for _, r := range s.st.Records {
			if pskID(r.PSK) == id {
				return r.PSK, r.ServerID
			}
		}
	case pskPairing:
		if pskID(s.st.PairingPSK) == id {
			return s.st.PairingPSK, ""
		}
	case pskSentinel:
		if pskID(sentinelPSK) == id {
			return sentinelPSK, ""
		}
	}
	return nil, ""
}

// addRecord persists a new pairing, replacing any for that server and evicting the least recently used
// record not in protect (servers with open connections) when full.
func (s *store) addRecord(serverID string, psk []byte, protect map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.st.Records[:0]
	for _, r := range s.st.Records {
		if r.ServerID != serverID {
			recs = append(recs, r)
		}
	}
	for len(recs) >= maxRecords {
		lru := -1
		for i, r := range recs {
			if !protect[r.ServerID] && (lru < 0 || r.Used.Before(recs[lru].Used)) {
				lru = i
			}
		}
		if lru < 0 {
			break
		}
		recs = append(recs[:lru], recs[lru+1:]...)
	}
	s.st.Records = append(recs, record{ServerID: serverID, PSK: psk, Used: time.Now()})
	return s.saveLocked()
}

// removeRecord drops the pairing with serverID (server/unpair).
func (s *store) removeRecord(serverID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.st.Records[:0]
	for _, r := range s.st.Records {
		if r.ServerID != serverID {
			recs = append(recs, r)
		}
	}
	s.st.Records = recs
	s.saveLocked()
}

func (s *store) hasRecord(serverID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.st.Records {
		if r.ServerID == serverID {
			return true
		}
	}
	return false
}

func (s *store) touch(serverID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Records {
		if s.st.Records[i].ServerID == serverID {
			s.st.Records[i].Used = time.Now()
		}
	}
	s.saveLocked()
}

func (s *store) get() state {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *store) update(fn func(*state)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	s.saveLocked()
}

// PairingToken is the light's version-0 pairing token (client key + pairing PSK) for the Pairing PSK
// method: what an operator pastes into the server.
func (s *store) pairingToken() string {
	return encodeToken('0', append(append([]byte{}, s.pub...), s.st.PairingPSK...))
}

func encodeToken(version byte, payload []byte) string {
	body := strings.TrimRight(base32.StdEncoding.EncodeToString(payload), "=")
	return "SP:" + string(version) + strings.ReplaceAll(body, "2", "9")
}

// Token reads the pairing token from the state file at path (creating the identity if there is none yet).
func Token(path string) (string, error) {
	s, err := openStore(path)
	if err != nil {
		return "", err
	}
	return s.pairingToken(), nil
}
