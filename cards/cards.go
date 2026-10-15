package cards

import (
	"encoding/json"
	"fmt"
	"time"

	"pixeld/panel"
)

// Badge is something the light is keeping track of that a card can make room for, e.g. a running timer.
type Badge struct {
	Text   string // short, e.g. "4:59"
	Color  RGB
	Paused bool // drawn blinking
}

// Env is what cards can draw on besides their spec.
type Env struct {
	// Badge reports the badge to show now, if any (nil: never).
	Badge func(now time.Time) (Badge, bool)
}

// Card draws a playlist item on the light.
type Card struct {
	Render func(now time.Time) (*panel.Frame, time.Time)
	// ShowsBadges: the card shows Env.Badge itself, so nothing needs drawing over it.
	ShowsBadges bool
}

// New builds a card from its JSON spec: {"type": "clock", ...}.
func New(spec json.RawMessage, env Env) (*Card, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(spec, &head); err != nil {
		return nil, err
	}
	switch head.Type {
	case "clock":
		var s ClockSpec
		if err := json.Unmarshal(spec, &s); err != nil {
			return nil, err
		}
		c, err := NewClock(s)
		if err != nil {
			return nil, err
		}
		c.SetBadge(env.Badge)
		return &Card{Render: c.Render, ShowsBadges: c.badge != nil}, nil
	}
	return nil, fmt.Errorf("unknown card type %q", head.Type)
}
