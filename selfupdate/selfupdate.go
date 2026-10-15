// Package selfupdate installs new pixeld builds into the app directory that the firmware's launcher
// (/etc/init.d/pixeld) runs them from. A new build is installed as the trial; the launcher runs it next,
// and it confirms itself (becoming current) once it is healthy. The launcher rejects a trial that keeps
// failing to start or doesn't confirm in time, and goes back to current (or the firmware's built-in
// pixeld).
//
//	<dir>/trial/pixeld     new build, not yet confirmed
//	<dir>/trial.tries      launcher's count of trial starts
//	<dir>/current/pixeld   confirmed build the launcher runs
//	<dir>/previous/pixeld  the build current replaced (for rollback)
//	<dir>/rejected/pixeld  a trial the launcher gave up on
//
// Each build sits in a directory of its own so the process is still called pixeld.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	Trial    = "trial"
	Current  = "current"
	Previous = "previous"
	Rejected = "rejected"
	Tries    = "trial.tries"
	Binary   = "pixeld"

	maxSize = 64 << 20 // unpacked binary
)

var executable = os.Executable // replaced in tests

// ErrUnsupported: the firmware's launcher doesn't run builds from an app directory.
var ErrUnsupported = errors.New("this firmware can't run updated pixeld builds (launcher too old)")

// Dir is the app directory ("" = updates unsupported).
type Dir string

// FromEnv is the app directory the launcher passes in PIXELD_APP.
func FromEnv() Dir { return Dir(os.Getenv("PIXELD_APP")) }

func (d Dir) path(name string) string { return filepath.Join(string(d), name) }

func (d Dir) bin(slot string) string { return filepath.Join(string(d), slot, Binary) }

// replace renames directory from to to, replacing any old to.
func (d Dir) replace(from, to string) error {
	if err := os.RemoveAll(d.path(to)); err != nil {
		return err
	}
	return os.Rename(d.path(from), d.path(to))
}

// Running is which build this process is: Trial, Current, or "" (the built-in one, or not via the launcher).
func (d Dir) Running() string {
	if d == "" {
		return ""
	}
	exe, err := executable() // follows renames: a confirmed trial reports current
	if err != nil {
		return ""
	}
	for _, slot := range []string{Trial, Current} {
		if exe == d.bin(slot) {
			return slot
		}
	}
	return ""
}

// Download fetches a build (e.g. from Home Assistant) for Install.
func Download(url string) ([]byte, error) {
	c := http.Client{Timeout: 5 * time.Minute}
	r, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	bin, err := io.ReadAll(io.LimitReader(r.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(bin) > maxSize {
		return nil, errors.New("too big")
	}
	return bin, nil
}

// Install checks an armv7 pixeld against sha256 (hex) and installs it as the trial. The caller then
// restarts so the launcher runs it.
func (d Dir) Install(bin []byte, sum string) error {
	if d == "" {
		return ErrUnsupported
	}
	if got := sha256.Sum256(bin); hex.EncodeToString(got[:]) != sum {
		return errors.New("checksum mismatch")
	}
	if err := checkELF(bin); err != nil {
		return err
	}
	tmp := Trial + ".new"
	os.RemoveAll(d.path(tmp))
	if err := os.MkdirAll(d.path(tmp), 0o755); err != nil {
		return err
	}
	if err := writeSync(d.bin(tmp), bin); err != nil {
		os.RemoveAll(d.path(tmp))
		return err
	}
	os.Remove(d.path(Tries))
	if err := d.replace(tmp, Trial); err != nil {
		return err
	}
	return syncDir(string(d))
}

// checkELF: a 32-bit little-endian ARM executable.
func checkELF(b []byte) error {
	if len(b) < 20 || string(b[:4]) != "\x7fELF" || b[4] != 1 || b[5] != 1 || b[16] != 2 || b[18] != 0x28 || b[19] != 0 {
		return errors.New("not an armv7 Linux executable")
	}
	return nil
}

// Confirm makes the running trial the current build (the one it replaced becomes previous).
func (d Dir) Confirm() error {
	if d.Running() != Trial {
		return nil
	}
	if _, err := os.Stat(d.path(Current)); err == nil {
		if err := d.replace(Current, Previous); err != nil {
			return err
		}
	}
	if err := os.Rename(d.path(Trial), d.path(Current)); err != nil {
		return err
	}
	os.Remove(d.path(Tries))
	return syncDir(string(d))
}

// Rollback installs the previous build as the trial (it becomes current again once it confirms).
func (d Dir) Rollback() error {
	if d == "" {
		return ErrUnsupported
	}
	if _, err := os.Stat(d.bin(Previous)); err != nil {
		return errors.New("no previous build to go back to")
	}
	os.Remove(d.path(Tries))
	if err := d.replace(Previous, Trial); err != nil {
		return err
	}
	return syncDir(string(d))
}

// TakeRejected reports (once) whether the launcher rejected a trial since the last call.
func (d Dir) TakeRejected() bool {
	if d == "" {
		return false
	}
	if _, err := os.Stat(d.path(Rejected)); err != nil {
		return false
	}
	return os.RemoveAll(d.path(Rejected)) == nil
}

// Sum is the SHA-256 (hex) of the running executable.
func Sum() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
