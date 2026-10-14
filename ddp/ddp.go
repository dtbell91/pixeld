// Package ddp receives frames over DDP (Distributed Display Protocol, UDP port 4048), as sent by LedFx,
// xLights, Hyperion and other WLED-style tools. Pixels are 8-bit RGB, row-major from the top-left
// (index = y*32 + x), 1024 pixels per frame.
//
// Header (10 bytes; 14 with the time flag): flags (version 1 = 0x40, push 0x01, query 0x02, time 0x10),
// sequence, data type, destination id, data offset (BE u32, bytes), data length (BE u16).
package ddp

import (
	"encoding/binary"
	"log"
	"net"

	"pixeld/panel"
)

const (
	Port        = 4048
	flagVer1    = 0x40
	flagVerMask = 0xc0
	flagPush    = 0x01
	flagQuery   = 0x02
	flagTime    = 0x10
	frameBytes  = panel.Size * panel.Size * 3
)

// Listen receives DDP on addr (e.g. ":4048") and calls show for every complete frame. Runs until the
// socket fails.
func Listen(addr string, show func(*panel.Frame)) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	log.Printf("ddp: listening on %s", pc.LocalAddr())
	go func() {
		defer pc.Close()
		buf := make([]byte, 65536) // a whole frame may come in one (fragmented) datagram
		var frame [frameBytes]byte
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				log.Printf("ddp: %v", err)
				return
			}
			if n < 10 || buf[0]&flagVerMask != flagVer1 || buf[0]&flagQuery != 0 {
				continue
			}
			hdr := 10
			if buf[0]&flagTime != 0 {
				hdr = 14
			}
			off := int(binary.BigEndian.Uint32(buf[4:8]))
			length := int(binary.BigEndian.Uint16(buf[8:10]))
			if n < hdr+length || off >= frameBytes {
				continue
			}
			end := min(off+length, frameBytes)
			copy(frame[off:end], buf[hdr:hdr+end-off])
			// A frame is complete on the push flag, or (senders that never push) when its last byte arrives.
			if buf[0]&flagPush != 0 || end == frameBytes {
				f := new(panel.Frame)
				for i := 0; i < panel.Size*panel.Size; i++ {
					f[i/panel.Size][i%panel.Size] = [3]uint8{frame[i*3], frame[i*3+1], frame[i*3+2]}
				}
				show(f)
			}
		}
	}()
	return nil
}
