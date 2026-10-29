package app

import (
	"time"

	"github.com/egoist/mygo/ui"
)

// Long durations show minutes, so they need no second-by-second redraws.
// Call only for visible rows; virtualization also builds off-screen overscan.
func refreshClock(c *ui.Context, value time.Duration, countdown bool) {
	if countdown && value < time.Second {
		return
	}
	delay := time.Second
	if value >= time.Hour {
		if countdown {
			delay = value%time.Minute + time.Millisecond
		} else {
			delay = time.Minute - value%time.Minute
		}
	} else if countdown {
		delay = min(delay, value)
	}
	c.After(delay)
}
