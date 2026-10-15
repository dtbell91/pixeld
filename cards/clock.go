package cards

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // the light has no zoneinfo; its system clock is UTC

	"pixeld/panel"
)

// ClockSpec configures the clock card (JSON from Home Assistant).
type ClockSpec struct {
	TZ      string `json:"tz"`      // IANA zone, e.g. "Australia/Sydney" (default UTC)
	Hour12  bool   `json:"hour12"`  // 12-hour clock (no leading zero)
	Date    string `json:"date"`    // "weekday_day" (TUE 14, default), "day_month" (14/10), "month_day" (10/14), "none"
	Color   *RGB   `json:"color"`   // digits (default warm white)
	Seconds bool   `json:"seconds"` // seconds bar along the bottom edge
}

type Clock struct {
	spec  ClockSpec
	loc   *time.Location
	color RGB
	badge func(time.Time) (Badge, bool) // shown in place of the date
}

func NewClock(spec ClockSpec) (*Clock, error) {
	loc := time.UTC
	if spec.TZ != "" {
		l, err := time.LoadLocation(spec.TZ)
		if err != nil {
			return nil, fmt.Errorf("clock: %w", err)
		}
		loc = l
	}
	c := &Clock{spec: spec, loc: loc, color: RGB{255, 190, 110}}
	if spec.Color != nil {
		c.color = *spec.Color
	}
	return c, nil
}

// Render draws the clock for now and returns when it next changes (the next second).
func (c *Clock) Render(now time.Time) (*panel.Frame, time.Time) {
	t := now.In(c.loc)
	var f panel.Frame
	h := t.Hour()
	if c.spec.Hour12 {
		h %= 12
		if h == 0 {
			h = 12
		}
	}
	hh := fmt.Sprintf("%02d", h)
	if c.spec.Hour12 && h < 10 {
		hh = fmt.Sprintf("%d", h)
	}
	mm := fmt.Sprintf("%02d", t.Minute())
	// 6x10 digits (3x5 doubled), 1 px gaps, 2 px colon: "HH:MM" is 30 px wide.
	w := TextWidth(hh, 2) + 1 + 2 + 1 + TextWidth(mm, 2)
	x := (panel.Size - w) / 2
	y := 6
	if c.spec.Date == "none" {
		y = 11
	}
	x = Text(&f, hh, x, y, 2, c.color) - 1
	if t.Second()%2 == 0 {
		for _, dy := range []int{2, 3, 6, 7} {
			set(&f, x, y+dy, c.color)
			set(&f, x+1, y+dy, c.color)
		}
	}
	Text(&f, mm, x+3, y, 2, c.color)

	var date string
	switch c.spec.Date {
	case "none":
	case "day_month":
		date = fmt.Sprintf("%d/%d", t.Day(), int(t.Month()))
	case "month_day":
		date = fmt.Sprintf("%d/%d", int(t.Month()), t.Day())
	default:
		date = fmt.Sprintf("%s %d", strings.ToUpper(t.Weekday().String()[:3]), t.Day())
	}
	dateColor := dim(c.color, 0.55)
	if c.badge != nil {
		if b, ok := c.badge(now); ok {
			date, dateColor = b.Text, b.Color
			if b.Paused && t.Second()%2 == 1 {
				date = ""
			}
		}
	}
	if date != "" {
		Text(&f, date, (panel.Size-TextWidth(date, 1))/2, 21, 1, dateColor)
	}
	if c.spec.Seconds {
		n := t.Second() * panel.Size / 60
		for i := 0; i <= n && i < panel.Size; i++ {
			set(&f, i, 31, dim(c.color, 0.25))
		}
	}
	return &f, t.Truncate(time.Second).Add(time.Second)
}

// SetBadge shows badge (e.g. a running timer) in place of the date.
func (c *Clock) SetBadge(badge func(time.Time) (Badge, bool)) { c.badge = badge }
