package cards

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSunAndMoon(t *testing.T) {
	// Sydney, 10 Nov 2026: sunrise ~05:47, sunset ~19:31 local (UTC+11)
	loc, _ := time.LoadLocation("Australia/Sydney")
	at, rise, ok := sunEvent(time.Date(2026, 11, 10, 12, 0, 0, 0, loc), -33.87, 151.21)
	if !ok || rise || at.In(loc).Hour() != 19 || at.In(loc).Minute() < 20 || at.In(loc).Minute() > 40 {
		t.Fatalf("sunset %v rise=%v ok=%v", at.In(loc), rise, ok)
	}
	at, rise, _ = sunEvent(time.Date(2026, 11, 10, 22, 0, 0, 0, loc), -33.87, 151.21)
	if !rise || at.In(loc).Hour() != 5 {
		t.Fatalf("sunrise %v rise=%v", at.In(loc), rise)
	}
	if s := sunAt(time.Date(2026, 11, 10, 13, 0, 0, 0, loc), -33.87, 151.21); s.elev < 65 || s.elev > 85 {
		t.Fatalf("midday elevation %.1f", s.elev)
	}
	// full moon 24 Nov 2026 ~14:53 UTC
	if a := moonAge(time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)); a < 0.46 || a > 0.54 {
		t.Fatalf("moon age at full moon %.3f", a)
	}
}

func TestWeatherCardsRender(t *testing.T) {
	specs := []string{
		`{"type":"weather","condition":"sunny"}`,
		`{"type":"weather","tz":"Australia/Sydney","lat":-33.9,"lon":151.2,"condition":"lightning-rainy","temperature":21,
		  "low":15,"high":25,"chance":80,"precip":12,"rate":5,"wind":45,"wind_unit":"km/h","bearing":250,"uv":4,"cloud":100}`,
		`{"type":"forecast","unit":"°F","days":[{"condition":"snowy","high":30,"low":20,"chance":100,"day":"MO"},
		  {"condition":"fog"},{"condition":"exceptional"}]}`,
	}
	for _, s := range specs {
		c, err := New(json.RawMessage(s), Env{})
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
		prev, _ := c.Render(now)
		changed := false
		for i := 1; i < 200; i++ {
			f, next := c.Render(now.Add(time.Duration(i) * 50 * time.Millisecond))
			if !next.After(now) || f == prev {
				t.Fatalf("%s: frame %d not a new frame", s, i)
			}
			changed = changed || *f != *prev
			prev = f
		}
		if !changed {
			t.Fatalf("%s: never changes", s)
		}
	}
	if _, err := New(json.RawMessage(`{"type":"forecast","days":[]}`), Env{}); err == nil {
		t.Fatal("empty forecast accepted")
	}
}

func BenchmarkStorm(b *testing.B) {
	c, _ := New(json.RawMessage(`{"type":"weather","condition":"lightning-rainy","temperature":21,"wind":45,"cloud":100,"low":1,"high":30}`), Env{})
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	for i := 0; i < b.N; i++ {
		c.Render(now.Add(time.Duration(i) * 50 * time.Millisecond))
	}
}
