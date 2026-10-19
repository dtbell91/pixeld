// pixeld: owns the H8630's panel, buttons and status LED, and talks to Home Assistant over MQTT.
//
// MQTT (prefix P, default "govee/h8630/"):
//
//	P px/status          <- "online" / "offline" (retained, last will)
//	P px/state           <- {"on","brightness","showing","item","playlist_items","paused"} (retained)
//	P px/set             -> {"on":bool, "brightness":1-100, "resume":true, "next":true}  (any subset)
//	P px/show            -> {"key":"<sha256>", "hold":seconds (0 = until resume), "name":"..."}
//	P px/playlist        -> {"items":[{"name":"..","key":"<sha256>","duration":seconds}]} (retained)
//	P px/content/<sha256> -> image bytes (PNG/JPEG/GIF), sent when the light asks for them on px/need
//	P px/need            <- {"keys":["<sha256>",...]} content the light is missing
//	P px/info            <- {"mac","sku","size","firmware","base","pixeld"} (retained)
//	P telemetry, button, jack  <- same messages as the firmware's telemetry layer
//
// Live frames: DDP on UDP 4048 (LedFx, xLights, Hyperion...), 32x32 RGB row-major; shown over everything
// while they arrive, then the panel goes back to normal (state "showing": "stream").
//
// Voice satellite: ESPHome API on :6053 (Home Assistant's ESPHome integration), wake word on the light
// (mww + /usr/share/mww models), advertised over mDNS as _esphomelib._tcp.
//
// Pac-Man: http://<light>:8080/ is a phone controller; games stream over the panel like DDP.
//
// Content is cached on the light by hash, so a playlist edit only transfers new items, and rotation runs
// here: no downloads, no loading screens.
//
// Run on a PC with -sim: no panel or hardware, a live preview at http://localhost:8099/.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"pixeld/sendspin"
)

var version = "dev"

type config struct {
	host, port, user, pass, prefix string
	dataDir                        string
	sim                            bool
	preview                        string
	ddp                            string
	game                           string
	voice                          string
	sendspin                       string
}

func readEnv(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return m
}

func main() {
	envPath := flag.String("env", "/mnt/UDISK/mqtt/bridge.env", "MQTT settings: MQTT_HOST, MQTT_PORT, MQTT_USER, MQTT_PASS[, MQTT_PREFIX]")
	data := flag.String("data", "/mnt/UDISK/pixeld", "state and content cache directory")
	sim := flag.Bool("sim", false, "simulator: no panel/hardware, browser preview instead")
	preview := flag.String("preview", "127.0.0.1:8099", "preview address in -sim mode")
	prefix := flag.String("prefix", "", "topic prefix (overrides MQTT_PREFIX)")
	ddpAddr := flag.String("ddp", ":4048", "DDP (UDP) listen address for live frames; empty to disable")
	gameAddr := flag.String("game", ":8080", "Pac-Man controller page (HTTP); empty to disable")
	voiceAddr := flag.String("voice", ":6053", "voice satellite (ESPHome API); empty to disable")
	sendspinAddr := flag.String("sendspin", ":8928", "Sendspin player (Music Assistant), with the voice satellite; empty to disable")
	showVersion := flag.Bool("version", false, "print the version and exit")
	showToken := flag.Bool("sendspin-token", false, "print the Sendspin pairing token (for pairing by token) and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *showToken {
		t, err := sendspin.Token(filepath.Join(*data, "sendspin.json"))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(t)
		return
	}

	env := readEnv(*envPath)
	for _, k := range []string{"MQTT_HOST", "MQTT_PORT", "MQTT_USER", "MQTT_PASS", "MQTT_PREFIX"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	cfg := config{host: env["MQTT_HOST"], port: env["MQTT_PORT"], user: env["MQTT_USER"], pass: env["MQTT_PASS"],
		prefix: env["MQTT_PREFIX"], dataDir: *data, sim: *sim, preview: *preview, ddp: *ddpAddr, game: *gameAddr, voice: *voiceAddr,
		sendspin: *sendspinAddr}
	if *prefix != "" {
		cfg.prefix = *prefix
	}
	if cfg.prefix == "" {
		cfg.prefix = "govee/h8630/"
	}
	if !strings.HasSuffix(cfg.prefix, "/") {
		cfg.prefix += "/"
	}
	if cfg.port == "" {
		cfg.port = "1883"
	}
	if cfg.host == "" {
		log.Fatalf("no MQTT_HOST (in %s or the environment)", *envPath)
	}
	log.SetFlags(0)
	if !*sim {
		// syslog only reaches the serial console on this firmware; keep a small log in RAM instead.
		log.SetOutput(io.MultiWriter(os.Stdout, &ramLog{path: "/tmp/pixeld.log", max: 256 << 10}))
		log.SetFlags(log.Ltime)
	}
	log.Printf("pixeld %s: broker %s:%s, prefix %s, data %s", version, cfg.host, cfg.port, cfg.prefix, cfg.dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

// ramLog appends to a file in tmpfs, rotating once (to .1) when it reaches max bytes.
type ramLog struct {
	path string
	max  int64
}

func (r *ramLog) Write(p []byte) (int, error) {
	if st, err := os.Stat(r.path); err == nil && st.Size() > r.max {
		os.Rename(r.path, r.path+".1")
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return len(p), nil
	}
	defer f.Close()
	return f.Write(p)
}
