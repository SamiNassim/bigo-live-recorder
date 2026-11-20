package app

import (
	"math"

	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo"
)

func (a *application) settingsConfig() state.Config {
	cfg := a.cfg
	cfg.PollSeconds = int(a.normalSeconds)
	cfg.ColdSeconds = int(a.coldSeconds)
	cfg.RequestDelaySeconds = int(a.delaySeconds)
	cfg.SplitMinutes, cfg.SplitBytes = 0, 0
	cfg.SplitSizeUnit = a.splitSizeUnit
	if a.splitMode == "By file size" {
		cfg.SplitBytes = int64(math.Round(a.splitSize * a.splitSizeScale()))
	} else if a.splitMode == "By duration" {
		switch a.splitChoice {
		case "30 minutes":
			cfg.SplitMinutes = 30
		case "1 hour":
			cfg.SplitMinutes = 60
		case "Custom":
			cfg.SplitMinutes = int(a.splitMinutes)
		}
	}
	return cfg
}

func (a *application) unsavedSettings() bool {
	draft, saved := a.settingsConfig(), a.store.Config()
	// Window geometry is saved automatically, independently of the settings form.
	draft.Window = saved.Window
	return draft != saved ||
		a.normalSeconds != float64(saved.PollSeconds) ||
		a.coldSeconds != float64(saved.ColdSeconds) ||
		a.delaySeconds != float64(saved.RequestDelaySeconds) ||
		(a.splitMode == "By duration" && a.splitChoice == "Custom" && a.splitMinutes != float64(draft.SplitMinutes)) ||
		(a.splitMode == "By file size" && a.splitSize != float64(draft.SplitBytes)/a.splitSizeScale())
}

func (a *application) splitSizeScale() float64 {
	if a.splitSizeUnit == "MB" {
		return float64(state.BytesPerMB)
	}
	return float64(state.BytesPerGB)
}

func (a *application) saveSettings(cfg state.Config) error {
	if err := a.engine.SaveConfig(cfg); err != nil {
		return err
	}
	ffmpegErr := a.checkFFmpegVersion(cfg.FFmpegPath)
	a.update(func() {
		a.cfg = a.store.Config()
		a.loadNumbers()
		a.ffmpeg = ffmpegErr == nil
		a.toasts = append(a.toasts, "Settings saved")
		if source := mygo.ThemeSource(cfg.Theme); source != mygo.Theme.Source() {
			mygo.Theme.SetSource(source)
		}
	})
	return nil
}
