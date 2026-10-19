package voice

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// pcmOut plays 16-bit mono PCM through an ALSA hardware device by its kernel interface (no alsa-lib in the
// image, and aplay can't say how much is queued): Delay is exactly how long until the next sample written
// is heard, which synchronised playback (Sendspin) needs to the millisecond.
type pcmOut struct {
	f      *os.File
	period int // frames
	buffer int // frames
}

// Kernel ABI (include/uapi/sound/asound.h). unsigned long fields are uintptr, so the layouts match on
// both 32-bit ARM (the light) and 64-bit hosts.
type sndMask struct{ bits [8]uint32 }

type sndInterval struct {
	min, max uint32
	flags    uint32 // openmin:1 openmax:1 integer:1 empty:1
}

type sndHwParams struct {
	flags     uint32
	masks     [3]sndMask
	mres      [5]sndMask
	intervals [12]sndInterval
	ires      [9]sndInterval
	rmask     uint32
	cmask     uint32
	info      uint32
	msbits    uint32
	rateNum   uint32
	rateDen   uint32
	fifoSize  uintptr
	reserved  [64]byte
}

type sndXferi struct {
	result uintptr // snd_pcm_sframes_t
	buf    uintptr
	frames uintptr
}

const (
	paramAccess     = 0
	paramFormat     = 1
	paramSubformat  = 2
	paramSampleBits = 8 // first interval
	paramFrameBits  = 9
	paramChannels   = 10
	paramRate       = 11
	paramPeriodSize = 13
	paramPeriods    = 15
	paramBufferSize = 17

	accessRWInterleaved = 3
	formatS16LE         = 2
	intervalInteger     = 1 << 2
)

func ioc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'A'<<8 | nr }

var (
	iocHwParams = ioc(3, 0x11, unsafe.Sizeof(sndHwParams{}))
	iocPrepare  = ioc(0, 0x40, 0)
	iocDrop     = ioc(0, 0x43, 0)
	iocDelay    = ioc(2, 0x21, unsafe.Sizeof(uintptr(0)))
	iocWritei   = ioc(1, 0x50, unsafe.Sizeof(sndXferi{}))
)

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// openPCM opens card 0 device 0 for playback at rate Hz mono, with the given period (frames) and periods.
func openPCM(rate, period, periods int) (*pcmOut, error) {
	f, err := os.OpenFile("/dev/snd/pcmC0D0p", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var p sndHwParams
	for i := range p.masks {
		for j := range p.masks[i].bits {
			p.masks[i].bits[j] = ^uint32(0)
		}
	}
	for i := range p.intervals {
		p.intervals[i] = sndInterval{0, ^uint32(0), 0}
	}
	p.rmask = ^uint32(0)
	only := func(m int, bit uint) { p.masks[m] = sndMask{}; p.masks[m].bits[bit/32] = 1 << (bit % 32) }
	only(paramAccess, accessRWInterleaved)
	only(paramFormat, formatS16LE)
	only(paramSubformat, 0)
	set := func(i, v int) { p.intervals[i-paramSampleBits] = sndInterval{uint32(v), uint32(v), intervalInteger} }
	set(paramSampleBits, 16)
	set(paramFrameBits, 16)
	set(paramChannels, 1)
	set(paramRate, rate)
	set(paramPeriodSize, period)
	set(paramPeriods, periods)
	if err := ioctl(f.Fd(), iocHwParams, unsafe.Pointer(&p)); err != nil {
		f.Close()
		return nil, fmt.Errorf("hw params: %w", err)
	}
	o := &pcmOut{f: f, period: int(p.intervals[paramPeriodSize-paramSampleBits].min),
		buffer: int(p.intervals[paramBufferSize-paramSampleBits].min)}
	if err := ioctl(f.Fd(), iocPrepare, nil); err != nil {
		f.Close()
		return nil, fmt.Errorf("prepare: %w", err)
	}
	return o, nil
}

// Write queues samples, blocking while the buffer is full; an underrun is recovered from (and reported).
func (o *pcmOut) Write(s []int16) (underrun bool, err error) {
	for len(s) > 0 {
		x := sndXferi{buf: uintptr(unsafe.Pointer(&s[0])), frames: uintptr(len(s))}
		err := ioctl(o.f.Fd(), iocWritei, unsafe.Pointer(&x))
		switch err {
		case nil:
			s = s[int(x.result):]
		case syscall.EPIPE: // underrun: the stream stopped; start again
			underrun = true
			if err := ioctl(o.f.Fd(), iocPrepare, nil); err != nil {
				return underrun, err
			}
		case syscall.EINTR, syscall.EAGAIN:
		default:
			return underrun, err
		}
	}
	return underrun, nil
}

// Delay is how many frames are queued before the next one written reaches the output.
func (o *pcmOut) Delay() (int, error) {
	var d uintptr
	if err := ioctl(o.f.Fd(), iocDelay, unsafe.Pointer(&d)); err != nil {
		return 0, err
	}
	return int(int32(d)), nil // snd_pcm_sframes_t; only ever small
}

func (o *pcmOut) Close() {
	ioctl(o.f.Fd(), iocDrop, nil)
	o.f.Close()
}
