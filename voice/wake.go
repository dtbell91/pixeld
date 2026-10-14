package voice

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// WakeModel is a microWakeWord model manifest (<id>.json next to <id>.tflite).
type WakeModel struct {
	ID        string
	Phrase    string   `json:"wake_word"`
	Languages []string `json:"trained_languages"`
	Path      string
}

// LoadModels lists the manifests in dir.
func LoadModels(dir string) map[string]WakeModel {
	out := map[string]WakeModel{}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, p := range paths {
		var m WakeModel
		b, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(b, &m) != nil || m.Phrase == "" {
			continue
		}
		m.ID = strings.TrimSuffix(filepath.Base(p), ".json")
		m.Path = p
		out[m.ID] = m
	}
	return out
}

// Detector runs mww on the active models; Feed it microphone audio, receive detections on Wake.
type Detector struct {
	bin    string
	models map[string]WakeModel
	wake   func(WakeModel)

	mu     sync.Mutex
	active []string
	in     chan []byte
	gen    int
}

func NewDetector(bin string, models map[string]WakeModel, wake func(WakeModel)) *Detector {
	return &Detector{bin: bin, models: models, wake: wake}
}

// SetActive (re)starts detection for these model ids (unknown ids are ignored).
func (d *Detector) SetActive(ids []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var use []string
	for _, id := range ids {
		if _, ok := d.models[id]; ok {
			use = append(use, id)
		}
	}
	sort.Strings(use)
	d.active = use
	d.gen++
	if d.in != nil {
		close(d.in)
		d.in = nil
	}
	if len(use) == 0 {
		return
	}
	in := make(chan []byte, 200) // 2 s of buffering; dropped beyond that rather than stalling capture
	d.in = in
	go d.run(d.gen, use, in)
}

func (d *Detector) Active() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.active...)
}

// Feed passes 10 ms of mono PCM to the detector (never blocks).
func (d *Detector) Feed(pcm []byte) {
	d.mu.Lock()
	in := d.in
	d.mu.Unlock()
	if in == nil {
		return
	}
	select {
	case in <- pcm:
	default:
	}
}

func (d *Detector) run(gen int, ids []string, in chan []byte) {
	for {
		args := []string{}
		for _, id := range ids {
			args = append(args, d.models[id].Path)
		}
		cmd := exec.Command(d.bin, args...)
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("voice: mww: %v", err)
			time.Sleep(5 * time.Second)
		} else {
			log.Printf("voice: wake words %v", ids)
			go func() {
				s := bufio.NewScanner(stdout)
				for s.Scan() {
					f := strings.Fields(s.Text())
					if len(f) >= 2 && f[0] == "wake" {
						if m, ok := d.models[f[1]]; ok {
							d.wake(m)
						}
					}
				}
			}()
			for pcm := range in {
				if _, err := stdin.Write(pcm); err != nil {
					break
				}
			}
			stdin.Close()
			cmd.Process.Kill()
			cmd.Wait()
		}
		d.mu.Lock()
		current := d.gen == gen
		d.mu.Unlock()
		if !current {
			return // replaced by SetActive
		}
		log.Printf("voice: mww exited; restarting")
		time.Sleep(time.Second)
	}
}
