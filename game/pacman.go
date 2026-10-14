// Package game: Pac-Man at one LED per tile, played from a phone (see the pixeld web page).
// Our own maze and code; the idea comes from a 32x32 LED-matrix Pac-Man project.
package game

import (
	_ "embed"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"pixeld/cards"
	"pixeld/panel"
)

//go:embed maze.txt
var mazeText string

type Dir int

const (
	None Dir = iota
	Up
	Down
	Left
	Right
)

var delta = map[Dir][2]int{Up: {0, -1}, Down: {0, 1}, Left: {-1, 0}, Right: {1, 0}}
var reverse = map[Dir]Dir{Up: Down, Down: Up, Left: Right, Right: Left, None: None}

const (
	mazeW, mazeH  = 32, 31
	frightTicks   = 45 // ~6 s at the starting speed
	startLives    = 3
	releaseEvery  = 25 // ticks between ghosts leaving the house
	deathPause    = 12
	levelUpPause  = 15
	readyPause    = 18 // ~2.5 s before the first move
	gameOverTicks = 0 // the score scroll decides
)

var (
	colWall   = cards.RGB{0, 6, 70}
	colDoor   = cards.RGB{200, 90, 140}
	colPellet = cards.RGB{120, 95, 60}
	colPower  = cards.RGB{255, 180, 140}
	colPac    = cards.RGB{255, 220, 0}
	colFright = cards.RGB{40, 40, 255}
	colEyes   = cards.RGB{200, 200, 255}
	ghostCols = []cards.RGB{{255, 0, 0}, {255, 140, 200}, {0, 230, 255}, {255, 150, 0}}
)

type ghost struct {
	x, y       int
	dir        Dir
	home       [2]int
	inHouse    bool
	eaten      bool // eyes on the way home
	frightened bool
	slow       bool // skip alternate moves (frightened)
}

type Pacman struct {
	tiles      [mazeH][mazeW]byte
	pellets    int
	px, py     int
	dir, want  Dir
	ghosts     []*ghost
	tick       int
	fright     int
	lives      int
	score      int
	level      int
	pause      int
	released   int
	over       bool
	overFrames []*panel.Frame
	overIdx    int
	rnd        *rand.Rand
}

func NewPacman() *Pacman {
	g := &Pacman{lives: startLives, level: 1, rnd: rand.New(rand.NewSource(time.Now().UnixNano()))}
	g.loadLevel()
	g.pause = readyPause // Pac-Man blinks: get ready
	return g
}

func (g *Pacman) loadLevel() {
	g.pellets = 0
	for y, line := range strings.Split(strings.TrimRight(mazeText, "\n"), "\n") {
		for x := 0; x < mazeW && x < len(line); x++ {
			g.tiles[y][x] = line[x]
			if line[x] == '.' || line[x] == 'o' {
				g.pellets++
			}
		}
	}
	g.resetPositions()
}

func (g *Pacman) resetPositions() {
	g.px, g.py, g.dir, g.want = 15, 23, Left, Left
	g.ghosts = []*ghost{
		{x: 15, y: 11, dir: Left, home: [2]int{15, 14}},
		{x: 13, y: 14, dir: Up, home: [2]int{13, 14}, inHouse: true},
		{x: 16, y: 14, dir: Up, home: [2]int{16, 14}, inHouse: true},
		{x: 18, y: 14, dir: Up, home: [2]int{18, 14}, inHouse: true},
	}
	g.released, g.fright = 0, 0
}

// Over reports that the game (including the score display) has finished.
func (g *Pacman) Over() bool { return g.over && g.overIdx >= len(g.overFrames) }

// Input steers Pac-Man (turns as soon as the way is open).
func (g *Pacman) Input(d Dir) {
	if d != None {
		g.want = d
	}
}

// TickInterval: movement speed for the current level.
func (g *Pacman) TickInterval() time.Duration {
	ms := 140 - 10*(g.level-1)
	if ms < 80 {
		ms = 80
	}
	if g.over {
		ms = 70
	}
	return time.Duration(ms) * time.Millisecond
}

func (g *Pacman) open(x, y int) bool {
	if y < 0 || y >= mazeH {
		return false
	}
	c := g.tiles[y][(x+mazeW)%mazeW]
	return c != '#' && c != 'H' && c != '-'
}

func (g *Pacman) ghostOpen(gh *ghost, x, y int) bool {
	if y < 0 || y >= mazeH {
		return false
	}
	c := g.tiles[y][(x+mazeW)%mazeW]
	if c == 'H' || c == '-' {
		return gh.inHouse || gh.eaten
	}
	return c != '#'
}

func wrap(x int) int { return (x + mazeW) % mazeW }

// Step advances the game by one tick.
func (g *Pacman) Step() {
	g.tick++
	if g.over {
		g.overIdx++
		return
	}
	if g.pause > 0 {
		g.pause--
		return
	}
	// Pac-Man
	if d := delta[g.want]; g.open(g.px+d[0], g.py+d[1]) {
		g.dir = g.want
	}
	if d := delta[g.dir]; g.open(g.px+d[0], g.py+d[1]) {
		g.px, g.py = wrap(g.px+d[0]), g.py+d[1]
	}
	switch g.tiles[g.py][g.px] {
	case '.':
		g.tiles[g.py][g.px] = ' '
		g.score += 10
		g.pellets--
	case 'o':
		g.tiles[g.py][g.px] = ' '
		g.score += 50
		g.pellets--
		g.fright = frightTicks
		for _, gh := range g.ghosts {
			if !gh.eaten && !gh.inHouse {
				gh.frightened, gh.dir = true, reverse[gh.dir]
			}
		}
	}
	if g.collide() {
		return
	}
	// ghosts
	if g.fright > 0 {
		if g.fright--; g.fright == 0 {
			for _, gh := range g.ghosts {
				gh.frightened = false
			}
		}
	}
	if g.released < len(g.ghosts)-1 && g.tick%releaseEvery == 0 {
		g.released++
	}
	for i, gh := range g.ghosts {
		if gh.inHouse && i <= g.released {
			g.leaveHouse(gh)
			continue
		}
		if gh.inHouse {
			continue
		}
		if gh.frightened && g.tick%2 == 0 {
			continue
		}
		g.moveGhost(i, gh)
		if gh.eaten && gh.x == gh.home[0] && gh.y == gh.home[1] {
			gh.eaten, gh.inHouse = false, true
		}
	}
	g.collide()
	if g.pellets == 0 {
		g.level++
		g.loadLevel()
		g.pause = levelUpPause
	}
}

func (g *Pacman) leaveHouse(gh *ghost) {
	// to the door column, then up and out
	switch {
	case gh.x < 15:
		gh.x++
	case gh.x > 16:
		gh.x--
	case gh.y > 11:
		gh.y--
	default:
		gh.inHouse, gh.dir = false, Left
	}
}

func dist2(ax, ay, bx, by int) int { dx, dy := ax-bx, ay-by; return dx*dx + dy*dy }

func (g *Pacman) target(i int, gh *ghost) (int, int) {
	if gh.eaten {
		return gh.home[0], gh.home[1]
	}
	d := delta[g.dir]
	switch i {
	case 1: // pink: four tiles ahead of Pac-Man
		return g.px + 4*d[0], g.py + 4*d[1]
	case 2: // cyan: wanders
		if g.rnd.Intn(3) == 0 {
			return g.rnd.Intn(mazeW), g.rnd.Intn(mazeH)
		}
		return g.px, g.py
	case 3: // orange: chases until close, then goes to its corner
		if dist2(gh.x, gh.y, g.px, g.py) < 64 {
			return 0, mazeH
		}
	}
	return g.px, g.py // red: straight at Pac-Man
}

func (g *Pacman) moveGhost(i int, gh *ghost) {
	tx, ty := g.target(i, gh)
	best, bestD := None, 1<<30
	options := []Dir{Up, Left, Down, Right}
	if gh.frightened {
		g.rnd.Shuffle(len(options), func(a, b int) { options[a], options[b] = options[b], options[a] })
	}
	for _, d := range options {
		if d == reverse[gh.dir] {
			continue // ghosts don't turn back
		}
		v := delta[d]
		nx, ny := wrap(gh.x+v[0]), gh.y+v[1]
		if !g.ghostOpen(gh, nx, ny) || (!gh.eaten && g.tiles[ny][nx] == '-') {
			continue
		}
		dd := dist2(nx, ny, tx, ty)
		if gh.frightened {
			dd = 0 // first open option of the shuffled list
		}
		if dd < bestD {
			best, bestD = d, dd
		}
	}
	if best == None {
		best = reverse[gh.dir]
	}
	v := delta[best]
	if g.ghostOpen(gh, gh.x+v[0], gh.y+v[1]) {
		gh.x, gh.y, gh.dir = wrap(gh.x+v[0]), gh.y+v[1], best
	}
}

// collide handles Pac-Man meeting a ghost; returns true if Pac-Man died.
func (g *Pacman) collide() bool {
	for _, gh := range g.ghosts {
		if gh.x != g.px || gh.y != g.py || gh.eaten || gh.inHouse {
			continue
		}
		if gh.frightened {
			gh.frightened, gh.eaten = false, true
			g.score += 200
			continue
		}
		g.lives--
		if g.lives <= 0 {
			g.gameOver()
		} else {
			g.resetPositions()
			g.pause = deathPause
		}
		return true
	}
	return false
}

func (g *Pacman) gameOver() {
	g.over = true
	msg := fmt.Sprintf("GAME OVER  SCORE %d  LEVEL %d", g.score, g.level)
	w := cards.TextWidth(msg, 2)
	for x := panel.Size; x > -w; x -= 2 {
		f := new(panel.Frame)
		cards.Text(f, msg, x, 11, 2, colPac)
		g.overFrames = append(g.overFrames, f)
	}
}

func (g *Pacman) Score() int { return g.score }

// edge: a wall tile next to (including diagonally) something that isn't wall.
func (g *Pacman) edge(x, y int) bool {
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			nx, ny := x+dx, y+dy
			if nx < 0 || ny < 0 || nx >= mazeW || ny >= mazeH {
				continue
			}
			if g.tiles[ny][nx] != '#' {
				return true
			}
		}
	}
	return false
}

// Frame draws the current state.
func (g *Pacman) Frame() *panel.Frame {
	if g.over {
		return g.overFrames[min(g.overIdx, len(g.overFrames)-1)]
	}
	f := new(panel.Frame)
	blink := (g.tick/3)%2 == 0
	for y := 0; y < mazeH; y++ {
		for x := 0; x < mazeW; x++ {
			switch g.tiles[y][x] {
			case '#':
				if g.edge(x, y) { // outline walls, as in the arcade game; solid blocks stay dark inside
					f[y][x] = colWall
				}
			case '-':
				f[y][x] = colDoor
			case '.':
				f[y][x] = colPellet
			case 'o':
				if blink {
					f[y][x] = colPower
				}
			}
		}
	}
	for i, gh := range g.ghosts {
		c := ghostCols[i]
		switch {
		case gh.eaten:
			c = colEyes
		case gh.frightened && g.fright < 15 && blink:
			c = cards.RGB{255, 255, 255}
		case gh.frightened:
			c = colFright
		}
		f[gh.y][gh.x] = c
	}
	if g.pause == 0 || blink {
		f[g.py][g.px] = colPac
	}
	for i := 0; i < g.lives; i++ { // lives along the bottom row
		f[31][1+i*2] = colPac
	}
	return f
}
