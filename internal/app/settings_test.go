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
		{"recording split interval", func() { a.splitMode = "By duration" }},
		{"recording split size", func() { a.splitMode = "By file size" }},
		{"split recording folders", func() { a.cfg.FolderPerSplitRecording = !saved.FolderPerSplitRecording }},
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

func TestRecordingSplitSettingsPresetsAndCustom(t *testing.T) {
	a := accountFixture(t)
	a.page = "settings"
	tt := ui.NewTester(a.view, 1400, 1100)
	const folderLabel = "Group split clips in a separate folder"
	if _, ok := tt.Find(folderLabel); ok {
		t.Fatal("split folder setting is visible when splitting is off")
	}
	a.splitMode = "By duration"
	tt.Frame()
	for _, choice := range []struct {
		label   string
		minutes int
	}{{"30 minutes", 30}, {"1 hour", 60}, {"Custom", 30}} {
		control, ok := tt.Find("Split recordings every")
		if !ok {
			t.Fatal("split setting is missing")
		}
		viewport, _ := tt.Find("Settings sections")
		a.settingsScroll.Y += control.Y - viewport.Y - 80
		tt.Frame()
		if err := tt.Click(a.splitChoice); err != nil {
			t.Fatal(err)
		}
		if err := tt.Click(choice.label); err != nil {
			t.Fatal(err)
		}
		_, customVisible := tt.Find("Split interval (minutes)")
		if got := a.settingsConfig().SplitMinutes; got != choice.minutes || customVisible != (choice.label == "Custom") {
			t.Fatal("wrong split preference", choice, got, customVisible)
		}
		if _, visible := tt.Find(folderLabel); visible != (choice.minutes > 0) {
			t.Fatal("split folder setting visibility does not follow splitting", choice)
		}
		if choice.label == "Custom" {
			a.splitMinutes = 45
			cfg := a.settingsConfig()
			if err := a.store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			a.cfg = a.store.Config()
			a.loadNumbers()
			if a.unsavedSettings() || a.splitChoice != "Custom" || a.splitMinutes != 45 {
				t.Fatal("custom interval did not survive saving")
			}
			tt.Frame()
		}
	}
	a.splitMode = "Off"
	tt.Frame()
	if _, visible := tt.Find(folderLabel); visible || a.settingsConfig().SplitEnabled() {
		t.Fatal("disabling splitting left a split limit or folder control active")
	}
}

func TestSplitFolderSwitchPersistsIndependentlyOfSplitting(t *testing.T) {
	a := accountFixture(t)
	a.page = "settings"
	a.splitMode = "By duration"
	a.splitChoice = "30 minutes"
	tt := ui.NewTester(a.view, 1400, 1100)
	const label = "Split recording folder"
	control, ok := tt.Find(label)
	if !ok || !a.cfg.FolderPerSplitRecording {
		t.Fatal("split folder setting is missing or disabled by default")
	}
	viewport, _ := tt.Find("Settings sections")
	a.settingsScroll.Y += control.Y - viewport.Y - 80
	tt.Frame()
	if err := tt.Click(label); err != nil {
		t.Fatal(err)
	}
	if a.cfg.FolderPerSplitRecording || !a.cfg.FolderPerStreamer || !a.unsavedSettings() {
		t.Fatal("split folder switch did not independently update draft settings")
	}
	a.splitMode = "Off"
	tt.Frame()
	if _, ok := tt.Find(label); ok {
		t.Fatal("split folder setting remained visible after disabling splitting")
	}
	if err := a.store.SaveConfig(a.settingsConfig()); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(a.store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = reopened.Config()
	a.loadNumbers()
	a.splitMode = "By duration"
	a.splitChoice = "1 hour"
	tt.Frame()
	if _, ok := tt.Find(label); !ok || a.cfg.FolderPerSplitRecording || !a.cfg.FolderPerStreamer {
		t.Fatal("split folder preference was lost while splitting was off")
	}
}

func TestFileSizeSplitUnitsAndSavedMode(t *testing.T) {
	a := accountFixture(t)
	a.page = "settings"
	a.splitMode = "By file size"
	tt := ui.NewTester(a.view, 1400, 1100)
	if _, ok := tt.Find("Split recordings every"); ok {
		t.Fatal("duration options remained visible in size mode")
	}
	control, ok := tt.Find("Split size unit")
	if !ok {
		t.Fatal("file size unit selector is missing")
	}
	viewport, _ := tt.Find("Settings sections")
	a.settingsScroll.Y += control.Y - viewport.Y - 80
	tt.Frame()
	if err := tt.Click("GB"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("MB"); err != nil {
		t.Fatal(err)
	}
	if a.splitSize != 2000 || a.settingsConfig().SplitBytes != 2*state.BytesPerGB {
		t.Fatal("changing units changed the size limit")
	}
	a.splitSize = 500
	a.cfg.FolderPerSplitRecording = false
	if err := a.store.SaveConfig(a.settingsConfig()); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(a.store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = reopened.Config()
	a.loadNumbers()
	tt.Frame()
	if a.splitMode != "By file size" || a.splitSizeUnit != "MB" || a.splitSize != 500 || a.cfg.SplitMinutes != 0 || a.unsavedSettings() {
		t.Fatal("size mode or preferred unit was not restored")
	}
	if err := tt.Click("MB"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("GB"); err != nil {
		t.Fatal(err)
	}
	if a.splitSize != 0.5 || a.settingsConfig().SplitBytes != 500*state.BytesPerMB || !a.unsavedSettings() {
		t.Fatal("MB to GB conversion lost the configured size")
	}
	a.splitSize = 1.001
	if err := a.store.SaveConfig(a.settingsConfig()); err != nil {
		t.Fatal(err)
	}
	a.cfg = a.store.Config()
	a.loadNumbers()
	tt.Frame()
	for _, unit := range []string{"MB", "GB"} {
		if err := tt.Click(a.splitSizeUnit); err != nil {
			t.Fatal(err)
		}
		if err := tt.Click(unit); err != nil {
			t.Fatal(err)
		}
	}
	if a.splitSize != 1.001 || a.unsavedSettings() {
		t.Fatal("switching units and back introduced rounding drift")
	}
	a.splitMode = "Off"
	tt.Frame()
	if _, ok := tt.Find("Split file size"); ok || a.settingsConfig().SplitEnabled() {
		t.Fatal("size control or limit remained active after turning splitting off")
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
