package app

import (
	"bigo-live-recorder/internal/monitor"
	"github.com/egoist/mygo/ui"
)

func nextCheckText(c *ui.Context, row monitor.Row, visible, listening bool) string {
	if row.Streamer.Paused {
		return "—"
	}
	if !row.NextCheck.IsZero() && row.Status != "Recording" {
		remaining := row.NextCheck.Sub(c.Now())
		if visible {
			refreshClock(c, remaining, true)
		}
		return "In " + duration(remaining)
	}
	if listening {
		return "Listening"
	}
	return "—"
}
