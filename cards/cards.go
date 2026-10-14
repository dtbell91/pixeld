package cards

import (
	"encoding/json"
	"fmt"
	"time"

	"pixeld/panel"
)

// New builds a card renderer from its JSON spec: {"type": "clock", ...}.
func New(spec json.RawMessage) (func(time.Time) (*panel.Frame, time.Time), error) {
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
		return c.Render, nil
	}
	return nil, fmt.Errorf("unknown card type %q", head.Type)
}
