// Package hw: the light's status LED, buttons, headphone jack and health readings. Everything degrades to a
// no-op where the hardware is absent (e.g. running the simulator on a PC).
package hw

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ---- status LED (RGB on PWM 5/2/3, inversed polarity: duty 50000 = off) ----

const pwmRoot = "/sys/class/pwm/pwmchip0"

var ledChannels = [3]int{5, 2, 3} // red, green, blue

// SetLED sets the status LED; each channel 0-100 (% of Govee's "on" level).
func SetLED(r, g, b int) {
	for i, ch := range ledChannels {
		dir := fmt.Sprintf("%s/pwm%d", pwmRoot, ch)
		if _, err := os.Stat(dir); err != nil {
			if os.WriteFile(pwmRoot+"/export", []byte(strconv.Itoa(ch)), 0) != nil {
				continue
			}
			os.WriteFile(dir+"/period", []byte("50000"), 0)
			os.WriteFile(dir+"/polarity", []byte("inversed"), 0)
			os.WriteFile(dir+"/enable", []byte("1"), 0)
		}
		level := []int{r, g, b}[i]
		if level < 0 {
			level = 0
		} else if level > 100 {
			level = 100
		}
		os.WriteFile(dir+"/duty_cycle", []byte(strconv.Itoa(50000-level*350)), 0)
	}
}

// ---- input: buttons (gpio-keys) and headphone jack ----

var buttonNames = map[uint16]string{101: "power", 102: "mode", 104: "volume", 108: "customize"}

const (
	evKey      = 1
	evSw       = 5
	swHeadOut  = 2 // SW_HEADPHONE_INSERT
	swMicIn    = 4 // SW_MICROPHONE_INSERT
	LongPress  = time.Second
	eventBytes = 16 // struct input_event on 32-bit ARM (timeval 8 + type 2 + code 2 + value 4)
)

type ButtonEvent struct {
	Button string `json:"button"`
	Action string `json:"action"` // press | long_press
}

type Jack struct {
	Headphones bool `json:"headphones"`
	Microphone bool `json:"microphone"`
}

func openInput(name string) *os.File {
	paths, _ := filepath.Glob("/sys/class/input/event*/device/name")
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil || strings.TrimSpace(string(b)) != name {
			continue
		}
		f, err := os.Open("/dev/input/" + filepath.Base(filepath.Dir(filepath.Dir(p))))
		if err == nil {
			return f
		}
	}
	return nil
}

// WatchButtons sends press / long_press events (long_press once, after LongPress held). Never grabs the
// device. Returns false if there are no buttons.
func WatchButtons(out chan<- ButtonEvent) bool {
	f := openInput("gpio-keys")
	if f == nil {
		return false
	}
	downs := map[uint16]*time.Timer{} // pending long-press timers
	go func() {
		defer f.Close()
		buf := make([]byte, eventBytes*16)
		for {
			n, err := f.Read(buf)
			if err != nil {
				return
			}
			for off := 0; off+eventBytes <= n; off += eventBytes {
				typ := binary.LittleEndian.Uint16(buf[off+8:])
				code := binary.LittleEndian.Uint16(buf[off+10:])
				val := int32(binary.LittleEndian.Uint32(buf[off+12:]))
				name, ok := buttonNames[code]
				if typ != evKey || !ok {
					continue
				}
				switch val {
				case 1:
					downs[code] = time.AfterFunc(LongPress, func() { out <- ButtonEvent{name, "long_press"} })
				case 0:
					if t := downs[code]; t != nil {
						if t.Stop() { // released before the long press fired
							out <- ButtonEvent{name, "press"}
						}
						delete(downs, code)
					}
				}
			}
		}
	}()
	return true
}

// WatchJack sends the jack state now and on every change. Returns false if there is no jack device.
func WatchJack(out chan<- Jack) bool {
	f := openInput("audiocodec Headphones")
	if f == nil {
		return false
	}
	read := func() {
		var sw [8]byte
		req := uintptr(uint32(2)<<30 | uint32(len(sw))<<16 | uint32('E')<<8 | 0x1b) // EVIOCGSW(len)
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&sw[0]))); e == 0 {
			bits := binary.LittleEndian.Uint64(sw[:])
			out <- Jack{Headphones: bits>>swHeadOut&1 == 1, Microphone: bits>>swMicIn&1 == 1}
		}
	}
	read()
	go func() {
		defer f.Close()
		buf := make([]byte, eventBytes*16)
		for {
			n, err := f.Read(buf)
			if err != nil {
				return
			}
			for off := 0; off+eventBytes <= n; off += eventBytes {
				if binary.LittleEndian.Uint16(buf[off+8:]) == evSw {
					read()
					break
				}
			}
		}
	}()
	return true
}

// ---- telemetry ----

type Telemetry struct {
	RSSI      *int     `json:"rssi"`
	TxBitrate *float64 `json:"tx_bitrate"`
	IP        string   `json:"ip"`
	Uptime    int64    `json:"uptime"`
	Load      float64  `json:"load"`
	MemTotal  int64    `json:"mem_total_kb"`
	MemAvail  int64    `json:"mem_available_kb"`
	UDiskFree int64    `json:"udisk_free_kb"`
	Firmware  string   `json:"firmware"`
	Base      string   `json:"base"`
	Profile   string   `json:"profile"`
	Slot      string   `json:"slot"`
	Pixeld    string   `json:"pixeld"`
}

var (
	reSignal  = regexp.MustCompile(`signal:\s*(-?\d+)`)
	reBitrate = regexp.MustCompile(`tx bitrate:\s*([\d.]+)`)
)

func readKV(path, sep string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if k, v, ok := strings.Cut(s.Text(), sep); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func kb(v string) int64 { n, _ := strconv.ParseInt(strings.TrimSuffix(v, " kB"), 10, 64); return n }

// ReadTelemetry gathers device health; fields that can't be read stay empty/zero.
func ReadTelemetry(version string) Telemetry {
	t := Telemetry{Pixeld: version}
	if out, err := exec.Command("iw", "dev", "wlan0", "link").Output(); err == nil {
		if m := reSignal.FindSubmatch(out); m != nil {
			v, _ := strconv.Atoi(string(m[1]))
			t.RSSI = &v
		}
		if m := reBitrate.FindSubmatch(out); m != nil {
			v, _ := strconv.ParseFloat(string(m[1]), 64)
			t.TxBitrate = &v
		}
	}
	if ifc, err := net.InterfaceByName("wlan0"); err == nil {
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					t.IP = ipn.IP.String()
				}
			}
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f, _ := strconv.ParseFloat(strings.Fields(string(b))[0], 64)
		t.Uptime = int64(f)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		t.Load, _ = strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	}
	mem := readKV("/proc/meminfo", ":")
	t.MemTotal, t.MemAvail = kb(mem["MemTotal"]), kb(mem["MemAvailable"])
	var st syscall.Statfs_t
	if syscall.Statfs("/mnt/UDISK", &st) == nil {
		t.UDiskFree = int64(st.Bavail) * int64(st.Bsize) / 1024
	}
	fw := readKV("/etc/custom-firmware", "=")
	t.Firmware, t.Base, t.Profile = fw["version"], fw["base"], fw["profile"]
	if out, err := exec.Command("fw_printenv", "-n", "root_partition").Output(); err == nil {
		t.Slot = strings.TrimPrefix(strings.TrimSpace(string(out)), "rootfs")
	}
	return t
}

