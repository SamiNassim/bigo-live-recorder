package app

import (
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo"
)

func (a *application) settingsConfig() state.Config {
	cfg := a.cfg
	cfg.PollSeconds = int(a.normalSeconds)
	cfg.ColdSeconds = int(a.coldSeconds)
	cfg.RequestDelaySeconds = int(a.delaySeconds)
	switch a.splitChoice {
	case "30 minutes":
		cfg.SplitMinutes = 30
	case "1 hour":
		cfg.SplitMinutes = 60
	case "Custom":
		cfg.SplitMinutes = int(a.splitMinutes)
	default:
		cfg.SplitMinutes = 0
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
		(a.splitChoice == "Custom" && a.splitMinutes != float64(draft.SplitMinutes))
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
