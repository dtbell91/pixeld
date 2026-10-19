package sendspin

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/flynn/noise"
	"github.com/gorilla/websocket"
)

const (
	handshakeTimeout = 30 * time.Second
	idleTimeout      = 60 * time.Second // time sync answers arrive every few seconds
	maxNoisePlain    = 65535 - 16
	msgJSON          = 0
	msgFragment      = 1
	msgAudio         = 4
	maxReassembled   = 8 << 20
	suiteChaCha      = "25519_ChaChaPoly_SHA256"
)

var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// errSilent closes a connection without sending anything (the spec's silent failures).
var errSilent = errors.New("protocol error")

type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func encode(typ string, payload any) []byte {
	if payload == nil {
		payload = struct{}{}
	}
	b, _ := json.Marshal(struct {
		Type    string `json:"type"`
		Payload any    `json:"payload"`
	}{typ, payload})
	return b
}

// conn is one server's encrypted connection: the Noise KKpsk2 handshake (the server initiates, the light
// responds), then Noise transport messages, each one WebSocket binary message.
type conn struct {
	c        *Client
	ws       *websocket.Conn
	serverID string
	psk      pskKind // which PSK keyed the session
	mismatch bool    // keyed by the Sentinel standing in for a credential the light couldn't use
	h        []byte  // handshake hash

	wmu       sync.Mutex
	gate      *sync.Cond // application sends wait while a re-handshake is under way
	rehand    bool
	send      *noise.CipherState
	recv      *noise.CipherState // only touched by the reader
	reasm     []byte
	reasmType int
	inFrag    bool
	closed    bool
}

func newConn(c *Client, ws *websocket.Conn) *conn {
	k := &conn{c: c, ws: ws}
	k.gate = sync.NewCond(&k.wmu)
	return k
}

func (k *conn) readText() ([]byte, error) {
	k.ws.SetReadDeadline(time.Now().Add(handshakeTimeout))
	t, b, err := k.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	if t != websocket.TextMessage {
		return nil, errSilent
	}
	return b, nil
}

// handshake runs client/init, server/init and the two Noise messages.
func (k *conn) handshake() error {
	st := k.c.store
	initMsg := encode("client/init", map[string]any{"client_id": st.clientID(), "version": 1, "suite": suiteChaCha})
	k.ws.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	if err := k.ws.WriteMessage(websocket.TextMessage, initMsg); err != nil {
		return err
	}
	srvInit, err := k.readText()
	if err != nil {
		return err
	}
	var env envelope
	if json.Unmarshal(srvInit, &env) != nil {
		return errSilent
	}
	if env.Type == "server/error" {
		return fmt.Errorf("server refused: %s", env.Payload)
	}
	var si struct {
		ServerID string `json:"server_id"`
		Version  *int   `json:"version"`
	}
	if env.Type != "server/init" || json.Unmarshal(env.Payload, &si) != nil || si.Version == nil || *si.Version != 1 {
		return errSilent
	}
	spub, err := b64u.DecodeString(si.ServerID)
	if err != nil || len(spub) != 32 {
		return errSilent
	}
	k.serverID = si.ServerID

	hs, err := k.newHandshake(append(initMsg, srvInit...), spub)
	if err != nil {
		return err
	}
	msg1, err := k.readText()
	if err != nil {
		return err
	}
	return k.respond(hs, msg1, true, func(b []byte) error {
		k.ws.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		return k.ws.WriteMessage(websocket.TextMessage, b)
	})
}

func (k *conn) newHandshake(prologue, serverPub []byte) (*noise.HandshakeState, error) {
	st := k.c.store.get()
	return noise.NewHandshakeState(noise.Config{
		CipherSuite:           cipherSuite,
		Pattern:               noise.HandshakeKK,
		Initiator:             false,
		Prologue:              prologue,
		StaticKeypair:         noise.DHKey{Private: st.PrivateKey, Public: k.c.store.pub},
		PeerStatic:            serverPub,
		PresharedKeyPlacement: 2,
	})
}

// respond processes Noise message 1 (a noise/handshake JSON message), picks the PSK it names, and sends
// message 2 through write. initial: the first handshake (a PSK miss falls back to the Sentinel).
func (k *conn) respond(hs *noise.HandshakeState, msg1 []byte, initial bool, write func([]byte) error) error {
	var env envelope
	var p struct {
		Data string `json:"data"`
	}
	if json.Unmarshal(msg1, &env) != nil || env.Type != "noise/handshake" || json.Unmarshal(env.Payload, &p) != nil {
		return errSilent
	}
	ct, err := b64u.DecodeString(p.Data)
	if err != nil {
		return errSilent
	}
	pt, _, _, err := hs.ReadMessage(nil, ct)
	if err != nil {
		return errSilent
	}
	var m1 struct {
		PSKID       string `json:"psk_id"`
		PSKCategory string `json:"psk_category"`
	}
	if json.Unmarshal(pt, &m1) != nil {
		return errSilent
	}
	kind := pskKind(m1.PSKCategory)
	if kind != pskLongTerm && kind != pskPairing && kind != pskSentinel {
		return errSilent
	}
	psk, bound := k.c.store.resolve(m1.PSKID, kind)
	mismatch := false
	if psk == nil {
		if !initial {
			return errSilent
		}
		psk, kind, mismatch = sentinelPSK, pskSentinel, true
	} else if kind == pskLongTerm && bound != k.serverID {
		return errSilent // a record bound to another server: a misbinding, not a miss
	}
	if err := hs.SetPresharedKey(psk); err != nil {
		return err
	}
	out, cs1, cs2, err := hs.WriteMessage(nil, []byte("{}"))
	if err != nil {
		return err
	}
	if err := write(encode("noise/handshake", map[string]string{"data": b64u.EncodeToString(out)})); err != nil {
		return err
	}
	// cs1 carries initiator -> responder (what the light receives), cs2 the other way.
	k.recv = cs1
	k.wmu.Lock()
	k.send = cs2
	k.wmu.Unlock()
	k.psk, k.mismatch, k.h = kind, mismatch, hs.ChannelBinding()
	if kind == pskLongTerm {
		k.c.store.touch(k.serverID)
	}
	return nil
}

// sendRaw encrypts and sends one plaintext (wmu held).
func (k *conn) sendRaw(pt []byte) error {
	if k.closed {
		return errors.New("closed")
	}
	if len(pt) > maxNoisePlain+1 {
		return errors.New("message too large")
	}
	ct, err := k.send.Encrypt(nil, nil, pt)
	if err != nil {
		return err
	}
	k.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return k.ws.WriteMessage(websocket.BinaryMessage, ct)
}

// sendJSON sends an application message, waiting out a re-handshake.
func (k *conn) sendJSON(typ string, payload any) error {
	k.wmu.Lock()
	defer k.wmu.Unlock()
	for k.rehand && !k.closed {
		k.gate.Wait()
	}
	return k.sendRaw(append([]byte{msgJSON}, encode(typ, payload)...))
}

func (k *conn) close() {
	k.wmu.Lock()
	k.closed = true
	k.gate.Broadcast()
	k.wmu.Unlock()
	k.ws.Close()
}

// goodbye sends client/goodbye and closes.
func (k *conn) goodbye(reason string) {
	k.wmu.Lock()
	if !k.closed {
		k.sendRaw(append([]byte{msgJSON}, encode("client/goodbye", map[string]string{"reason": reason})...))
	}
	k.wmu.Unlock()
	k.close()
}

// next returns the next decrypted message (reassembled from fragments): its type byte, body and when its
// last byte arrived.
func (k *conn) next() (typ int, body []byte, at time.Time, err error) {
	for {
		k.ws.SetReadDeadline(time.Now().Add(idleTimeout))
		t, ct, err := k.ws.ReadMessage()
		at = time.Now()
		if err != nil {
			return 0, nil, at, err
		}
		if t != websocket.BinaryMessage {
			return 0, nil, at, errSilent // cleartext after the switch to transport mode
		}
		pt, err := k.recv.Decrypt(nil, nil, ct)
		if err != nil || len(pt) == 0 {
			return 0, nil, at, errSilent
		}
		if pt[0] != msgFragment {
			if k.inFrag {
				return 0, nil, at, errSilent
			}
			return int(pt[0]), pt[1:], at, nil
		}
		if len(pt) < 2 || pt[1]&0xfc != 0 {
			return 0, nil, at, errSilent
		}
		first, last := pt[1]&2 != 0, pt[1]&1 != 0
		data := pt[2:]
		switch {
		case first && k.inFrag, !first && !k.inFrag:
			return 0, nil, at, errSilent
		case first:
			if len(data) < 1 || data[0] == msgFragment {
				return 0, nil, at, errSilent
			}
			k.inFrag, k.reasmType, k.reasm = true, int(data[0]), append([]byte{}, data[1:]...)
		default:
			k.reasm = append(k.reasm, data...)
		}
		if len(k.reasm) > maxReassembled {
			return 0, nil, at, errSilent
		}
		if last {
			k.inFrag = false
			b := k.reasm
			k.reasm = nil
			return k.reasmType, b, at, nil
		}
	}
}

// rehandshake answers an in-band Noise message 1 (already decrypted, as JSON): message 2 goes out under
// the current keys, then the new keys take over. Application sends wait until the server/activate that
// must follow.
func (k *conn) rehandshake(msg1 []byte) error {
	spub, _ := b64u.DecodeString(k.serverID)
	hs, err := k.newHandshake(k.h, spub)
	if err != nil {
		return err
	}
	k.wmu.Lock()
	k.rehand = true
	k.wmu.Unlock()
	return k.respond(hs, msg1, false, func(b []byte) error { // before respond switches the keys
		k.wmu.Lock()
		defer k.wmu.Unlock()
		return k.sendRaw(append([]byte{msgJSON}, b...))
	})
}

// endRehandshake lets application sends through again (the server/activate after a re-handshake).
func (k *conn) endRehandshake() {
	k.wmu.Lock()
	k.rehand = false
	k.gate.Broadcast()
	k.wmu.Unlock()
}

func be64(b []byte) int64  { return int64(binary.BigEndian.Uint64(b)) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func (k *conn) logf(format string, a ...any) {
	id := k.serverID
	if len(id) > 8 {
		id = id[:8]
	}
	log.Printf("sendspin [%s]: "+format, append([]any{id}, a...)...)
}
