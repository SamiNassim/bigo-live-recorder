package app

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestOriginQualitySelectionTracksUnsavedChangesAndPollingMode(t *testing.T) {
	a := accountFixture(t)
	a.page = "settings"
	tt := ui.NewTester(a.view, 1200, 900)
	showQuality := func() {
		t.Helper()
		r, ok := tt.Find("Stream quality")
		if !ok {
			t.Fatal("stream quality control is missing")
		}
		viewport, _ := tt.Find("Settings sections")
		a.settingsScroll.Y += r.Y - viewport.Y - 80
		tt.Frame()
	}
	showQuality()
	if a.cfg.StreamQuality != "origin" {
		t.Fatal("new installations do not prefer Origin")
	}
	if err := tt.Click("Origin (native)"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Web (automatic)"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.cfg.StreamQuality != "auto" || !a.unsavedSettings() {
		t.Fatal("selecting Web did not retain an unsaved preference")
	}
	if a.store.Config().StreamQuality != "origin" {
		t.Fatal("selection changed the saved preference before saving")
	}
	if err := tt.Click("Web (automatic)"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Origin (native)"); err != nil {
		t.Fatal(err)
	}
	if a.cfg.StreamQuality != "origin" || a.unsavedSettings() {
		t.Fatal("returning to Origin did not restore the saved preference")
	}
	a.cfg.NativeListening = false
	tt.Frame()
	showQuality()
	if err := tt.Click("Origin (native)"); err != nil {
		t.Fatal(err)
	}
	if tt.HasText("Web (automatic)") || a.cfg.StreamQuality != "origin" {
		t.Fatal("native quality selection remained active in polling-only mode")
	}
	for _, label := range []string{"Processed copy format", "FFmpeg output options"} {
		if _, ok := tt.Find(label); !ok {
			t.Fatal("quality selection removed an existing processing setting", label)
		}
	}
}
