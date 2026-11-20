package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

func (a *application) streamerStartup(c *ui.Context) {
	cfg := a.store.Config()
	checks, listener := a.snapshot.InitialChecks, a.snapshot.Listener
	if !cfg.NativeListening || checks.Total == 0 || strings.TrimSpace(cfg.Cookie) == "" && !cfg.PollingBackup {
		a.startupProgressShown = false
		return
	}
	attention := 0
	attentionStatus := "Needs attention"
	for _, row := range a.snapshot.Rows {
		if row.Startup && !row.Streamer.Paused && (row.Status == "Needs attention" || row.Status == "Setup needed") {
			attention++
			if row.Status == "Setup needed" {
				attentionStatus = row.Status
			}
		}
	}
	// Readiness ends this launch's progress permanently. Added or resumed
	// monitors and later listener reconnects use their existing indicators.
	if !a.startupReadyUntil.IsZero() && !c.Now().Before(a.startupReadyUntil) {
		a.startupProgressShown = false
		return
	}
	checking := a.startupReadyUntil.IsZero() && checks.Completed < checks.Total
	finishing := a.startupReadyUntil.IsZero() && !checking && attention == 0 &&
		(!listener.Connected || listener.ReadyTargets < checks.Total) && (listener.State == "connecting" || listener.State == "idle")
	warning := a.startupReadyUntil.IsZero() && !checking && !finishing && (attention > 0 || !listener.Connected || listener.ReadyTargets < checks.Total)
	t := c.Theme()
	title, detail, color := "Checking streamers…", "Initial checks run one at a time.", t.Accent
	if listener.Connected {
		detail = "Native listener connected · Initial checks run one at a time."
	}
	switch {
	case checking, finishing:
		a.startupProgressShown = true
		a.startupReadyUntil = time.Time{}
		if finishing {
			title = "Finishing listener setup…"
			detail = "Initial checks complete · Connecting to live notifications."
		}
	case warning:
		a.startupReadyUntil = time.Time{}
		title, color = "Initial checks complete", t.Warning
		if attention == 1 {
			title += " · 1 streamer needs attention"
		} else if attention > 1 {
			title += fmt.Sprintf(" · %d streamers need attention", attention)
		}
		detail = "Native listener reconnecting · See Logs for details."
		if listener.Connected {
			detail = fmt.Sprintf("Native listener connected · %d / %d subscriptions ready", listener.ReadyTargets, listener.TotalTargets)
		} else if listener.State == "error" {
			detail = "Native listener unavailable · See Logs for details."
		}
	default:
		if !a.startupProgressShown {
			return
		}
		if a.startupReadyUntil.IsZero() {
			a.startupReadyUntil = c.Now().Add(3 * time.Second)
		}
		c.After(a.startupReadyUntil.Sub(c.Now()))
		title, detail, color = "Monitoring ready", "Initial checks complete · Listening for live notifications.", t.Success
	}
	ui.Column(c).Label("Monitoring startup").FillWidth().Shrink(0).Padding(12, 16).Gap(10).Background(t.Surface).Border(1, color.Alpha(0.35)).Radius(8).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(12).Children(func() {
			if checking || finishing {
				ui.Spinner(c).Label("Checking monitored streamers").Size(16, 16).TextColor(color)
			} else if warning {
				ui.Icon(c, warningIcon).Size(16, 16).TextColor(color)
			} else {
				ui.Text(c, "✓").TextColor(color)
			}
			ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
				ui.Text(c, title).FontSize(12).FontWeight(600).TextColor(color)
				ui.Text(c, detail).FontSize(11).TextColor(t.TextMuted).MaxLines(2).Tooltip(listener.Message)
			})
			if checking || finishing {
				ui.Text(c, fmt.Sprintf("%d / %d checked", checks.Completed, checks.Total)).FontSize(11).TextColor(t.TextMuted)
			} else if warning {
				if attention > 0 {
					if ui.Button(c, "View streamers").Height(30).Clicked() {
						a.query, a.status, a.priority = "", attentionStatus, "All priorities"
					}
				} else if ui.Button(c, "View logs").Height(30).Clicked() {
					a.page = "logs"
				}
			}
		})
		if checking || finishing {
			value := float64(checks.Completed) / float64(checks.Total)
			if finishing {
				value = -1
			}
			ui.Progress(c, value).Label("Initial streamer checks").Height(3).FillWidth()
		}
	})
}
