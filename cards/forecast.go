package cards

import (
	"fmt"
	"math"
	"time"

	"pixeld/panel"
)

// ForecastSpec configures the live forecast card: up to three days side by side.
type ForecastSpec struct {
	TZ   string        `json:"tz"`
	Lat  *float64      `json:"lat"`
	Lon  *float64      `json:"lon"`
	Unit string        `json:"unit"`
	Days []ForecastDay `json:"days"`
	// PageSeconds: how long the highs and lows, then the days and rain chances, show (default 4 each).
	PageSeconds *float64 `json:"page_seconds"`
}

type ForecastDay struct {
	Condition string   `json:"condition"`
	High      *float64 `json:"high"`
	Low       *float64 `json:"low"`
	Chance    *float64 `json:"chance"` // % chance of rain
	Day       string   `json:"day"`    // e.g. "MO"; "" for none
	Today     bool     `json:"today"`
}

type Forecast struct {
	spec        ForecastSpec
	page        float64 // seconds for the highs and lows, and for the days and rain chance
	start, last time.Time
	cv          canvas
}

func NewForecast(spec ForecastSpec) (*Forecast, error) {
	if len(spec.Days) == 0 {
		return nil, fmt.Errorf("forecast: no days")
	}
	if len(spec.Days) > 3 {
		spec.Days = spec.Days[:3]
	}
	page := seconds(spec.PageSeconds, defaultPageSeconds, 1, 120)
	return &Forecast{spec: spec, page: page}, nil
}

const fcFade = 0.4

func (f *Forecast) Render(now time.Time) (*panel.Frame, time.Time) {
	if f.start.IsZero() || now.Sub(f.last) > 2*time.Second || now.Before(f.last) {
		f.start = now
	}
	f.last = now
	c := &f.cv
	*c = canvas{}
	t := float64(now.UnixMilli()%(1<<40)) / 1000
	night := false
	if f.spec.Lat != nil && f.spec.Lon != nil {
		night = sunAt(now, *f.spec.Lat, *f.spec.Lon).elev < -0.833
	}
	for i, d := range f.spec.Days {
		cond := normCond(d.Condition)
		if d.Today && night {
			switch cond {
			case "sunny":
				cond = "night"
			case "partly":
				cond = "partly-night"
			}
		}
		c.miniIcon(cond, float64(5+i*11), 6, t)
	}
	for _, x := range []int{10, 21} { // dividers after the icons, so a wide icon doesn't run into its neighbour
		for y := 0; y < panel.Size; y++ {
			c[y][x] = col{}
		}
		for y := 2; y < 30; y++ {
			c[y][x] = col{35, 35, 50}
		}
	}
	// the text alternates: highs and lows, then the days and the chance of rain
	fcHigh := f.page
	el := math.Mod(now.Sub(f.start).Seconds(), 2*f.page)
	a := 1.0 // how much of the highs/lows show
	switch {
	case el < fcFade && now.Sub(f.start).Seconds() > fcFade:
		a = el / fcFade
	case el >= fcHigh && el < fcHigh+fcFade:
		a = 1 - (el-fcHigh)/fcFade
	case el >= fcHigh+fcFade:
		a = 0
	}
	for i, d := range f.spec.Days {
		cx := 5 + i*11
		if a > 0 {
			if d.High != nil {
				s := deg(*d.High)
				c.narrow(s, cx, 15, tempCol(*d.High, f.spec.Unit), a)
			}
			if d.Low != nil {
				s := deg(*d.Low)
				c.narrow(s, cx, 22, scalec(col{120, 170, 255}, 0.9), a)
			}
		}
		if a < 1 {
			if d.Day != "" {
				k := col{200, 200, 215}
				if d.Today {
					k = col{255, 255, 255}
				}
				c.narrow(d.Day, cx, 15, k, 1-a)
			}
			if d.Chance != nil {
				c.narrow(fmt.Sprintf("%d", int(math.Round(*d.Chance))), cx, 22, col{90, 160, 255}, 1-a)
			}
		}
		// rain chance bar
		if d.Chance != nil {
			n := int(math.Round(clamp01(*d.Chance/100) * 9))
			for x := 0; x < 9; x++ {
				k := col{14, 18, 32}
				if x < n {
					k = lerpc(col{40, 90, 220}, col{120, 190, 255}, float64(x)/8)
				}
				c[30][cx-4+x] = k
			}
		}
		if d.Today {
			for x := cx - 4; x <= cx+4; x++ {
				c.blend(x, 31, col{255, 255, 255}, 0.55)
			}
		}
	}
	return c.frame(), now.Add(50 * time.Millisecond)
}

// narrow draws s centred on cx, squeezing "1"s to one pixel when it would be wider than a column (9 px).
func (c *canvas) narrow(s string, cx, y int, k col, a float64) {
	w := TextWidth(s, 1)
	squeeze := w > 9
	if squeeze {
		w = 0
		for _, r := range s {
			if r == '1' {
				w += 2
			} else {
				w += 4
			}
		}
		w--
	}
	x := cx - w/2
	for _, r := range s {
		if squeeze && r == '1' {
			for row := 0; row < 5; row++ {
				c.blend(x, y+row, k, a)
			}
			x += 2
			continue
		}
		c.text(string(r), x, y, k, a)
		x += 4
	}
}

// miniIcon is a ~10x10 animated icon (animations.mini_icon), drawn live: t in seconds.
func (c *canvas) miniIcon(cond string, cx, cy, t float64) {
	ph := frac(t / 3.6)
	s := math.Sin(tau * ph)
	pulse := 0.5 + 0.5*math.Sin(tau*ph*2)
	small := func(r float64) sunStyle {
		st := defaultSun(r)
		st.rayIn, st.rayLong, st.rayShort, st.halo = 1.2, 2.6, 1.8, 1.4
		return st
	}
	switch cond {
	case "sunny":
		c.sun(cx, cy, small(2.2), 2*tau/8*ph, pulse, 1)
	case "night":
		c.crescent(cx, cy, 3.2, 0.5+0.5*math.Sin(tau*ph))
	case "partly", "partly-night":
		if cond == "partly" {
			st := small(1.8)
			st.rayIn, st.rayLong, st.rayShort, st.halo = 1.0, 2.2, 1.5, 1.2
			c.sun(cx-1.8, cy-1.5, st, 2*tau/8*ph, pulse, 1)
		} else {
			c.crescent(cx-1.5, cy-1.5, 2.4, 0.5)
		}
		c.puff(cx+1+0.4*s, cy+1.5, 2.2, cloudWhite, 0, 1)
	case "rain", "pouring", "sleet", "storm":
		f := 0.0
		if cond == "storm" {
			f, _ = lightning(t)
		}
		n := 3
		if cond == "pouring" {
			n = 4
		}
		c.rain(t, rainOpts{x0: cx - 3.5, x1: cx + 3.5, y0: cy - 1, y1: cy + 5, n: n, speed: 12, length: 2,
			slant: 0.2, seed: 7})
		if cond == "sleet" {
			c.snow(t, snowOpts{x0: cx - 2, x1: cx + 2, y0: cy, y1: cy + 5, n: 2, speed: 2, sway: 0.8, seed: 8})
		}
		if f > 0.3 {
			c.bolt([][2]float64{{cx + 0.5, cy}, {cx - 1, cy + 2}, {cx + 1, cy + 3}, {cx - 0.5, cy + 5}}, f)
		}
		pal := cloudRain
		if cond == "storm" || cond == "pouring" {
			pal = cloudStorm
		}
		c.puff(cx, cy-1.5, 2.3, pal, f, 1)
	case "snow":
		c.snow(t, snowOpts{x0: cx - 3.5, x1: cx + 3.5, y0: cy, y1: cy + 5, n: 3, speed: 1.5, sway: 0.8, seed: 9})
		c.puff(cx, cy-1.5, 2.3, cloudSnow, 0, 1)
	case "hail":
		c.hail(t, cx-3.5, cx+3.5, cy, cy+5, 3, 1.7, 3)
		c.puff(cx, cy-1.5, 2.3, cloudGrey, 0, 1)
	case "fog":
		x0, x1 := int(cx-4), int(cx+4)
		c.fog(16*ph*1.0, x0, x1, [][2]float64{{cy - 2, 0.8}, {cy + 0.5, 0.8}, {cy + 3, 0.8}}, col{190, 195, 210}, 1)
	case "windy":
		for i, g := range [3][3]float64{{cy - 2, 5, 0}, {cy + 1, 6, 0.5}, {cy + 4, 4, 0.3}} {
			p := frac(t/(3.6/float64(1+i%2)) + g[2])
			hx := cx - 6 + p*14
			c.gust(hx, g[0], g[1], ph, 1, clamp01((hx-(cx-5))/2)*clamp01((cx+5-hx)/2), col{200, 230, 255})
		}
	case "exceptional":
		k := lerpc(col{255, 140, 20}, col{255, 215, 60}, pulse)
		for y := int(cy) - 3; y <= int(cy)+1; y++ {
			c.blend(int(cx), y, k, 1)
		}
		c.blend(int(cx), int(cy)+3, k, 1)
	default:
		c.puff(cx, cy, 2.6, cloudGrey, 0, 1)
	}
}
