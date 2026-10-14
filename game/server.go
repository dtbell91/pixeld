package game

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"pixeld/panel"
)

//go:embed page.html
var page []byte

// Server runs Pac-Man games on demand and serves the controller page.
type Server struct {
	show func(*panel.Frame) // e.g. player.Stream
	mu   sync.Mutex
	game *Pacman
	last *Pacman
}

func NewServer(show func(*panel.Frame)) *Server { return &Server{show: show} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("POST /api/start", func(w http.ResponseWriter, _ *http.Request) {
		s.Start()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/input", func(w http.ResponseWriter, r *http.Request) {
		d := map[string]Dir{"up": Up, "down": Down, "left": Left, "right": Right}[r.URL.Query().Get("d")]
		s.mu.Lock()
		if s.game != nil {
			s.game.Input(d)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		st := map[string]any{"running": s.game != nil}
		if g := s.game; g != nil {
			st["score"], st["lives"], st["level"] = g.score, g.lives, g.level
		} else if s.last != nil {
			st["score"] = s.last.score
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	return mux
}

// Start begins a game unless one is running.
func (s *Server) Start() {
	s.mu.Lock()
	if s.game != nil {
		s.mu.Unlock()
		return
	}
	g := NewPacman()
	s.game = g
	s.mu.Unlock()
	log.Printf("game: pac-man started")
	go s.run(g)
}

func (s *Server) run(g *Pacman) {
	frame := time.NewTicker(33 * time.Millisecond) // keep the stream alive and blink smoothly
	defer frame.Stop()
	next := time.Now()
	for {
		<-frame.C
		s.mu.Lock()
		for !time.Now().Before(next) {
			g.Step()
			next = next.Add(g.TickInterval())
		}
		f := g.Frame()
		over := g.Over()
		if over {
			s.game, s.last = nil, g
		}
		s.mu.Unlock()
		s.show(f)
		if over {
			log.Printf("game: over, score %d", g.score)
			return
		}
	}
}

// ListenAndServe serves the controller page on addr (e.g. ":8080").
func (s *Server) ListenAndServe(addr string) {
	log.Printf("game: controller on http://%s/", addr)
	if err := http.ListenAndServe(addr, s.Handler()); err != nil {
		log.Printf("game: %v", err)
	}
}
