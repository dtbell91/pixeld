package sendspin

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const attemptTimeout = 2 * time.Minute

var (
	errPairAborted = errors.New("pairing aborted")
	errPairTimeout = errors.New("pairing attempt timed out")

	// dynamic-code rounds since the last verified one, across attempts and servers (reset by restarting
	// the light: the spec's deliberate operator action)
	pairRounds   int
	pairRoundsMu sync.Mutex
)

func (s *session) pairingRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pairCh != nil
}

func (s *session) startPairing(method string, index int) {
	ch, stop := make(chan envelope, 8), make(chan struct{})
	s.mu.Lock()
	s.pairCh, s.pairStop = ch, stop
	s.mu.Unlock()
	go func() {
		err := s.pair(method, index, ch, stop)
		s.mu.Lock()
		if s.pairCh == ch {
			s.pairCh, s.pairStop = nil, nil
		}
		s.mu.Unlock()
		if s.c.cfg.OnPairingCode != nil {
			s.c.cfg.OnPairingCode("")
		}
		switch {
		case err == nil:
			s.logf("paired")
		case errors.Is(err, errPairAborted):
			s.logf("pairing: %v", err)
		case errors.Is(err, errPairTimeout):
			s.sendJSON("pair/abort", map[string]string{"reason": "attempt_timeout"})
			s.logf("pairing: %v", err)
		case err == errCancelled:
		default:
			s.logf("pairing failed: %v", err)
			s.close() // a protocol error
		}
	}()
}

// stopPairing abandons the attempt in progress (a new activation, a re-handshake, the connection ending).
func (s *session) stopPairing() {
	s.mu.Lock()
	stop := s.pairStop
	s.pairCh, s.pairStop = nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

var errCancelled = errors.New("cancelled")

// pair runs the client side of one pairing attempt.
func (s *session) pair(method string, index int, in <-chan envelope, stop <-chan struct{}) error {
	deadline := time.NewTimer(attemptTimeout)
	defer deadline.Stop()
	recv := func(want string, v any) error {
		select {
		case env := <-in:
			if env.Type == "pair/abort" {
				var a struct {
					Reason string `json:"reason"`
				}
				json.Unmarshal(env.Payload, &a)
				return fmt.Errorf("%w by the server: %s", errPairAborted, a.Reason)
			}
			if env.Type != want {
				return fmt.Errorf("expected %s, got %s", want, env.Type)
			}
			if json.Unmarshal(env.Payload, v) != nil {
				return fmt.Errorf("malformed %s", want)
			}
			return nil
		case <-stop:
			return errCancelled
		case <-deadline.C:
			return errPairTimeout
		}
	}
	send := func(typ string, payload any) error { return s.sendJSON(typ, payload) }

	if method == "pairing_psk" {
		if err := send("client/pair-init", map[string]int{"pairing_index": index}); err != nil {
			return err
		}
		psk := randBytes(32)
		if err := send("client/pair-finalize", map[string]string{"long_term_psk": b64u.EncodeToString(psk)}); err != nil {
			return err
		}
		if err := recv("server/pair-finalize", &struct{}{}); err != nil {
			return err
		}
		return s.c.store.addRecord(s.serverID, psk, s.c.openServers())
	}

	// dynamic pairing code, digits shown on the panel
	pairRoundsMu.Lock()
	held := pairRounds >= pairRoundLimit
	pairRoundsMu.Unlock()
	if held {
		send("client/pair-pending", map[string]any{"pairing_index": index,
			"message": "Too many wrong pairing codes: restart the light to try again."})
		<-stop
		return errCancelled
	}
	nonceB := randBytes(32)
	commit := sha256.Sum256(append([]byte("sendspin-pair-commit-v1"), nonceB...))
	if err := send("client/pair-init", map[string]any{"pairing_index": index, "commit_B": b64u.EncodeToString(commit[:])}); err != nil {
		return err
	}
	var init struct {
		NonceA *string `json:"nonce_A"`
	}
	if err := recv("server/pair-init", &init); err != nil {
		return err
	}
	if init.NonceA == nil {
		return errors.New("first server/pair-init has no nonce_A")
	}
	nonceA, err := decodeField(*init.NonceA, 32)
	if err != nil {
		return err
	}
	code := pairingCode(s.h, nonceA, nonceB)
	s.logf("pairing: showing the code")
	var cp *cpace
	var sid []byte
	for round := 1; ; round++ {
		pairRoundsMu.Lock()
		pairRounds++
		pairRoundsMu.Unlock()
		if s.c.cfg.OnPairingCode != nil {
			s.c.cfg.OnPairingCode(code)
		}
		sid = pakeSID(s.h, index, round)
		if cp, err = newCPaceResponder([]byte(code), sid, []byte("client")); err != nil {
			return err
		}
		var auth struct {
			PakeMsg1 string `json:"pake_msg_1"`
		}
		if err := recv("server/pair-auth", &auth); err != nil {
			return err
		}
		ya, err := decodeField(auth.PakeMsg1, 32)
		if err != nil {
			return err
		}
		if err := send("client/pair-auth", map[string]string{"pake_msg_2": b64u.EncodeToString(cp.share)}); err != nil {
			return err
		}
		if err := cp.derive(ya, []byte("server")); err != nil {
			return err
		}
		var conf struct {
			ServerKC string `json:"server_kc"`
		}
		if err := recv("server/pair-confirm", &conf); err != nil {
			return err
		}
		kc, err := decodeField(conf.ServerKC, 64)
		if err != nil {
			return err
		}
		if cp.verify(kc) {
			break
		}
		pairRoundsMu.Lock()
		limit := pairRounds >= pairRoundLimit
		pairRoundsMu.Unlock()
		if limit {
			send("pair/abort", map[string]string{"reason": "pairing_code_mismatch"})
			return fmt.Errorf("%w: wrong code, round limit reached", errPairAborted)
		}
		s.logf("pairing: wrong code entered; trying again")
		if err := send("client/pair-retry", nil); err != nil {
			return err
		}
		var again struct {
			NonceA *string `json:"nonce_A"`
		}
		if err := recv("server/pair-init", &again); err != nil {
			return err
		}
		if again.NonceA != nil {
			return errors.New("server/pair-init carries nonce_A after the first round")
		}
	}
	pairRoundsMu.Lock()
	pairRounds = 0
	pairRoundsMu.Unlock()
	wrappedNonce, err := wrap("sendspin-pair-nonce-wrap-v1", sid, cp.isk, nonceB)
	if err != nil {
		return err
	}
	if err := send("client/pair-confirm", map[string]string{"client_kc": b64u.EncodeToString(cp.tag()),
		"wrapped_nonce_B": b64u.EncodeToString(wrappedNonce)}); err != nil {
		return err
	}
	psk := randBytes(32)
	wrappedPSK, err := wrap("sendspin-pair-psk-wrap-v1", sid, cp.isk, psk)
	if err != nil {
		return err
	}
	if err := send("client/pair-finalize", map[string]string{"wrapped_psk": b64u.EncodeToString(wrappedPSK)}); err != nil {
		return err
	}
	if err := recv("server/pair-finalize", &struct{}{}); err != nil {
		return err
	}
	return s.c.store.addRecord(s.serverID, psk, s.c.openServers())
}

// pairingCode is the 6-digit dynamic pairing code bound to the handshake and both nonces.
func pairingCode(h, nonceA, nonceB []byte) string {
	in := append(append(append([]byte("sendspin-pairing-code-derive-v1"), h...), nonceA...), nonceB...)
	d := sha256.Sum256(in)
	n := new(big.Int).Mod(new(big.Int).SetBytes(d[:]), big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n.Int64())
}

func pakeSID(h []byte, index, round int) []byte {
	sid := append([]byte("sendspin-pair-pake-v1"), h...)
	sid = binary.BigEndian.AppendUint32(sid, uint32(index))
	return binary.BigEndian.AppendUint32(sid, uint32(round))
}

// wrap seals a 32-byte value under a key derived from the CPace output (the connection's AEAD:
// ChaCha20-Poly1305, zero nonce; each key is used once).
func wrap(label string, sid, isk, v []byte) ([]byte, error) {
	k := sha256.Sum256(append(append([]byte(label), sid...), isk...))
	a, err := chacha20poly1305.New(k[:])
	if err != nil {
		return nil, err
	}
	return a.Seal(nil, make([]byte, 12), v, nil), nil
}

func decodeField(s string, n int) ([]byte, error) {
	b, err := b64u.DecodeString(s)
	if err != nil || len(b) != n {
		return nil, fmt.Errorf("malformed field (%d bytes expected)", n)
	}
	return b, nil
}

// openServers are the servers with open connections (their pairing records are not evicted).
func (c *Client) openServers() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[string]bool{}
	for s := range c.sessions {
		m[s.serverID] = true
	}
	return m
}
