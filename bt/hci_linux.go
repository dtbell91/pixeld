// Package bt makes the light a Home Assistant Bluetooth proxy: it brings up the RTL8723DS's Bluetooth side,
// scans for BLE advertisements through a raw HCI socket (no BlueZ daemon), and relays them to Home
// Assistant over the ESPHome API, as an ESPHome Bluetooth proxy does (passive, raw advertisements).
package bt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	afBluetooth    = 31
	btprotoHCI     = 1
	hciChannelUser = 1
	hciDevDown     = 0x400448ca // _IOW('H', 202, int)

	pktCommand = 0x01
	pktEvent   = 0x04

	evtCommandComplete  = 0x0e
	evtCommandStatus    = 0x0f
	evtLEMeta           = 0x3e
	leAdvertisingReport = 0x02

	opReset           = 0x0c03
	opSetEventMask    = 0x0c01
	opLESetEventMask  = 0x2001
	opReadBDAddr      = 0x1009
	opLESetScanParams = 0x200b
	opLESetScanEnable = 0x200c
)

// powerUp attaches the controller if there's no hci0 yet: Bluetooth is powered off at boot (rfkill) and
// Govee's app, which used to attach it, doesn't run in native mode. The attach tool stays running (it
// carries the UART line discipline) until ctx ends.
func powerUp(ctx context.Context) error {
	if _, err := os.Stat("/sys/class/bluetooth/hci0"); err == nil {
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		setRfkill(false)
		time.Sleep(500 * time.Millisecond)
		setRfkill(true)
		time.Sleep(500 * time.Millisecond)
		cmd := exec.CommandContext(ctx, "rtk_hciattach", "-n", "-s", "115200", "/dev/ttyS1", "rtk_h5")
		if err := cmd.Start(); err != nil {
			return err
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		for i := 0; i < 60; i++ {
			if _, err := os.Stat("/sys/class/bluetooth/hci0"); err == nil {
				return nil
			}
			select {
			case <-done:
				i = 60
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		cmd.Process.Kill()
		<-done
	}
	return errors.New("the Bluetooth controller didn't come up")
}

func setRfkill(on bool) {
	paths, _ := filepath.Glob("/sys/class/rfkill/rfkill*")
	for _, p := range paths {
		if t, _ := os.ReadFile(filepath.Join(p, "type")); strings.TrimSpace(string(t)) == "bluetooth" {
			v := "0"
			if on {
				v = "1"
			}
			os.WriteFile(filepath.Join(p, "state"), []byte(v), 0)
		}
	}
}

// hci is an exclusive (user channel) HCI socket on hci0: the kernel stays out of it.
type hci struct {
	f    *os.File
	cmds chan []byte // Command Complete / Status events
	adv  func(report)
}

type report struct {
	evtType, addrType byte
	addr              uint64 // as ESPHome has it: the printed MAC, most significant byte first
	data              []byte
	rssi              int8
}

func openHCI(adv func(report)) (*hci, error) {
	fd, err := syscall.Socket(afBluetooth, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, btprotoHCI)
	if err != nil {
		return nil, fmt.Errorf("hci socket: %w", err)
	}
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), hciDevDown, 0) // the user channel needs it down
	sa := [3]uint16{afBluetooth, 0, hciChannelUser}
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("hci bind: %w", e)
	}
	syscall.SetNonblock(fd, true)
	h := &hci{f: os.NewFile(uintptr(fd), "hci0"), cmds: make(chan []byte, 4), adv: adv}
	go h.read()
	return h, nil
}

func (h *hci) close() { h.f.Close() }

func (h *hci) read() {
	buf := make([]byte, 1024)
	for {
		n, err := h.f.Read(buf)
		if err != nil {
			close(h.cmds)
			return
		}
		if n < 3 || buf[0] != pktEvent {
			continue
		}
		code, p := buf[1], buf[3:n]
		switch code {
		case evtCommandComplete, evtCommandStatus:
			select {
			case h.cmds <- append([]byte{code}, p...):
			default:
			}
		case evtLEMeta:
			if len(p) > 1 && p[0] == leAdvertisingReport {
				h.reports(p[1:])
			}
		}
	}
}

// reports parses an LE Advertising Report: num_reports, then each field as an array across the reports.
func (h *hci) reports(p []byte) {
	if len(p) < 1 {
		return
	}
	n := int(p[0])
	p = p[1:]
	if len(p) < n*10 {
		return
	}
	evt, at, addrs, lens := p[:n], p[n:2*n], p[2*n:8*n], p[8*n:9*n]
	rest := p[9*n:]
	total := 0
	for i := 0; i < n; i++ {
		total += int(lens[i])
	}
	if len(rest) < total+n {
		return
	}
	data, rssi := rest[:total], rest[total:total+n]
	off := 0
	for i := 0; i < n; i++ {
		a := addrs[6*i : 6*i+6]
		var addr uint64
		for j := 5; j >= 0; j-- {
			addr = addr<<8 | uint64(a[j])
		}
		l := int(lens[i])
		h.adv(report{evtType: evt[i], addrType: at[i], addr: addr,
			data: append([]byte{}, data[off:off+l]...), rssi: int8(rssi[i])})
		off += l
	}
}

// command sends an HCI command and returns its Command Complete return parameters (after the status).
func (h *hci) command(op uint16, params ...byte) ([]byte, error) {
	pkt := []byte{pktCommand, byte(op), byte(op >> 8), byte(len(params))}
	if _, err := h.f.Write(append(pkt, params...)); err != nil {
		return nil, err
	}
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-h.cmds:
			if !ok {
				return nil, errors.New("hci closed")
			}
			switch {
			case ev[0] == evtCommandComplete && len(ev) >= 4 && binary.LittleEndian.Uint16(ev[2:]) == op:
				if len(ev) > 4 && ev[4] != 0 {
					return nil, fmt.Errorf("hci command %04x: status %#x", op, ev[4])
				}
				if len(ev) > 4 {
					return ev[5:], nil
				}
				return nil, nil
			case ev[0] == evtCommandStatus && len(ev) >= 5 && binary.LittleEndian.Uint16(ev[3:]) == op:
				if ev[1] != 0 {
					return nil, fmt.Errorf("hci command %04x: status %#x", op, ev[1])
				}
				return nil, nil
			}
		case <-timeout:
			return nil, fmt.Errorf("hci command %04x: no response", op)
		}
	}
}

// setup resets the controller, enables LE advertising reports and returns its address.
func (h *hci) setup() (string, error) {
	steps := []struct {
		op uint16
		p  []byte
	}{
		{opReset, nil},
		{opSetEventMask, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0x1f, 0x00, 0x20}}, // defaults + LE Meta (bit 61)
		{opLESetEventMask, []byte{0x1f, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, s := range steps {
		if _, err := h.command(s.op, s.p...); err != nil {
			return "", err
		}
	}
	r, err := h.command(opReadBDAddr)
	if err != nil || len(r) < 6 {
		return "", fmt.Errorf("read address: %v", err)
	}
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", r[5], r[4], r[3], r[2], r[1], r[0]), nil
}

// scan starts or stops scanning: active sends scan requests (names etc. in scan responses), passive only
// listens. interval and window are in 0.625 ms units.
func (h *hci) scan(on, active bool, interval, window uint16) error {
	if !on {
		_, err := h.command(opLESetScanEnable, 0, 0)
		return err
	}
	h.command(opLESetScanEnable, 0, 0) // parameters can only change while stopped
	typ := byte(0)
	if active {
		typ = 1
	}
	if _, err := h.command(opLESetScanParams, typ, byte(interval), byte(interval>>8), byte(window), byte(window>>8), 0, 0); err != nil {
		return err
	}
	_, err := h.command(opLESetScanEnable, 1, 0) // duplicates not filtered: Home Assistant wants every one
	return err
}

func logf(format string, a ...any) { log.Printf("bluetooth: "+format, a...) }
