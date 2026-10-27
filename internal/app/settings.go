package app

import (
	"bigo-live-recorder/internal/ffmpeg"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo"
)

func (a *application) settingsConfig() state.Config {
	cfg := a.cfg
	cfg.PollSeconds = int(a.normalSeconds)
	cfg.ColdSeconds = int(a.coldSeconds)
	cfg.RequestDelaySeconds = int(a.delaySeconds)
	return cfg
}

func (a *application) unsavedSettings() bool {
	draft, saved := a.cfg, a.store.Config()
	// Window geometry is saved automatically, independently of the settings form.
	draft.Window = saved.Window
	return draft != saved ||
		a.normalSeconds != float64(saved.PollSeconds) ||
		a.coldSeconds != float64(saved.ColdSeconds) ||
		a.delaySeconds != float64(saved.RequestDelaySeconds)
}

func (a *application) saveSettings(cfg state.Config) error {
	if err := a.engine.SaveConfig(cfg); err != nil {
		return err
	}
	_, ffmpegErr := ffmpeg.Check(a.ctx, cfg.FFmpegPath)
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
