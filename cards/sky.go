package cards

import (
	"math"
	"time"
)

// Where the sun and moon are, from the light's own clock and its latitude/longitude, so the weather card's
// sky keeps moving without Home Assistant. Accurate to a few minutes, which is plenty at 32 pixels.

const rad = math.Pi / 180

type sunInfo struct {
	elev float64 // degrees above the horizon
	ha   float64 // hour angle, degrees: 0 at solar noon, negative in the morning
	ha0  float64 // hour angle of sunrise/sunset (0: never rises, 180: never sets)
	peak float64 // highest elevation today, degrees
	decl float64 // declination, degrees
}

// sunAt is the NOAA approximation of the sun's position.
func sunAt(t time.Time, lat, lon float64) sunInfo {
	t = t.UTC()
	doy := float64(t.YearDay())
	hour := float64(t.Hour()) + float64(t.Minute())/60 + float64(t.Second())/3600
	g := 2 * math.Pi / 365 * (doy - 1 + (hour-12)/24)
	eqt := 229.18 * (0.000075 + 0.001868*math.Cos(g) - 0.032077*math.Sin(g) - 0.014615*math.Cos(2*g) -
		0.040849*math.Sin(2*g))
	decl := 0.006918 - 0.399912*math.Cos(g) + 0.070257*math.Sin(g) - 0.006758*math.Cos(2*g) +
		0.000907*math.Sin(2*g) - 0.002697*math.Cos(3*g) + 0.00148*math.Sin(3*g)
	tst := hour*60 + eqt + 4*lon
	ha := tst/4 - 180
	ha = math.Mod(math.Mod(ha+180, 360)+360, 360) - 180
	phi := lat * rad
	cz := math.Sin(phi)*math.Sin(decl) + math.Cos(phi)*math.Cos(decl)*math.Cos(ha*rad)
	elev := 90 - math.Acos(clampf(cz, -1, 1))/rad
	c0 := (math.Cos(90.833*rad) - math.Sin(phi)*math.Sin(decl)) / (math.Cos(phi) * math.Cos(decl))
	ha0 := math.Acos(clampf(c0, -1, 1)) / rad
	return sunInfo{elev: elev, ha: ha, ha0: ha0, peak: 90 - math.Abs(lat-decl/rad), decl: decl / rad}
}

// sunEvent is the next sunrise (rise) or sunset after t, found by stepping the sun's elevation.
func sunEvent(t time.Time, lat, lon float64) (at time.Time, rise bool, ok bool) {
	up := sunAt(t, lat, lon).elev > -0.833
	step := 10 * time.Minute
	for i := 1; i <= 6*48; i++ {
		u := t.Add(time.Duration(i) * step)
		if now := sunAt(u, lat, lon).elev > -0.833; now != up {
			lo, hi := u.Add(-step), u // refine to the minute
			for hi.Sub(lo) > 30*time.Second {
				mid := lo.Add(hi.Sub(lo) / 2)
				if (sunAt(mid, lat, lon).elev > -0.833) == up {
					lo = mid
				} else {
					hi = mid
				}
			}
			return hi, !up, true
		}
	}
	return time.Time{}, false, false
}

// moonAge is the moon's phase, 0..1 (0 new, 0.5 full), from a known new moon and the synodic month.
func moonAge(t time.Time) float64 {
	const synodic = 29.530588853
	ref := time.Date(2000, 1, 6, 18, 14, 0, 0, time.UTC)
	days := t.Sub(ref).Hours() / 24
	return frac(days / synodic)
}

// moonHA is the moon's hour angle (degrees), roughly: it crosses the sky about 50 minutes later each day,
// transiting at noon when new and midnight when full.
func moonHA(s sunInfo, age float64) float64 {
	ha := s.ha - 360*age
	return math.Mod(math.Mod(ha+180, 360)+360, 360) - 180
}

// moonElev is the moon's elevation from its hour angle, taking its declination as 0 (it varies ±28° over a
// month; this keeps it simple and the moon still rises, climbs and sets at about the right times).
func moonElev(ha, lat float64) float64 {
	return math.Asin(clampf(math.Cos(lat*rad)*math.Cos(ha*rad), -1, 1)) / rad
}

// season is 0 summer, 1 autumn, 2 winter, 3 spring for the hemisphere of lat (meteorological seasons).
func season(t time.Time, lat float64) int {
	m := int(t.Month()) // 12,1,2 northern winter
	s := map[int]int{12: 2, 1: 2, 2: 2, 3: 3, 4: 3, 5: 3, 6: 0, 7: 0, 8: 0, 9: 1, 10: 1, 11: 1}[m]
	if lat < 0 {
		s = (s + 2) % 4
	}
	return s
}
