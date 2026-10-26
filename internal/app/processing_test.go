package app

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestProcessingSettingsReplaceMP4OnlyControls(t *testing.T) {
	a := accountFixture(t)
	a.page = "settings"
	tt := ui.NewTester(a.view, 1400, 1100)
	for _, label := range []string{"Convert finalized recordings to MP4", "Delete MKV after successful conversion"} {
		if tt.HasText(label) {
			t.Fatal("obsolete conversion control remains", label)
		}
	}
	for _, label := range []string{"Create a processed copy with FFmpeg", "Processed copy format", "FFmpeg output options"} {
		if _, ok := tt.Find(label); !ok {
			t.Fatal("processing control missing", label)
		}
	}
	show := func(label string) {
		t.Helper()
		r, ok := tt.Find(label)
		if !ok {
			t.Fatal(label)
		}
		viewport, _ := tt.Find("Settings sections")
		a.settingsScroll.Y += r.Y - viewport.Y - 80
		tt.Frame()
	}
	show("FFmpeg output options")
	before := a.cfg.FFmpegArgs
	if err := tt.Click("FFmpeg output options"); err != nil {
		t.Fatal(err)
	}
	tt.Type("not enabled")
	if a.cfg.FFmpegArgs != before {
		t.Fatal("output options changed while processing was disabled")
	}
	a.cfg.PostProcess = true
	tt.Frame()
	for _, format := range []string{"mp4", "ts", "mkv"} {
		show("Processed copy format")
		if err := tt.Click(a.cfg.OutputExtension); err != nil {
			t.Fatal(err)
		}
		if err := tt.Click(format); err != nil {
			t.Fatal(err)
		}
		if a.cfg.OutputExtension != format {
			t.Fatal("format selection was not retained", format, a.cfg.OutputExtension)
		}
	}
	a.cfg.FFmpegArgs = ""
	tt.Frame()
	show("FFmpeg output options")
	if err := tt.Click("FFmpeg output options"); err != nil {
		t.Fatal(err)
	}
	tt.Type("-c copy")
	if a.cfg.FFmpegArgs != "-c copy" {
		t.Fatal("custom output options were not editable", a.cfg.FFmpegArgs)
	}
}
