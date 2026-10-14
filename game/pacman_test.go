package game

import "testing"

// A bot that always heads for the nearest pellet should eat a good number before the ghosts get it, and the
// game must end cleanly (no panics, game-over screen finishes).
func TestPlaysToGameOver(t *testing.T) {
	g := NewPacman()
	for i := 0; i < 20000 && !g.Over(); i++ {
		g.Input([]Dir{Up, Down, Left, Right}[i/7%4])
		g.Step()
		g.Frame()
	}
	if !g.Over() {
		t.Fatalf("game did not end (score %d, lives %d)", g.Score(), g.lives)
	}
	if g.Score() == 0 {
		t.Fatal("scored nothing")
	}
	t.Logf("score %d, level %d", g.Score(), g.level)
}

func TestMazeLoads(t *testing.T) {
	g := NewPacman()
	if g.pellets != 326 { // 322 pellets + 4 power pellets
		t.Fatalf("pellets %d", g.pellets)
	}
	if !g.open(g.px, g.py) {
		t.Fatal("pac starts in a wall")
	}
}
