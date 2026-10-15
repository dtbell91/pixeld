package selfupdate

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func armELF() []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5], b[16], b[18] = 1, 1, 2, 0x28
	return append(b, []byte("pixeld")...)
}

func pack(b []byte) ([]byte, string) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	s := sha256.Sum256(b)
	return buf.Bytes(), hex.EncodeToString(s[:])
}

func TestInstallChecks(t *testing.T) {
	d := Dir(t.TempDir())
	gz, sum := pack(armELF())
	if err := d.Install(gz, sum[:63]+"0"); err == nil {
		t.Fatal("accepted a wrong checksum")
	}
	x86 := armELF()
	x86[18] = 0x3e
	gzx, sumx := pack(x86)
	if err := d.Install(gzx, sumx); err == nil {
		t.Fatal("accepted a non-ARM binary")
	}
	if err := d.Install([]byte("nope"), sum); err == nil {
		t.Fatal("accepted non-gzip")
	}
	os.WriteFile(filepath.Join(string(d), Tries), []byte("2"), 0o644)
	if err := d.Install(gz, sum); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(string(d), Trial, Binary))
	if err != nil || st.Mode()&0o100 == 0 {
		t.Fatalf("trial not installed executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(string(d), Tries)); err == nil {
		t.Fatal("old trial count kept")
	}
	if err := Dir("").Install(gz, sum); err != ErrUnsupported {
		t.Fatalf("no app dir: %v", err)
	}
}

func TestRollbackAndRejected(t *testing.T) {
	d := Dir(t.TempDir())
	if err := d.Rollback(); err == nil {
		t.Fatal("rolled back with no previous")
	}
	os.MkdirAll(filepath.Join(string(d), Previous), 0o755)
	os.WriteFile(filepath.Join(string(d), Previous, Binary), armELF(), 0o755)
	os.MkdirAll(filepath.Join(string(d), Trial), 0o755) // an old trial is replaced
	if err := d.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(string(d), Trial, Binary)); err != nil {
		t.Fatal("previous not made the trial")
	}
	if d.TakeRejected() {
		t.Fatal("rejected with none")
	}
	os.MkdirAll(filepath.Join(string(d), Rejected), 0o755)
	if !d.TakeRejected() || d.TakeRejected() {
		t.Fatal("rejected not reported exactly once")
	}
}

func TestConfirm(t *testing.T) {
	d := Dir(t.TempDir())
	for _, slot := range []string{Trial, Current} {
		os.MkdirAll(filepath.Join(string(d), slot), 0o755)
		os.WriteFile(filepath.Join(string(d), slot, Binary), []byte(slot), 0o755)
	}
	os.WriteFile(filepath.Join(string(d), Tries), []byte("1"), 0o644)
	executable = func() (string, error) { return filepath.Join(string(d), Trial, Binary), nil }
	defer func() { executable = os.Executable }()
	if d.Running() != Trial {
		t.Fatal("not running as trial")
	}
	if err := d.Confirm(); err != nil {
		t.Fatal(err)
	}
	for slot, want := range map[string]string{Current: Trial, Previous: Current} {
		if b, _ := os.ReadFile(filepath.Join(string(d), slot, Binary)); string(b) != want {
			t.Errorf("%s holds %q, want %q", slot, b, want)
		}
	}
	for _, gone := range []string{Trial, Tries} {
		if _, err := os.Stat(filepath.Join(string(d), gone)); err == nil {
			t.Errorf("%s left behind", gone)
		}
	}
}
