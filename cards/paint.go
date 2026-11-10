package cards

// Soft-edged drawing for the weather cards: a float canvas with blending and additive light, and the shaded
// clouds, sun, moon, rain, snow, hail, lightning, wind and fog of the Home Assistant integration's weather
// animations (animations.py), ported so the light can draw them live from the weather data.

import (
	"math"

	"pixeld/panel"
)

const tau = 2 * math.Pi

type col [3]float64

type canvas [panel.Size][panel.Size]col

func (c *canvas) frame() *panel.Frame {
	var f panel.Frame
	for y := range c {
		for x := range c[y] {
			for i := 0; i < 3; i++ {
				f[y][x][i] = uint8(math.Round(clampf(c[y][x][i], 0, 255)))
			}
		}
	}
	return &f
}

func clampf(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp01(v float64) float64 { return clampf(v, 0, 1) }

func lerpc(a, b col, f float64) col {
	return col{a[0] + (b[0]-a[0])*f, a[1] + (b[1]-a[1])*f, a[2] + (b[2]-a[2])*f}
}

func scalec(a col, f float64) col { return col{a[0] * f, a[1] * f, a[2] * f} }

func rgbc(c RGB) col { return col{float64(c[0]), float64(c[1]), float64(c[2])} }

func inside(x, y int) bool { return x >= 0 && y >= 0 && x < panel.Size && y < panel.Size }

// blend mixes colour k into pixel (x, y) with opacity a.
func (c *canvas) blend(x, y int, k col, a float64) {
	if a <= 0.004 || !inside(x, y) {
		return
	}
	if a >= 1 {
		c[y][x] = k
		return
	}
	c[y][x] = lerpc(c[y][x], k, a)
}

// glow adds light k*a to pixel (x, y).
func (c *canvas) glow(x, y int, k col, a float64) {
	if a <= 0.004 || !inside(x, y) {
		return
	}
	for i := 0; i < 3; i++ {
		c[y][x][i] = math.Min(255, c[y][x][i]+k[i]*a)
	}
}

// dot is a one-pixel dot at a fractional position, shared between the four pixels around it (or, crisp, in
// the nearest one).
func (c *canvas) dot(x, y float64, k col, a float64, add, crisp bool) {
	put := c.blend
	if add {
		put = c.glow
	}
	if crisp {
		put(int(math.Round(x)), int(math.Round(y)), k, a)
		return
	}
	fx, fy := math.Floor(x), math.Floor(y)
	ax, ay := x-fx, y-fy
	put(int(fx), int(fy), k, a*(1-ax)*(1-ay))
	put(int(fx)+1, int(fy), k, a*ax*(1-ay))
	put(int(fx), int(fy)+1, k, a*(1-ax)*ay)
	put(int(fx)+1, int(fy)+1, k, a*ax*ay)
}

// mix is a small deterministic hash of two ints (animations._mix).
func mix(a, b int) uint32 {
	h := uint32(a*73856093) ^ uint32(b*19349663) ^ 0x5BD1E995
	h = (h ^ (h >> 13)) * 0x5BD1E995
	return h ^ (h >> 15)
}

// unit is a hash as a fraction in [0, 1).
func unit(a, b int) float64 { return float64(mix(a, b)&0xFFFF) / 65536 }

func frac(v float64) float64 { return v - math.Floor(v) }

// ---------- clouds ----------

type palette struct{ light, shadow col }

var (
	cloudWhite = palette{col{255, 255, 255}, col{125, 145, 190}}
	cloudGrey  = palette{col{205, 212, 226}, col{85, 92, 118}}
	cloudFar   = palette{col{140, 148, 168}, col{55, 60, 82}}
	cloudRain  = palette{col{175, 186, 210}, col{62, 72, 102}}
	cloudStorm = palette{col{118, 112, 150}, col{32, 28, 52}}
	cloudSnow  = palette{col{240, 244, 255}, col{130, 145, 180}}
	cloudFog   = palette{col{150, 156, 172}, col{75, 80, 100}}
)

func (p palette) dim(f float64) palette { return palette{scalec(p.light, f), scalec(p.shadow, f)} }

var lobes = [4][3]float64{{-1.0, 0.35, 0.62}, {1.05, 0.4, 0.58}, {-0.25, -0.22, 0.8}, {0.62, -0.02, 0.66}}

const baseX0, baseX1, baseY, baseR = -1.05, 1.05, 0.45, 0.5

var light3 = [3]float64{-0.48, -0.72, 0.5} // from the upper left, towards the viewer

// puff is a shaded, soft-edged cumulus about 3.3u wide and 2u high centred on (cx, cy). flash (0-1) lights
// it from inside, for lightning.
func (c *canvas) puff(cx, cy, u float64, pal palette, flash, alpha float64) {
	var ls [5][3]float64
	for i, l := range lobes {
		ls[i] = [3]float64{cx + l[0]*u, cy + l[1]*u, l[2] * u}
	}
	bx0, bx1, by, br := cx+baseX0*u, cx+baseX1*u, cy+baseY*u, baseR*u
	bottom, top := cy+0.85*u, cy-1.02*u
	for y := int(math.Floor(top)); y <= int(math.Ceil(bottom)); y++ {
		if y < 0 || y >= panel.Size {
			continue
		}
		for x := int(math.Floor(cx - 1.7*u)); x <= int(math.Ceil(cx+1.7*u)); x++ {
			if x < 0 || x >= panel.Size {
				continue
			}
			fx, fy := float64(x), float64(y)
			ls[4] = [3]float64{clampf(fx, bx0, bx1), by, br}
			sd, best := 9e9, -9e9
			n := [3]float64{0, 0, 1}
			for _, l := range ls {
				dx, dy := fx-l[0], fy-l[1]
				d := math.Hypot(dx, dy)
				sd = math.Min(sd, d-l[2])
				if d < l[2]+0.7 {
					q := math.Min(d/l[2], 1)
					w := math.Sqrt(1 - q*q)
					z := w*l[2] + l[1]*-0.05
					if z > best {
						best = z
						m := math.Max(d, l[2])
						n = [3]float64{dx / m, dy / m, w}
					}
				}
			}
			sd = math.Max(sd, fy-bottom)
			cov := clamp01(0.5 - sd)
			if cov <= 0 {
				continue
			}
			lam := clamp01(n[0]*light3[0] + n[1]*light3[1] + n[2]*light3[2])
			low := clamp01((fy - top) / (bottom - top))
			k := lerpc(pal.shadow, pal.light, clamp01(0.15+0.95*lam-0.35*low))
			if flash > 0 {
				k = lerpc(k, col{235, 230, 255}, flash*(0.35+0.5*low))
			}
			c.blend(x, y, k, cov*alpha)
		}
	}
}

// ---------- sun and moon ----------

type sunStyle struct {
	r, rayIn, rayLong, rayShort, halo float64
	rays                              int
	haloA                             float64 // halo strength (UV)
	core, edge, ray, haloCol          col
}

func defaultSun(r float64) sunStyle {
	return sunStyle{r: r, rayIn: 1.6, rayLong: 4.6, rayShort: 3.2, halo: 2.4, rays: 8, haloA: 0.32,
		core: col{255, 250, 190}, edge: col{255, 160, 10}, ray: col{255, 175, 30}, haloCol: col{255, 110, 0}}
}

// sun draws a gradient sun with a breathing halo and long and short rays; rot turns the rays (radians),
// pulse (0-1) breathes the halo and rays. alpha fades the whole sun (behind haze).
func (c *canvas) sun(cx, cy float64, s sunStyle, rot, pulse, alpha float64) {
	reach := s.r + math.Max(s.rayLong, s.rayShort) + 1.5 + s.halo
	for y := int(math.Floor(cy - reach)); y <= int(math.Ceil(cy+reach)); y++ {
		for x := int(math.Floor(cx - reach)); x <= int(math.Ceil(cx+reach)); x++ {
			if !inside(x, y) {
				continue
			}
			dx, dy := float64(x)-cx, float64(y)-cy
			d := math.Hypot(dx, dy)
			out := d - s.r
			if out > 0 {
				c.glow(x, y, s.haloCol, alpha*s.haloA*(0.75+0.25*pulse)*math.Exp(-out/s.halo))
				a := (math.Atan2(dy, dx) - rot) * float64(s.rays) / tau
				k := math.Round(a)
				across := math.Abs(a-k) * tau / float64(s.rays) * d
				long := s.rayShort
				if int(k)%2 == 0 {
					long = s.rayLong
				}
				length := s.r + long + 0.5*pulse
				along := clamp01(out-s.rayIn+0.5) * clamp01(length-d+0.5)
				ray := clamp01(1-across/0.75) * along * (1 - 0.45*(d-s.r)/(length-s.r))
				c.blend(x, y, s.ray, ray*alpha)
			}
			if cov := clamp01(s.r + 0.5 - d); cov > 0 {
				f := clamp01(math.Hypot(dx+s.r*0.35, dy+s.r*0.35) / (s.r * 1.25))
				c.blend(x, y, lerpc(s.core, s.edge, f), cov*alpha)
			}
		}
	}
}

// crescent is the moon of the forecast's clear-night icon: a fixed crescent with a soft glow.
func (c *canvas) crescent(cx, cy, r, pulse float64) {
	ox, oy, rc := cx+r*0.62, cy-r*0.42, r*0.86
	for y := int(math.Floor(cy - r - 3)); y <= int(math.Ceil(cy+r+3)); y++ {
		for x := int(math.Floor(cx - r - 3)); x <= int(math.Ceil(cx+r+3)); x++ {
			fx, fy := float64(x), float64(y)
			d := math.Hypot(fx-cx, fy-cy)
			sd := math.Max(d-r, rc-math.Hypot(fx-ox, fy-oy))
			if sd > 0 {
				c.glow(x, y, col{90, 80, 30}, (0.22+0.08*pulse)*math.Exp(-sd/1.6))
				continue
			}
			shade := clamp01(((fx-cx+r)*0.4 + (fy-cy+r)*0.6) / (2 * r))
			c.blend(x, y, lerpc(col{255, 245, 200}, col{225, 185, 90}, shade), clamp01(0.5-sd))
		}
	}
}

// moon draws the moon at its phase (age 0..1: new, first quarter 0.25, full 0.5, last quarter 0.75); mirror:
// lit from the other side, as seen from the southern hemisphere. alpha fades it (daytime moon).
func (c *canvas) moon(cx, cy, r, age float64, mirror bool, alpha float64) {
	k := math.Cos(tau * age) // terminator position: 1 new, 0 quarter, -1 full
	lit := 0.5 * (1 - k)
	for y := int(math.Floor(cy - r - 3)); y <= int(math.Ceil(cy+r+3)); y++ {
		for x := int(math.Floor(cx - r - 3)); x <= int(math.Ceil(cx+r+3)); x++ {
			fx, fy := float64(x), float64(y)
			d := math.Hypot(fx-cx, fy-cy)
			if d > r {
				c.glow(x, y, col{70, 70, 50}, alpha*lit*0.3*math.Exp(-(d-r)/1.5))
			}
			cov := clamp01(r + 0.5 - d)
			if cov <= 0 {
				continue
			}
			u, v := (fx-cx)/r, (fy-cy)/r
			if mirror {
				u = -u
			}
			w := math.Sqrt(math.Max(0, 1-v*v))
			edge := k * w // waxing: lit right of the terminator; waning: left of it
			var on float64
			if age < 0.5 {
				on = clamp01((u-edge)*r + 0.5)
			} else {
				on = clamp01((-edge-u)*r + 0.5)
			}
			shade := clamp01(((fx-cx+r)*0.4 + (fy-cy+r)*0.6) / (2 * r))
			body := lerpc(col{255, 248, 215}, col{215, 195, 140}, shade)
			dark := col{22, 24, 38}
			c.blend(x, y, lerpc(dark, body, on), cov*alpha)
		}
	}
}

// stars twinkle: (x, y, twinkles per period, offset) at time ph (periods).
func (c *canvas) twinkle(x, y int, rate, off, ph, alpha float64) {
	s := math.Sin(tau * (ph*rate + off))
	b := (0.25 + 0.75*math.Pow(math.Max(0, s), 2)) * alpha
	c.blend(x, y, col{255, 245, 200}, b)
	if b > 0.85 {
		for _, d := range [4][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			c.blend(x+d[0], y+d[1], col{170, 180, 255}, (b-0.85)*3)
		}
	}
}

// streak is a shooting star with its head at (hx, hy) going along (ux, uy), fade 0-1.
func (c *canvas) streak(hx, hy, ux, uy, fade float64) {
	n := math.Hypot(ux, uy)
	for k := 0; k < 9; k++ {
		f := float64(k) / 2
		k2 := col{150, 170, 255}
		if k < 2 {
			k2 = col{255, 255, 230}
		}
		c.dot(hx-ux/n*f, hy-uy/n*f, k2, fade*(1-float64(k)/9), true, false)
	}
}

// ---------- precipitation ----------

// rainOpts: drops falling from y0 to y1 between x0 and x1. t is in seconds; speed in rows per second.
type rainOpts struct {
	x0, x1, y0, y1 float64
	n              int
	speed          float64 // rows per second (spread ±25% between drops)
	length, slant  float64
	splash, wrap   bool
	seed           int
	head, tail     col
	alpha          float64
}

func (c *canvas) rain(t float64, o rainOpts) {
	if o.head == (col{}) {
		o.head, o.tail = col{210, 238, 255}, col{60, 130, 250}
	}
	if o.alpha == 0 {
		o.alpha = 1
	}
	fall := o.y1 - o.y0 + o.length
	w := o.x1 - o.x0
	for i := 0; i < o.n; i++ {
		h := mix(o.seed, i)
		near := float64(h>>8&0xFF) / 255
		sp := o.speed * (0.75 + 0.5*near)
		bright := (0.65 + 0.35*near) * o.alpha
		cycle := t*sp/fall + float64(h>>16&0xFF)/256
		y := o.y0 + frac(cycle)*fall
		// each fall starts at a new place along the width
		x := o.x0 + (float64(i)+0.5)*w/float64(o.n) + (unit(o.seed+int(math.Floor(cycle)), i)-0.5)*w/float64(o.n)
		for row := int(math.Floor(y - o.length)); row <= int(math.Ceil(y)); row++ {
			if float64(row) > o.y1 {
				continue
			}
			fr := float64(row)
			cov := clamp01(math.Min(fr+0.5, y) - math.Max(fr-0.5, y-o.length))
			g := clamp01(1 - (y-fr)/(o.length+0.5))
			rx := x + o.slant*(fr-o.y0)
			if o.wrap {
				rx = o.x0 + math.Mod(math.Mod(rx-o.x0, w)+w, w)
			}
			c.blend(int(math.Round(rx)), row, lerpc(o.tail, o.head, g), cov*(0.45+0.55*g)*bright)
		}
		if o.splash && y > o.y1 {
			s := (y - o.y1) / o.length
			sx := x + o.slant*(o.y1-o.y0)
			if o.wrap {
				sx = o.x0 + math.Mod(math.Mod(sx-o.x0, w)+w, w)
			}
			for _, side := range []float64{-1, 1} {
				c.dot(sx+side*(1+1.5*s), o.y1-1.5*math.Sin(math.Pi*s), o.head, (1-s)*bright, false, true)
			}
		}
	}
}

type snowOpts struct {
	x0, x1, y0, y1 float64
	n              int
	speed          float64 // rows per second
	drift, sway    float64 // px per second sideways (wind); sway amplitude
	seed           int
	wrap           bool
}

func (c *canvas) snow(t float64, o snowOpts) {
	fall := o.y1 - o.y0 + 2
	w := o.x1 - o.x0
	for i := 0; i < o.n; i++ {
		h := mix(o.seed, i)
		big := h>>8&1 == 1
		sp := o.speed
		if big {
			sp *= 1.6
		}
		cycle := t*sp/fall + float64(h>>16&0xFF)/256
		x := o.x0 + (float64(i)+0.5)*w/float64(o.n) + (unit(o.seed+int(math.Floor(cycle)), i)-0.5)*w/float64(o.n)
		x += o.sway*math.Sin(tau*(t*0.35*(1+float64(h>>4&3)*0.2)+float64(h>>4&0xFF)/256)) + o.drift*frac(cycle)*fall/sp
		if o.wrap {
			x = o.x0 + math.Mod(math.Mod(x-o.x0, w)+w, w)
		}
		y := o.y0 - 1 + frac(cycle)*fall
		a := clamp01(o.y1 + 1 - y)
		c.dot(x, y, col{245, 248, 255}, a, false, true)
		if big {
			for _, d := range [4][2]float64{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				c.dot(x+d[0], y+d[1], col{150, 185, 255}, 0.5*a, false, true)
			}
		}
	}
}

func (c *canvas) hail(t float64, x0, x1, y0, y1 float64, n int, rate float64, seed int) {
	const drop = 0.7
	for i := 0; i < n; i++ {
		h := mix(seed, i)
		sp := rate * (0.85 + 0.3*float64(h>>8&0xFF)/255)
		cycle := t*sp + float64(h>>16&0xFF)/256
		p := frac(cycle)
		x := x0 + (float64(i)+0.5)*(x1-x0)/float64(n) + (unit(seed+int(math.Floor(cycle)), i)-0.5)*1.5
		var y, a float64
		if p < drop {
			y = y0 + math.Pow(p/drop, 1.3)*(y1-y0)
			c.dot(x, y-1, col{90, 140, 220}, 0.5, false, true)
			a = 1
		} else {
			s := (p - drop) / (1 - drop)
			if i%2 == 1 {
				x += 2 * s
			} else {
				x -= 2 * s
			}
			y = y1 - 3*math.Sin(math.Pi*s)*(1-0.3*s)
			a = 1 - s*s
		}
		c.dot(x, y, col{235, 248, 255}, a, false, true)
		c.dot(x+1, y, col{150, 190, 240}, 0.7*a, false, true)
		c.dot(x, y+1, col{150, 190, 240}, 0.7*a, false, true)
		c.dot(x+1, y+1, col{90, 130, 200}, 0.6*a, false, true)
	}
}

// bolt is a forked lightning bolt along the points: a white core with a violet glow.
func (c *canvas) bolt(pts [][2]float64, a float64) {
	for i := 0; i+1 < len(pts); i++ {
		xa, ya, xb, yb := pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1]
		steps := max(1, int(math.Round(math.Hypot(xb-xa, yb-ya)*3)))
		for k := 0; k <= steps; k++ {
			x, y := xa+(xb-xa)*float64(k)/float64(steps), ya+(yb-ya)*float64(k)/float64(steps)
			for _, d := range [4][2]float64{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				c.glow(int(math.Round(x+d[0])), int(math.Round(y+d[1])), col{60, 45, 120}, a*0.5)
			}
			c.blend(int(math.Round(x)), int(math.Round(y)), col{255, 250, 210}, a)
		}
	}
}

// gust is a wavy streak of wind with its head at hx on row y0, blowing towards dir (+1 right, -1 left), with a
// curl at the front.
func (c *canvas) gust(hx, y0, length, wavePh, dir, alpha float64, k col) {
	edge := func(x float64) float64 { return clamp01((x+1)/3) * clamp01((panel.Size-x)/3) }
	wave := func(x float64) float64 { return y0 + 0.5*math.Sin(tau*(x/16-wavePh)) }
	for i := 0; i <= int(math.Ceil(length))+1; i++ {
		x := hx - dir*float64(i)
		f := float64(i)
		a := clamp01(1-f/length) * edge(x) * alpha
		y := wave(x)
		fy := math.Floor(y)
		xi := int(math.Round(x))
		c.blend(xi, int(fy), k, a*(1-(y-fy)))
		c.blend(xi, int(fy)+1, k, a*(y-fy))
	}
	y := wave(hx)
	for _, d := range [4][3]float64{{1, -1, 0.85}, {1.5, -2, 0.7}, {1, -3, 0.5}, {0, -3, 0.3}} {
		c.dot(hx+dir*d[0], y+d[1], k, d[2]*edge(hx+dir*d[0])*alpha, false, false)
	}
}

// fog draws wisps of mist drifting (shift in px), rows: (y, thickness).
func (c *canvas) fog(shift float64, x0, x1 int, rows [][2]float64, k col, strength float64) {
	for i, r := range rows {
		yc, th := r[0], r[1]
		s := shift
		if i%2 == 0 {
			s = -shift * 0.7
		}
		o := float64(i) * 1.7
		for y := int(math.Floor(yc - th - 1)); y <= int(math.Ceil(yc+th+1)); y++ {
			v := math.Exp(-math.Pow((float64(y)-yc)/th, 2))
			for x := x0; x <= x1; x++ {
				u := (float64(x) + s) / 16
				tex := clamp01(0.45 + 0.55*math.Sin(tau*u+o) + 0.3*math.Sin(tau*2*u+2*o))
				edge := clamp01(float64(x-x0+1)/4) * clamp01(float64(x1+1-x)/4)
				c.blend(x, y, k, v*tex*edge*(0.55+0.1*float64(i))*strength)
			}
		}
	}
}

// triangle fills the triangle (scanline, with soft-ish edges by coverage sampling).
func (c *canvas) triangle(p [3][2]float64, k col, a float64) {
	sign := func(ax, ay, bx, by, cx, cy float64) float64 { return (ax-cx)*(by-cy) - (bx-cx)*(ay-cy) }
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			n := 0
			for _, s := range [4][2]float64{{0.25, 0.25}, {0.75, 0.25}, {0.25, 0.75}, {0.75, 0.75}} {
				px, py := float64(x)+s[0]-0.5, float64(y)+s[1]-0.5
				d1 := sign(px, py, p[0][0], p[0][1], p[1][0], p[1][1])
				d2 := sign(px, py, p[1][0], p[1][1], p[2][0], p[2][1])
				d3 := sign(px, py, p[2][0], p[2][1], p[0][0], p[0][1])
				neg := d1 < 0 || d2 < 0 || d3 < 0
				pos := d1 > 0 || d2 > 0 || d3 > 0
				if !(neg && pos) {
					n++
				}
			}
			if n > 0 {
				c.blend(x, y, k, a*float64(n)/4)
			}
		}
	}
}

// warning is a pulsing warning sign centred on cx, 18 px tall.
func (c *canvas) warning(cx, top, pulse float64) {
	k := lerpc(col{255, 140, 20}, col{255, 215, 60}, pulse)
	c.triangle([3][2]float64{{cx, top}, {cx + 9.5, top + 17}, {cx - 9.5, top + 17}}, k, 1)
	for y := 0; y < 6; y++ {
		c.blend(int(cx), int(top)+6+y, col{40, 10, 0}, 1)
		c.blend(int(cx)-1, int(top)+6+y, col{40, 10, 0}, 1)
	}
	for _, y := range []int{13, 14} {
		c.blend(int(cx), int(top)+y, col{40, 10, 0}, 1)
		c.blend(int(cx)-1, int(top)+y, col{40, 10, 0}, 1)
	}
}

// text draws s in the 3x5 font (scale 1) at (x, y).
func (c *canvas) text(s string, x, y int, k col, a float64) int {
	for _, r := range s {
		g := glyph(r)
		for i := 0; i < 15; i++ {
			if g>>(14-i)&1 == 1 {
				c.blend(x+i%3, y+i/3, k, a)
			}
		}
		x += 4
	}
	return x
}
