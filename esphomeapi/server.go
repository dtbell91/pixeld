// Package esphomeapi speaks the ESPHome native API (plaintext framing) so Home Assistant's ESPHome
// integration can talk to pixeld: voice assistant satellite now, Bluetooth proxy later.
//
// Message definitions are generated from aioesphomeapi's api.proto (MIT, v45.3.1, see proto/):
//
//	protoc -I proto -I <protoc>/include --go_out=. --go_opt=paths=source_relative \
//	  --go_opt=Mapi.proto=pixeld/esphomeapi --go_opt=Mapi_options.proto=pixeld/esphomeapi api.proto api_options.proto
//
// Frame: 0x00, varint payload length, varint message type, protobuf payload.
package esphomeapi

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

var (
	byID   = map[uint32]protoreflect.MessageType{}
	idOf   = map[protoreflect.FullName]uint32{}
	loaded sync.Once
)

// registry reads each message's (id) option from the generated descriptors.
func registry() {
	loaded.Do(func() {
		msgs := File_api_proto.Messages()
		for i := 0; i < msgs.Len(); i++ {
			md := msgs.Get(i)
			opts, ok := md.Options().(*descriptorpb.MessageOptions)
			if !ok || opts == nil || !proto.HasExtension(opts, E_Id) {
				continue
			}
			id := proto.GetExtension(opts, E_Id).(uint32)
			mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
			if err != nil {
				continue
			}
			byID[id] = mt
			idOf[md.FullName()] = id
		}
	})
}

// Handler receives every message other than the connection-level ones handled here.
type Handler interface {
	Connected(c *Conn)
	Message(c *Conn, m proto.Message)
	Disconnected(c *Conn)
}

type Conn struct {
	nc     net.Conn
	mu     sync.Mutex
	w      *bufio.Writer
	closed bool
}

// Send writes messages to Home Assistant (safe from any goroutine).
func (c *Conn) Send(msgs ...proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	for _, m := range msgs {
		id, ok := idOf[m.ProtoReflect().Descriptor().FullName()]
		if !ok {
			return fmt.Errorf("esphomeapi: no id for %s", m.ProtoReflect().Descriptor().FullName())
		}
		body, err := proto.Marshal(m)
		if err != nil {
			return err
		}
		var hdr [1 + 2*binary.MaxVarintLen32]byte
		n := 1
		n += binary.PutUvarint(hdr[n:], uint64(len(body)))
		n += binary.PutUvarint(hdr[n:], uint64(id))
		c.w.Write(hdr[:n])
		c.w.Write(body)
	}
	c.nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.w.Flush()
}

func (c *Conn) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.nc.Close()
}

// Server accepts Home Assistant connections.
type Server struct {
	Name    string // node name, e.g. "h8630-1a2b3c"
	Handler Handler
}

func (s *Server) ListenAndServe(addr string) error {
	registry()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("esphome api: listening on %s (%d message types)", addr, len(byID))
	for {
		nc, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serve(nc)
	}
}

func (s *Server) serve(nc net.Conn) {
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
	}
	c := &Conn{nc: nc, w: bufio.NewWriter(nc)}
	r := bufio.NewReader(nc)
	log.Printf("esphome api: connection from %s", nc.RemoteAddr())
	defer func() {
		c.Close()
		s.Handler.Disconnected(c)
		log.Printf("esphome api: %s disconnected", nc.RemoteAddr())
	}()
	connected := false
	for {
		nc.SetReadDeadline(time.Now().Add(3 * time.Minute)) // HA pings every ~20 s
		m, err := readFrame(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("esphome api: %v", err)
			}
			return
		}
		if m == nil {
			continue // unknown message type
		}
		switch req := m.(type) {
		case *HelloRequest:
			log.Printf("esphome api: hello from %q (api %d.%d)", req.ClientInfo, req.ApiVersionMajor, req.ApiVersionMinor)
			c.Send(&HelloResponse{ApiVersionMajor: 1, ApiVersionMinor: 10, ServerInfo: "pixeld", Name: s.Name})
		case *AuthenticationRequest:
			c.Send(&AuthenticationResponse{})
			if !connected {
				connected = true
				s.Handler.Connected(c)
			}
		case *PingRequest:
			c.Send(&PingResponse{})
		case *DisconnectRequest:
			c.Send(&DisconnectResponse{})
			return
		default:
			if !connected { // newer clients skip AuthenticationRequest entirely
				connected = true
				s.Handler.Connected(c)
			}
			s.Handler.Message(c, m)
		}
	}
}

func readFrame(r *bufio.Reader) (proto.Message, error) {
	pre, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if pre != 0 {
		return nil, fmt.Errorf("encrypted or bad frame (preamble %#x); only plaintext is supported", pre)
	}
	size, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	typ, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if size > 1<<20 {
		return nil, fmt.Errorf("frame too large (%d)", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	mt, ok := byID[uint32(typ)]
	if !ok {
		return nil, nil
	}
	m := mt.New().Interface()
	if err := proto.Unmarshal(body, m); err != nil {
		return nil, fmt.Errorf("type %d: %w", typ, err)
	}
	return m, nil
}
