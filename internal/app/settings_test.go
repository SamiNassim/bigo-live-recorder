package app

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestUnsavedSettingsTracksEditsAndReverts(t *testing.T) {
	a := accountFixture(t)
	saved := a.store.Config()
	for _, test := range []struct {
		name string
		edit func()
	}{
		{"cookie", func() { a.cfg.Cookie = "synthetic-cookie" }},
		{"recording folder", func() { a.cfg.RecordingPath += "-new" }},
		{"notifications", func() { a.cfg.Notifications = !saved.Notifications }},
		{"output options", func() { a.cfg.FFmpegArgs += " -an" }},
		{"output format", func() { a.cfg.OutputExtension = "ts" }},
		{"normal interval", func() { a.normalSeconds++ }},
		{"low-priority interval", func() { a.coldSeconds++ }},
		{"request delay", func() { a.delaySeconds++ }},
		{"fractional input", func() { a.normalSeconds += 0.5 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if a.unsavedSettings() {
				t.Fatal("unchanged settings are marked unsaved")
			}
			test.edit()
			if !a.unsavedSettings() {
				t.Fatal("edited setting is not marked unsaved")
			}
			if a.store.Config() != saved {
				t.Fatal("editing settings changed the saved configuration")
			}
			a.cfg = saved
			a.loadNumbers()
			if a.unsavedSettings() {
				t.Fatal("reverting the edit did not clear the warning")
			}
		})
	}
}

func TestUnsavedSettingsIgnoresAutomaticPersistence(t *testing.T) {
	a := accountFixture(t)
	if err := a.store.SaveWindow(state.Window{Width: 1280, Height: 800}); err != nil {
		t.Fatal(err)
	}
	if a.unsavedSettings() {
		t.Fatal("automatic window persistence marked settings unsaved")
	}
	cfg := a.store.Config()
	cfg.FFmpegPath = filepath.Join(t.TempDir(), "ffmpeg")
	if err := a.store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.cfg.FFmpegPath = cfg.FFmpegPath
	if a.unsavedSettings() {
		t.Fatal("the automatically saved FFmpeg path marked settings unsaved")
	}
	a.cfg.Notifications = !cfg.Notifications
	if !a.unsavedSettings() {
		t.Fatal("automatic persistence hid another unsaved setting")
	}
}

func TestSettingsWarningStaysInHeaderAcrossNavigationAndScroll(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		t.Run(theme, func(t *testing.T) {
			a := accountFixture(t)
			a.cfg.Theme = theme
			if err := a.store.SaveConfig(a.cfg); err != nil {
				t.Fatal(err)
			}
			a.page = "settings"
			tt := ui.NewTester(a.view, 1050, 650)
			if tt.HasText("Unsaved settings") {
				t.Fatal("warning is visible before editing")
			}
			if err := tt.Click("BIGO web cookie"); err != nil {
				t.Fatal(err)
			}
			tt.Type("synthetic-cookie")
			checkHeader := func() {
				t.Helper()
				warning, ok := tt.Find("Unsaved settings")
				if !ok || warning.Y < 0 || warning.Y+warning.H > 150 {
					t.Fatal("unsaved warning is missing or outside the fixed header", warning)
				}
				button, ok := tt.Find("Save settings")
				if !ok || button.X+button.W > 1050 || warning.X+warning.W >= button.X {
					t.Fatal("warning overlaps or clips the save button", warning, button)
				}
				if _, ok := tt.Find("Unsaved settings warning"); !ok {
					t.Fatal("warning icon is missing")
				}
			}
			checkHeader()
			a.settingsScroll.Y = a.settingsScroll.MaxY
			tt.Frame()
			checkHeader()
			if err := tt.Click("Logs"); err != nil {
				t.Fatal(err)
			}
			if tt.HasText("Unsaved settings") {
				t.Fatal("settings warning leaked into another page")
			}
			if err := tt.Click("Settings"); err != nil {
				t.Fatal(err)
			}
			checkHeader()
			if dir := os.Getenv("BIGO_UI_SNAPSHOTS"); dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				f, err := os.Create(filepath.Join(dir, "settings-unsaved-"+theme+".png"))
				if err != nil {
					t.Fatal(err)
				}
				err = png.Encode(f, tt.Image())
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
		})
	}
}

func TestSettingsWarningClearsOnlyAfterSuccessfulSave(t *testing.T) {
	a := accountFixture(t)
	cfg := a.store.Config()
	cfg.NativeListening = false
	cfg.FFmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
	if err := a.store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.cfg = cfg
	a.loadNumbers()
	a.engine = monitor.New(a.store, a.log)
	t.Cleanup(a.engine.Close)
	a.page = "settings"
	a.normalSeconds += 1.5
	a.cfg.Notifications = !cfg.Notifications
	tt := ui.NewTester(a.view, 1050, 650)
	if !tt.HasText("Unsaved settings") {
		t.Fatal("warning is missing before saving")
	}
	if err := a.saveSettings(a.settingsConfig()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.unsavedSettings() || tt.HasText("Unsaved settings") {
		t.Fatal("successful save did not clear the warning")
	}
	if got := a.store.Config(); got.PollSeconds != cfg.PollSeconds+1 || got.Notifications == cfg.Notifications {
		t.Fatal("edited settings were not saved")
	}
	a.normalSeconds = 1
	if err := a.saveSettings(a.settingsConfig()); err == nil {
		t.Fatal("invalid settings were accepted")
	}
	tt.Frame()
	if !a.unsavedSettings() || !tt.HasText("Unsaved settings") {
		t.Fatal("a failed save cleared the warning")
	}
}
