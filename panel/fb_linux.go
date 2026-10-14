package panel

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	fbioGetVScreenInfo = 0x4600
	fbioPanDisplay     = 0x4606
	varYOffset         = 20 // byte offset of yoffset in struct fb_var_screeninfo
)

// Display drives /dev/fb0: frames are encoded into the hidden half, then panned to (as Govee does), so the
// panel never shows a half-written frame. Nothing else may draw to fb0 at the same time.
type Display struct {
	f     *os.File
	mem   []byte
	words []uint32
	vinfo [160]byte
	shown int // 0 or 1: which half is on the panel
}

func OpenDisplay(path string) (*Display, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	d := &Display{f: f}
	if err := d.ioctl(fbioGetVScreenInfo); err != nil {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %w", err)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, 2*HalfWords*4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap: %w", err)
	}
	d.mem = mem
	d.words = unsafe.Slice((*uint32)(unsafe.Pointer(&mem[0])), 2*HalfWords)
	if *(*uint32)(unsafe.Pointer(&d.vinfo[varYOffset])) >= FBLines {
		d.shown = 1
	}
	// Both halves start from the template, so later frames only need to rewrite LED slots.
	copy(d.words[:HalfWords], template[:])
	copy(d.words[HalfWords:], template[:])
	return d, nil
}

func (d *Display) ioctl(req uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), req, uintptr(unsafe.Pointer(&d.vinfo[0]))); e != 0 {
		return e
	}
	return nil
}

func (d *Display) half(n int) []uint32 { return d.words[n*HalfWords : (n+1)*HalfWords] }

// Show puts a frame on the panel.
func (d *Display) Show(f *Frame, brightness int) error {
	back := 1 - d.shown
	Encode(d.half(back), f, brightness)
	*(*uint32)(unsafe.Pointer(&d.vinfo[varYOffset])) = uint32(back * FBLines)
	if err := d.ioctl(fbioPanDisplay); err != nil {
		return fmt.Errorf("FBIOPAN_DISPLAY: %w", err)
	}
	d.shown = back
	return nil
}

// Shown returns the half currently on the panel (for checks).
func (d *Display) Shown() []uint32 { return d.half(d.shown) }

func (d *Display) Close() error {
	syscall.Munmap(d.mem)
	return d.f.Close()
}
