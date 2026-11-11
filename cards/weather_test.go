package cards

import (
	"encoding/json"
	"strings"
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
		`{"type":"clock","tz":"Australia/Sydney","seconds":true,"weather":{"lat":-33.9,"lon":151.2,"condition":"rainy",
		  "temperature":17,"wind":30,"cloud":90,"rate":2}}`,
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

func TestWeatherPages(t *testing.T) {
	spec := WeatherSpec{Condition: "sunny", Temp: ptr(20), Wind: ptr(10), UV: ptr(5), Low: ptr(10), High: ptr(25),
		Pages: []string{"temperature", "Wind", "uv", "nonsense", "rain", "wind"}, PageSeconds: ptr(2), TempSeconds: ptr(3)}
	w, err := NewWeather(spec)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range w.pages {
		got = append(got, p.id)
	}
	// in the order given, unknown and duplicate names dropped, no rain page (no rain)
	if want := "temperature wind uv"; strings.Join(got, " ") != want {
		t.Fatalf("pages %v, want %s", got, want)
	}
	if w.pages[0].dwell != 3 || w.pages[1].dwell != 2 {
		t.Fatalf("dwell %v %v", w.pages[0].dwell, w.pages[1].dwell)
	}
	t0 := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	showing := func(at time.Duration) string {
		w.Render(t0.Add(at))
		return w.pages[w.page].id
	}
	frames := func(from, to time.Duration) {
		for d := from; d < to; d += 50 * time.Millisecond {
			w.Render(t0.Add(d))
		}
	}
	frames(0, 4500*time.Millisecond) // temperature 0-3 s, then wind
	if p := showing(4500 * time.Millisecond); p != "wind" {
		t.Fatalf("at 4.5 s: %s", p)
	}
	// off screen while wind has been on 1.5 s (more than half): next time, temperature then uv
	if p := showing(60 * time.Second); p != "temperature" {
		t.Fatalf("back on screen: %s", p)
	}
	frames(60*time.Second, 63100*time.Millisecond)
	if p := showing(63100 * time.Millisecond); p != "uv" {
		t.Fatalf("after the first page: %s, want uv", p)
	}
	frames(63100*time.Millisecond, 65100*time.Millisecond)
	if p := showing(65100 * time.Millisecond); p != "temperature" {
		t.Fatalf("after uv: %s", p)
	}

	// temperature 65-68, wind from 68: seen 1.25 s, so uv after the first page next time
	frames(65100*time.Millisecond, 69300*time.Millisecond)
	showing(120 * time.Second)
	frames(120*time.Second, 123100*time.Millisecond)
	if p := showing(123100 * time.Millisecond); p != "uv" {
		t.Fatalf("seen wind: %s, want uv", p)
	}
	// uv from 123: off screen after 0.3 s, so uv again (after the first page)
	frames(123100*time.Millisecond, 123300*time.Millisecond)
	showing(180 * time.Second)
	frames(180*time.Second, 183100*time.Millisecond)
	if p := showing(183100 * time.Millisecond); p != "uv" {
		t.Fatalf("barely seen uv: %s, want uv", p)
	}

	// restart_pages: always from the top, then in order
	spec.RestartPages = true
	w, _ = NewWeather(spec)
	frames(0, 4500*time.Millisecond)
	showing(60 * time.Second)
	frames(60*time.Second, 63100*time.Millisecond)
	if p := showing(63100 * time.Millisecond); p != "wind" {
		t.Fatalf("restart_pages: %s, want wind", p)
	}

	// no pages: nothing in the strip, and it still renders
	spec.Pages = []string{}
	w, _ = NewWeather(spec)
	if len(w.pages) != 0 {
		t.Fatal("pages: [] should show none")
	}
	w.Render(t0)
}

func ptr(v float64) *float64 { return &v }
