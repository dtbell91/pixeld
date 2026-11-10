package cards

import (
	"fmt"
	"math"
	"strings"
	"time"

	"pixeld/panel"
)

// WeatherSpec configures the live weather card (JSON from Home Assistant). Everything but the condition is
// optional; what is missing is left out (or, for the sky, guessed from the condition).
type WeatherSpec struct {
	TZ        string   `json:"tz"`
	Lat       *float64 `json:"lat"`
	Lon       *float64 `json:"lon"`
	Hour12    bool     `json:"hour12"`
	Unit      string   `json:"unit"`      // temperature unit, "°C" (default) or "°F"
	Condition string   `json:"condition"` // Home Assistant weather condition, e.g. "partlycloudy"
	Temp      *float64 `json:"temperature"`
	Apparent  *float64 `json:"apparent"` // feels like
	Low       *float64 `json:"low"`      // today's forecast
	High      *float64 `json:"high"`
	Chance    *float64 `json:"chance"`      // % chance of rain today
	Precip    *float64 `json:"precip"`      // expected today
	Rate      *float64 `json:"rate"`        // expected in the next hour (how hard it rains now)
	PUnit     string   `json:"precip_unit"` // "mm" (default) or "in"
	Wind      *float64 `json:"wind"`
	WindUnit  string   `json:"wind_unit"` // "km/h" (default), "m/s", "mph", "kn", "ft/s"
	Bearing   *float64 `json:"bearing"`   // where the wind comes from, degrees
	UV        *float64 `json:"uv"`
	Cloud     *float64 `json:"cloud"` // cloud coverage %
}

// The condition's icon family (animations.HA_CONDITIONS).
var weatherConds = map[string]string{
	"sunny": "sunny", "clear-night": "night", "partlycloudy": "partly", "cloudy": "cloudy",
	"fog": "fog", "rainy": "rain", "pouring": "pouring", "lightning": "storm",
	"lightning-rainy": "storm", "snowy": "snow", "snowy-rainy": "sleet", "hail": "hail",
	"windy": "windy", "windy-variant": "windy", "exceptional": "exceptional",
}

func normCond(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if c, ok := weatherConds[s]; ok {
		return c
	}
	for _, c := range weatherConds {
		if c == s {
			return c
		}
	}
	return "cloudy"
}

const (
	groundY   = 18 // the horizon: sky above, two rows of ground, the info strip from row 20
	stripY    = 20
	stripRows = panel.Size - stripY
)

type Weather struct {
	spec    WeatherSpec
	loc     *time.Location
	cond    string
	lat     float64
	lon     float64
	view    float64 // +1: facing south (northern hemisphere: east on the left), -1: facing north
	windKmh float64
	windVX  float64 // wind across the screen, km/h (+ right)
	cover   float64 // cloud coverage 0..1
	rate    float64 // precipitation now, mm/h
	clouds  []cloud
	pages   []page

	start, last time.Time // when the card came on screen (for the info pages); last frame drawn
	cv          canvas
}

type cloud struct {
	x0, y, u, speed float64
	far             bool
	pal             palette
}

type page struct {
	dwell float64 // seconds
	draw  func(c *canvas, now time.Time)
}

func NewWeather(spec WeatherSpec) (*Weather, error) {
	w := &Weather{spec: spec, loc: time.UTC, cond: normCond(spec.Condition), view: 1}
	if spec.TZ != "" {
		l, err := time.LoadLocation(spec.TZ)
		if err != nil {
			return nil, fmt.Errorf("weather: %w", err)
		}
		w.loc = l
	}
	if spec.Lat != nil && spec.Lon != nil {
		w.lat, w.lon = *spec.Lat, *spec.Lon
	} else {
		w.lat, w.lon = 51.5, 0 // somewhere: the sky still follows the light's clock
	}
	if w.lat < 0 {
		w.view = -1
	}
	if spec.Wind != nil {
		w.windKmh = kmh(*spec.Wind, spec.WindUnit)
	}
	east := 1.0 // no bearing: drift gently one way
	if spec.Bearing != nil {
		east = math.Sin((*spec.Bearing + 180) * rad) // the wind blows towards bearing+180
	}
	if math.Abs(east) < 0.35 { // mostly north/south: still drift a little, the way it leans
		if east < 0 {
			east = -0.35
		} else {
			east = 0.35
		}
	}
	w.windVX = -w.view * east * math.Max(w.windKmh, 6)
	w.cover = defaultCover[w.cond]
	if spec.Cloud != nil {
		w.cover = clamp01(*spec.Cloud / 100)
		if w.cond == "partly" || w.cond == "sunny" || w.cond == "night" {
			w.cover = math.Min(w.cover, 0.75) // the condition says the sun is out
		}
		if precipCond(w.cond) || w.cond == "cloudy" {
			w.cover = math.Max(w.cover, 0.7)
		}
	}
	w.rate = defaultRate[w.cond]
	if spec.Rate != nil && *spec.Rate > 0 && precipCond(w.cond) {
		w.rate = *spec.Rate
		if spec.PUnit == "in" {
			w.rate *= 25.4
		}
	}
	w.clouds = w.makeClouds()
	w.pages = w.makePages()
	return w, nil
}

var defaultCover = map[string]float64{"sunny": 0.08, "night": 0.08, "partly": 0.45, "cloudy": 0.9, "fog": 0.6,
	"rain": 0.92, "pouring": 1, "storm": 1, "snow": 0.9, "sleet": 0.92, "hail": 0.95, "windy": 0.35,
	"exceptional": 0.5}

var defaultRate = map[string]float64{"rain": 1.2, "pouring": 7, "storm": 4, "snow": 1, "sleet": 1, "hail": 2}

func precipCond(c string) bool { _, ok := defaultRate[c]; return ok }

func kmh(v float64, u string) float64 {
	switch strings.ToLower(u) {
	case "m/s":
		return v * 3.6
	case "mph":
		return v * 1.609344
	case "kn", "kt", "knots":
		return v * 1.852
	case "ft/s":
		return v * 1.09728
	}
	return v
}

func toC(t float64, unit string) float64 {
	if strings.HasSuffix(strings.ToUpper(unit), "F") {
		return (t - 32) * 5 / 9
	}
	return t
}

func (w *Weather) palettes() (near, far palette) {
	switch w.cond {
	case "fog":
		return cloudFog, cloudFog.dim(0.8)
	case "rain", "sleet":
		return cloudRain, cloudRain.dim(0.75)
	case "pouring", "storm":
		return cloudStorm, cloudStorm.dim(0.8)
	case "snow":
		return cloudSnow, cloudGrey
	case "hail":
		return cloudGrey, cloudFar
	case "cloudy":
		return cloudGrey, cloudFar
	}
	return cloudWhite, cloudFar
}

// makeClouds lays out the clouds for the coverage: a few far ones high up, nearer, bigger ones lower down.
func (w *Weather) makeClouds() []cloud {
	near, far := w.palettes()
	nFar := int(math.Round(w.cover * 3))
	nNear := int(math.Round(w.cover * 3.4))
	if w.cover >= 0.3 && nNear < 2 { // so one is always in view
		nNear = 2
	}
	if w.cover > 0.02 && nFar+nNear == 0 {
		nFar = 1
	}
	seed := len(w.cond) * 7
	var cs []cloud
	for i := 0; i < nFar; i++ {
		cs = append(cs, cloud{x0: (float64(i) + unit(seed, i)*0.6) / float64(nFar), y: 3 + unit(seed+1, i)*2.5,
			u: 2.1 + unit(seed+2, i)*0.8, speed: 0.55, far: true, pal: far})
	}
	for i := 0; i < nNear; i++ {
		pal := near
		if w.cond == "cloudy" && i%2 == 1 {
			pal = cloudWhite
		}
		u := 3.6 + unit(seed+4, i)*1.3 + w.cover*0.6
		y := 6 + unit(seed+5, i)*2.5
		if precipCond(w.cond) { // higher, to leave room for what falls from them
			u, y = math.Min(u, 4.4), 4+unit(seed+5, i)*1.5
		}
		cs = append(cs, cloud{x0: (float64(i) + 0.3 + unit(seed+3, i)*0.5) / float64(nNear),
			y: y, u: u, speed: 1, pal: pal})
	}
	return cs
}

// Render draws the weather for now; the scene moves continuously, so it returns the next frame's time.
func (w *Weather) Render(now time.Time) (*panel.Frame, time.Time) {
	if w.start.IsZero() || now.Sub(w.last) > 2*time.Second || now.Before(w.last) {
		w.start = now // back on screen: start again from the first page
	}
	w.last = now
	c := &w.cv
	*c = canvas{}
	t := float64(now.UnixMilli()%(1<<40)) / 1000
	w.drawSky(c, now, t)
	w.drawStrip(c, now)
	return c.frame(), now.Add(50 * time.Millisecond)
}

// ---------- the sky ----------

type skyColors struct{ top, bottom col }

var (
	skyNight    = skyColors{col{1, 2, 10}, col{3, 5, 18}}
	skyTwilight = skyColors{col{8, 10, 38}, col{55, 22, 42}}
	skyGolden   = skyColors{col{14, 30, 78}, col{100, 50, 22}}
	skyDay      = skyColors{col{6, 30, 88}, col{34, 74, 125}}
)

func mixSky(a, b skyColors, f float64) skyColors {
	f = clamp01(f)
	return skyColors{lerpc(a.top, b.top, f), lerpc(a.bottom, b.bottom, f)}
}

func skyFor(elev float64) skyColors {
	switch {
	case elev < -12:
		return skyNight
	case elev < -4:
		return mixSky(skyNight, skyTwilight, (elev+12)/8)
	case elev < 2:
		return mixSky(skyTwilight, skyGolden, (elev+4)/6)
	default:
		return mixSky(skyGolden, skyDay, (elev-2)/10)
	}
}

func grey(k col, f float64) col {
	l := 0.3*k[0] + 0.59*k[1] + 0.11*k[2]
	return lerpc(k, col{l, l, l * 1.1}, f)
}

func (w *Weather) skyX(ha, ha0 float64) float64 {
	if ha0 < 1 || ha0 > 179 {
		ha0 = 180
	}
	return 16 + w.view*14.5*ha/ha0
}

func skyY(elev, peak float64) float64 {
	peak = math.Max(peak, 12)
	return groundY - 1.5 - (groundY-7.5)*clamp01(elev/peak)
}

func (w *Weather) drawSky(c *canvas, now time.Time, t float64) {
	s := sunAt(now, w.lat, w.lon)
	day := clamp01((s.elev + 6) / 12) // 0 night .. 1 day
	dark := clamp01((-s.elev - 3) / 9)
	sky := skyFor(s.elev)
	overcast := clamp01((w.cover - 0.55) / 0.45)
	gloom := 1.0
	switch w.cond {
	case "rain", "sleet", "snow", "hail":
		gloom = 0.8
	case "pouring", "storm":
		gloom = 0.6
	case "fog":
		gloom = 0.9
	}
	sky.top = scalec(grey(sky.top, overcast*0.75), gloom)
	sky.bottom = scalec(grey(sky.bottom, overcast*0.75), gloom)
	flash := 0.0
	var boltPts [][2]float64
	if w.cond == "storm" {
		flash, boltPts = lightning(t)
	}
	for y := 0; y < groundY; y++ {
		k := lerpc(sky.top, sky.bottom, float64(y)/float64(groundY-1))
		if flash > 0 {
			k = lerpc(k, col{70, 62, 120}, flash*0.5*(1-float64(y)/30))
		}
		for x := 0; x < panel.Size; x++ {
			c[y][x] = k
		}
	}

	// stars and the odd shooting star on clear nights, behind everything
	if dark > 0 && w.cover < 0.95 {
		n := int(14 * (1 - w.cover*0.6))
		for i := 0; i < n; i++ {
			x, y := int(mix(91, i)%32), int(mix(92, i)%uint32(groundY-3))
			c.twinkle(x, y, 0.15+unit(93, i)*0.35, unit(94, i), t, dark*(0.5+0.5*unit(95, i)))
		}
		if w.cover < 0.6 {
			win := math.Floor(t / 11)
			if unit(96, int(win)) < 0.45 {
				s0 := (t - win*11 - 2 - unit(97, int(win))*6) / 0.8
				if s0 >= 0 && s0 <= 1 {
					sx, sy := 4+unit(98, int(win))*24, 1+unit(99, int(win))*4
					dx := 7.0
					if sx > 16 {
						dx = -7
					}
					c.streak(sx+dx*s0, sy+4*s0, dx, 4, math.Sin(math.Pi*s0)*dark)
				}
			}
		}
	}

	// the moon, at its phase, roughly where it is
	age := moonAge(now)
	if lit := 0.5 * (1 - math.Cos(tau*age)); lit > 0.04 {
		mha := moonHA(s, age)
		me := moonElev(mha, w.lat)
		mx, my := w.skyX(mha, 90), skyY(me, 90-math.Abs(w.lat))
		if s.elev > -3 && math.Hypot(mx-w.skyX(s.ha, s.ha0), my-skyY(s.elev, s.peak)) < 10 {
			me = -90 // lost in the sun's glare
		}
		if me > -3 {
			alpha := 0.35 + 0.65*clamp01((-s.elev+2)/8)
			alpha *= 1 - 0.5*overcast
			c.moon(mx, my, 3.2, age, w.lat < 0, alpha)
		}
	}

	// the sun, its halo and rays from the UV index, redder low down
	if s.elev > -3 {
		uv := 3 + 5*clamp01(s.elev/50)*(1-w.cover*0.6)
		if w.spec.UV != nil {
			uv = math.Max(0, *w.spec.UV)
		}
		st := defaultSun(3.2)
		st.halo = 1.8 + 0.22*math.Min(uv, 12)
		st.haloA = 0.2 + 0.03*math.Min(uv, 12)
		st.rayLong, st.rayShort = 3.2+0.12*math.Min(uv, 12), 2.3+0.08*math.Min(uv, 12)
		if uv >= 8 {
			st.core = col{255, 255, 235}
		}
		if low := clamp01(1 - s.elev/7); low > 0 {
			st.core = lerpc(st.core, col{255, 205, 130}, low)
			st.edge = lerpc(st.edge, col{255, 85, 20}, low)
			st.ray = lerpc(st.ray, col{255, 110, 40}, low)
			st.haloCol = lerpc(st.haloCol, col{255, 50, 10}, low)
			st.rayLong *= 1 - 0.4*low
			st.rayShort *= 1 - 0.4*low
		}
		alpha := 1 - 0.55*overcast
		if w.cond == "fog" {
			alpha *= 0.5
		}
		c.sun(w.skyX(s.ha, s.ha0), skyY(s.elev, s.peak), st, t*0.22, 0.5+0.5*math.Sin(t*1.7), alpha)
	}

	// clouds drift with the wind, lit by the sky: pink at sunrise and sunset, dim at night
	tint := clamp01(1-math.Abs(s.elev-1)/6) * 0.45
	light := 0.32 + 0.68*day
	drift := w.windVX * 0.055 // px per second
	draw := func(cl cloud, flash float64) {
		span := float64(panel.Size) + 4.4*cl.u
		x := -2.2*cl.u + frac(cl.x0+t*drift*cl.speed/span)*span
		pal := cl.pal.dim(light)
		pal.light = lerpc(pal.light, col{255, 160, 140}, tint)
		pal.shadow = lerpc(pal.shadow, col{120, 60, 90}, tint*0.7)
		bob := 0.4 * math.Sin(t*0.4+cl.x0*9)
		c.puff(x, cl.y+bob, cl.u, pal, flash, 1)
	}
	for _, cl := range w.clouds {
		if cl.far {
			draw(cl, flash*0.5)
		}
	}

	// precipitation falls from under the nearer clouds
	slant := clampf(w.windVX*0.012, -0.9, 0.9)
	if precipCond(w.cond) {
		r := w.rate
		n := int(clampf(math.Round(4+7*math.Log1p(r)), 4, 24))
		switch w.cond {
		case "rain", "pouring", "storm":
			c.rain(t, rainOpts{x0: -1, x1: 33, y0: 8, y1: groundY - 0.5, n: n, speed: 15 + 6*math.Log1p(r),
				length: 2.2 + 0.6*math.Log1p(r), slant: slant, splash: true, wrap: true, seed: 1, alpha: 0.75 + 0.25*day})
		case "sleet":
			c.rain(t, rainOpts{x0: -1, x1: 33, y0: 7, y1: groundY - 0.5, n: n * 2 / 3, speed: 15, length: 2.2,
				slant: slant, splash: true, wrap: true, seed: 5})
			c.snow(t, snowOpts{x0: 0, x1: 32, y0: 8, y1: groundY - 1, n: n / 2, speed: 3.5, drift: w.windVX * 0.04,
				sway: 1, seed: 6, wrap: true})
		case "snow":
			c.snow(t, snowOpts{x0: 0, x1: 32, y0: 8, y1: groundY - 1, n: n + 2, speed: 2.6, drift: w.windVX * 0.05,
				sway: 1.1, seed: 2, wrap: true})
		case "hail":
			c.hail(t, 1, 31, 9, groundY-1, n, 0.55, 3)
		}
	}
	if len(boltPts) > 0 && flash > 0.3 {
		c.bolt(boltPts, flash)
	}
	for _, cl := range w.clouds {
		if !cl.far {
			draw(cl, flash)
		}
	}

	if w.cond == "fog" {
		c.fog(t*(1.2+math.Abs(drift)*0.6), 0, 31, [][2]float64{{9.5, 0.9}, {12, 0.9}, {14.5, 0.9}, {16.8, 0.9}},
			scalec(col{190, 195, 210}, 0.45+0.55*day), 1)
	}

	// wind: streaks once it's breezy, and leaves (petals in spring) in a strong wind
	windy := w.windKmh
	if w.cond == "windy" {
		windy = math.Max(windy, 40)
	}
	dir := 1.0
	if w.windVX < 0 {
		dir = -1
	}
	if windy >= 22 {
		n := int(clampf(1+(windy-22)/12, 1, 4))
		speed := 9 + windy*0.35
		for i := 0; i < n; i++ {
			lane := 40.0 + unit(31, i)*20
			p := frac(t*speed/lane + unit(32, i))
			hx := -6 + p*lane
			if dir < 0 {
				hx = 37 - p*lane
			}
			y := 3 + float64(mix(33, i)%12) + float64(i%2)
			c.gust(hx, y, 6+unit(34, i)*5, t*1.4, dir, (0.35+0.35*day)*clamp01((windy-18)/10), col{200, 230, 255})
		}
	}
	if windy >= 35 && !precipCond(w.cond) {
		ss := season(now, w.lat)
		if ss != 2 {
			leafCols := map[int][3]col{
				0: {{120, 200, 70}, {70, 160, 40}, {40, 110, 30}},
				1: {{240, 160, 40}, {220, 110, 30}, {170, 70, 20}},
				3: {{255, 190, 215}, {255, 150, 190}, {220, 110, 160}},
			}[ss]
			for i := 0; i < 2; i++ {
				lane := 46.0
				p := frac(t*(6+windy*0.18)/lane + float64(i)*0.5)
				x := -4 + p*lane
				if dir < 0 {
					x = 36 - p*lane
				}
				y := 9 + 4*float64(i) + 2.5*math.Sin(t*2.3+float64(i)*2)
				a := t*4 + float64(i)
				for k := -1; k <= 1; k++ {
					c.dot(x+math.Cos(a)*float64(k)*0.9, y+math.Sin(a)*float64(k)*0.9, scalec(leafCols[k+1], 0.4+0.6*day),
						1, false, false)
				}
			}
		}
	}

	// heat shimmer near the ground on a hot day
	if w.spec.Temp != nil && toC(*w.spec.Temp, w.spec.Unit) >= 32 && s.elev > 5 && !precipCond(w.cond) {
		heat := clamp01((toC(*w.spec.Temp, w.spec.Unit) - 30) / 8)
		for y := 11; y < groundY; y++ {
			off := heat * 0.6 * math.Sin(t*5+float64(y)*1.4) * float64(y-10) / 7
			row := c[y]
			for x := 0; x < panel.Size; x++ {
				sx := float64(x) + off
				x0 := int(math.Floor(sx))
				f := sx - float64(x0)
				a, b := row[max(0, min(31, x0))], row[max(0, min(31, x0+1))]
				c[y][x] = lerpc(a, b, f)
			}
		}
	}

	if w.cond == "exceptional" {
		c.warning(16, 0, 0.5+0.5*math.Sin(t*3.5))
	}
	w.drawGround(c, now, t, day)
}

// lightning returns how bright the sky flashes now (0 most of the time) and the bolt's path.
func lightning(t float64) (float64, [][2]float64) {
	const every = 4.5
	win := int(math.Floor(t / every))
	if unit(71, win) > 0.7 {
		return 0, nil
	}
	dt := t - float64(win)*every - 0.5 - unit(72, win)*2.8
	var f float64
	switch {
	case dt < 0:
	case dt < 0.08:
		f = 1
	case dt < 0.16:
		f = 0.6
	case dt < 0.22:
		f = 0.15
	case dt < 0.3:
		f = 0.9
	case dt < 0.42:
		f = 0.45
	case dt < 0.55:
		f = 0.15
	}
	if f == 0 {
		return 0, nil
	}
	x := 6 + unit(73, win)*20
	pts := [][2]float64{{x, 8}}
	y := 8.0
	for k := 0; y < groundY-0.5; k++ {
		y = math.Min(groundY-0.5, y+2.2+unit(74+k, win)*1.5)
		x += (unit(80+k, win) - 0.5) * 4.5
		pts = append(pts, [2]float64{x, y})
	}
	return f, pts
}

func (w *Weather) drawGround(c *canvas, now time.Time, t, day float64) {
	ss := season(now, w.lat)
	g := map[int]col{0: {22, 46, 14}, 1: {40, 34, 12}, 2: {26, 30, 20}, 3: {18, 50, 18}}[ss]
	frosty := w.spec.Temp != nil && toC(*w.spec.Temp, w.spec.Unit) <= 0
	snowy := w.cond == "snow" || w.cond == "hail" || (w.cond == "sleet" && frosty)
	wet := w.cond == "rain" || w.cond == "pouring" || w.cond == "storm" || w.cond == "sleet"
	switch {
	case snowy:
		g = col{150, 160, 185}
	case frosty:
		g = col{95, 110, 135}
	case wet:
		g = scalec(g, 0.7)
		g[2] += 10
	}
	g = scalec(g, 0.3+0.7*day)
	for x := 0; x < panel.Size; x++ {
		c[groundY][x] = g
		c[groundY+1][x] = scalec(g, 0.35)
	}
	if snowy || frosty { // glints in the frost/snow
		for i := 0; i < 3; i++ {
			life := int(math.Floor(t/0.9 + float64(i)/3))
			p := frac(t/0.9 + float64(i)/3)
			x := int(mix(61+i, life) % 32)
			c.glow(x, groundY, col{255, 255, 255}, math.Sin(math.Pi*p)*0.8)
		}
	}
	if wet { // puddles catch the light
		for i := 0; i < 4; i++ {
			x := int(mix(65, i) % 32)
			c.glow(x, groundY+1, scalec(col{90, 130, 200}, 0.3+0.7*day), 0.25+0.2*math.Sin(t*3+float64(i)))
		}
	}
}

// ---------- the info strip: temperature, then the rest of the data in turn ----------

var arrowUp = uint16(0b010_111_010_010_010)
var arrowDown = uint16(0b010_010_010_111_010)
var drop = uint16(0b010_010_111_111_010)

func (c *canvas) glyphBits(g uint16, x, y int, k col, a float64) {
	for i := 0; i < 15; i++ {
		if g>>(14-i)&1 == 1 {
			c.blend(x+i%3, y+i/3, k, a)
		}
	}
}

func tempCol(t float64, unit string) col {
	c := toC(t, unit)
	switch {
	case c <= 10:
		return col{90, 170, 255}
	case c <= 19:
		return col{170, 255, 200}
	case c <= 27:
		return col{255, 190, 60}
	}
	return col{255, 80, 60}
}

// tempRamp is a smooth version of tempCol for the day's range bar.
func tempRamp(c float64) col {
	stops := []struct {
		t float64
		k col
	}{{-5, col{150, 120, 255}}, {5, col{80, 150, 255}}, {13, col{120, 230, 220}}, {19, col{190, 255, 170}},
		{24, col{255, 200, 60}}, {30, col{255, 110, 50}}, {38, col{255, 40, 60}}}
	if c <= stops[0].t {
		return stops[0].k
	}
	for i := 1; i < len(stops); i++ {
		if c <= stops[i].t {
			return lerpc(stops[i-1].k, stops[i].k, (c-stops[i-1].t)/(stops[i].t-stops[i-1].t))
		}
	}
	return stops[len(stops)-1].k
}

func deg(v float64) string { return fmt.Sprintf("%d", int(math.Round(v))) }

// bigTemp draws the temperature in 6x10 digits, light at the top shading darker (animations.draw_temp).
func (c *canvas) bigTemp(v float64, unit string, y int) {
	s := deg(v)
	k := tempCol(v, unit)
	x := (panel.Size - (TextWidth(s, 2) + 1 + 3)) / 2
	for row := 0; row < 5; row++ {
		rk := lerpc(lerpc(k, col{255, 255, 255}, 0.3), scalec(k, 0.8), float64(row)/4)
		for i, r := range s {
			g := glyph(r)
			for b := 0; b < 15; b++ {
				if g>>(14-b)&1 == 1 && b/3 == row {
					gx := x + i*8 + (b%3)*2
					for dy := 0; dy < 2; dy++ {
						c.blend(gx, y+row*2+dy, rk, 1)
						c.blend(gx+1, y+row*2+dy, rk, 1)
					}
				}
			}
		}
	}
	x += len(s) * 8
	for _, d := range [4][2]int{{1, 0}, {0, 1}, {2, 1}, {1, 2}} {
		c.blend(x+d[0], y+d[1], k, 1)
	}
}

// line draws a small line of text centred, with an optional glyph in front.
func (c *canvas) line(g uint16, s string, y int, gk, k col) {
	w := TextWidth(s, 1)
	if g != 0 {
		w += 4
	}
	x := (panel.Size - w) / 2
	if g != 0 {
		c.glyphBits(g, x, y, gk, 1)
		x += 4
	}
	c.text(s, x, y, k, 1)
}

func amount(v float64, unit string) string {
	if unit == "in" {
		return strings.TrimPrefix(fmt.Sprintf("%.1fIN", v), "0")
	}
	if v < 1 && v > 0 {
		return fmt.Sprintf("%.1fMM", v)
	}
	return fmt.Sprintf("%dMM", int(math.Round(v)))
}

var compass = []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}

func uvLevel(uv float64) (string, col) {
	switch u := math.Round(uv); {
	case u <= 2:
		return "LOW", col{90, 220, 90}
	case u <= 5:
		return "MOD", col{255, 220, 50}
	case u <= 7:
		return "HIGH", col{255, 140, 30}
	case u <= 10:
		return "V.HIGH", col{255, 60, 50}
	}
	return "EXTREME", col{190, 90, 255}
}

func (w *Weather) makePages() []page {
	s := w.spec
	var ps []page
	if s.Temp != nil {
		ps = append(ps, page{5, func(c *canvas, now time.Time) {
			c.bigTemp(*s.Temp, s.Unit, 1)
			if s.Low != nil && s.High != nil && *s.High > *s.Low {
				lo, hi := toC(*s.Low, s.Unit), toC(*s.High, s.Unit)
				for x := 2; x <= 29; x++ {
					c.blend(x, 11, scalec(tempRamp(lo+(hi-lo)*float64(x-2)/27), 0.35), 1)
				}
				p := clamp01((toC(*s.Temp, s.Unit) - lo) / (hi - lo))
				mx := 2 + p*26
				k := lerpc(tempRamp(toC(*s.Temp, s.Unit)), col{255, 255, 255}, 0.35)
				c.dot(mx, 11, k, 1, false, false)
				c.dot(mx+1, 11, k, 1, false, false)
			}
		}})
	}
	if s.High != nil || s.Low != nil {
		ps = append(ps, page{3, func(c *canvas, now time.Time) {
			if s.High != nil {
				k := tempCol(*s.High, s.Unit)
				c.line(arrowUp, deg(*s.High)+"°", 0, k, k)
			}
			if s.Low != nil {
				k := tempCol(*s.Low, s.Unit)
				c.line(arrowDown, deg(*s.Low)+"°", 6, k, k)
			}
		}})
	}
	if s.Apparent != nil && s.Temp != nil && math.Abs(toC(*s.Apparent, s.Unit)-toC(*s.Temp, s.Unit)) >= 3 {
		ps = append(ps, page{3, func(c *canvas, now time.Time) {
			c.line(0, "FEELS", 0, col{}, col{170, 170, 190})
			c.line(0, deg(*s.Apparent)+"°", 6, col{}, tempCol(*s.Apparent, s.Unit))
		}})
	}
	raining := precipCond(w.cond)
	if (s.Chance != nil && *s.Chance >= 10) || (s.Precip != nil && *s.Precip >= 0.1) || raining {
		ps = append(ps, page{3, func(c *canvas, now time.Time) {
			blue, pale := col{90, 160, 255}, col{170, 200, 255}
			switch {
			case s.Chance != nil:
				c.line(drop, fmt.Sprintf("%d%%", int(math.Round(*s.Chance))), 0, blue, blue)
				if s.Precip != nil && *s.Precip > 0 {
					c.line(0, amount(*s.Precip, s.PUnit), 6, col{}, pale)
				}
			case s.Precip != nil:
				c.line(drop, amount(*s.Precip, s.PUnit), 0, blue, blue)
			default:
				word := "RAIN"
				switch w.cond {
				case "snow":
					word = "SNOW"
				case "hail":
					word = "HAIL"
				case "sleet":
					word = "SLEET"
				}
				c.line(drop, word, 0, blue, blue)
			}
		}})
	}
	if s.Wind != nil {
		ps = append(ps, page{3, func(c *canvas, now time.Time) {
			k := col{170, 220, 255}
			switch {
			case w.windKmh >= 50:
				k = col{255, 110, 70}
			case w.windKmh >= 30:
				k = col{255, 210, 90}
			case w.windKmh >= 12:
				k = col{200, 255, 220}
			}
			if w.windKmh < 2 {
				c.line(0, "CALM", 3, col{}, k)
				return
			}
			v := deg(*s.Wind)
			if s.Bearing != nil {
				v += " " + compass[int(math.Round(math.Mod(*s.Bearing+360, 360)/45))%8]
			}
			c.line(0, v, 0, col{}, k)
			u := strings.ToUpper(s.WindUnit)
			if u == "" {
				u = "KM/H"
			}
			c.line(0, u, 6, col{}, scalec(k, 0.6))
		}})
	}
	if s.UV != nil && *s.UV >= 1 {
		ps = append(ps, page{3, func(c *canvas, now time.Time) {
			word, k := uvLevel(*s.UV)
			c.line(0, "UV "+deg(*s.UV), 0, col{}, k)
			c.line(0, word, 6, col{}, scalec(k, 0.7))
		}})
	}
	ps = append(ps, page{3, func(c *canvas, now time.Time) {
		at, rise, ok := sunEvent(now, w.lat, w.lon)
		if !ok {
			return
		}
		at = at.In(w.loc)
		label, k := "SUNSET", col{255, 140, 60}
		if rise {
			label, k = "SUNRISE", col{255, 200, 90}
		}
		h := at.Hour()
		hh := fmt.Sprintf("%02d", h)
		if w.spec.Hour12 {
			h %= 12
			if h == 0 {
				h = 12
			}
			hh = fmt.Sprintf("%d", h)
		}
		c.line(0, label, 0, col{}, k)
		c.line(0, fmt.Sprintf("%s:%02d", hh, at.Minute()), 6, col{}, scalec(k, 0.7))
	}})
	return ps
}

const slide = 0.35 // seconds for one page to slide out and the next in

// drawStrip draws the info pages under the sky, sliding from one to the next.
func (w *Weather) drawStrip(c *canvas, now time.Time) {
	if len(w.pages) == 0 {
		return
	}
	total := 0.0
	for _, p := range w.pages {
		total += p.dwell
	}
	el := math.Mod(now.Sub(w.start).Seconds(), total)
	i := 0
	for el >= w.pages[i].dwell {
		el -= w.pages[i].dwell
		i++
	}
	off := 0.0
	if len(w.pages) > 1 {
		if left := w.pages[i].dwell - el; left < slide {
			off = ease(1 - left/slide)
		}
	}
	shift := int(math.Round(off * float64(stripRows)))
	w.blitPage(c, i, now, -shift)
	if shift > 0 {
		w.blitPage(c, (i+1)%len(w.pages), now, stripRows-shift)
	}
}

func ease(f float64) float64 { return f * f * (3 - 2*f) }

func (w *Weather) blitPage(c *canvas, i int, now time.Time, dy int) {
	var p canvas
	w.pages[i].draw(&p, now)
	for y := 0; y < stripRows; y++ {
		ty := stripY + y + dy
		if ty < stripY || ty >= panel.Size {
			continue
		}
		for x := 0; x < panel.Size; x++ {
			if p[y][x] != (col{}) {
				c[ty][x] = p[y][x]
			}
		}
	}
}
