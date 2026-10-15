package panel

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The oracle (Govee's own renderer, built by the firmware workspace's renderer/oracle/build.sh) and the
// extracted base rootfs it runs in under qemu-arm. Defaults: this repo as the firmware workspace's
// pixeld submodule; override with PIXELD_ORACLE / PIXELD_SYSROOT.
var (
	oracleBin = envOr("PIXELD_ORACLE", "../../firmware-build/renderer/oracle/oracle")
	sysroot   = envOr("PIXELD_SYSROOT", "../../extracted_1.00.91/rootfs_ext")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestScale(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, 0}, {1, 6}, {50, 129}, {73, 187}, {100, 255}} {
		if got := Scale(c.in); got != c.want {
			t.Errorf("Scale(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestMatchesGovee renders frames with Govee's own libgovee_display (under qemu-arm) and requires our
// encoder to produce the identical framebuffer.
func TestMatchesGovee(t *testing.T) {
	if _, err := exec.LookPath("qemu-arm"); err != nil {
		t.Skip("qemu-arm not installed")
	}
	for _, p := range []string{oracleBin, sysroot} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not available", p)
		}
	}
	rnd := rand.New(rand.NewSource(8630))
	frames := []Frame{{}, {}}
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			frames[1][y][x] = [3]uint8{255, 255, 255}
		}
	}
	for n := 0; n < 6; n++ {
		var f Frame
		for y := 0; y < Size; y++ {
			for x := 0; x < Size; x++ {
				f[y][x] = [3]uint8{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256))}
			}
		}
		frames = append(frames, f)
	}
	dir := t.TempDir()
	in := make([]byte, 0, len(frames)*Size*Size*3)
	for i := range frames {
		for y := 0; y < Size; y++ {
			for x := 0; x < Size; x++ {
				in = append(in, frames[i][y][x][:]...)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "in.rgb"), in, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, lumi := range []int{100, 73, 50, 1} {
		out := filepath.Join(dir, fmt.Sprint(lumi))
		os.Mkdir(out, 0o755)
		cmd := exec.Command("qemu-arm", "-L", sysroot, oracleBin, filepath.Join(dir, "in.rgb"), fmt.Sprint(lumi), out)
		cmd.Env = append(os.Environ(), "FBSHIM_FILE="+filepath.Join(dir, fmt.Sprintf("fb%d", lumi)))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("oracle: %v\n%s", err, b)
		}
		for i := range frames {
			raw, err := os.ReadFile(filepath.Join(out, fmt.Sprintf("f%03d.fb", i)))
			if err != nil {
				t.Fatal(err)
			}
			got := Template()
			Encode(got, &frames[i], lumi)
			diff := 0
			for j := range got {
				if got[j] != binary.LittleEndian.Uint32(raw[j*4:]) {
					diff++
				}
			}
			if diff != 0 {
				t.Errorf("brightness %d frame %d: %d pixels differ from Govee", lumi, i, diff)
			}
		}
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	var f Frame
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			f[y][x] = [3]uint8{uint8(x * 8), uint8(y * 8), uint8((x + y) * 4)}
		}
	}
	half := Template()
	Encode(half, &f, 100)
	got := Decode(half)
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			for c := 0; c < 3; c++ {
				// the tables flatten near black (several low values share a word), so allow a little error there
				if d := int(got[y][x][c]) - int(f[y][x][c]); d < -3 || d > 3 {
					t.Fatalf("(%d,%d) ch %d: got %d want %d", x, y, c, got[y][x][c], f[y][x][c])
				}
			}
		}
	}
}
