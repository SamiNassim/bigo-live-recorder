package app

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestDefaultSortAndLogOrder(t *testing.T) {
	now := time.Now()
	rows := []monitor.Row{{Streamer: state.Streamer{ID: "old", LastRecordedAt: now.Add(-time.Hour)}}, {Streamer: state.Streamer{ID: "new", LastRecordedAt: now}}}
	if got := filteredRows(rows, "", "All statuses", "All priorities", "Last recorded"); got[0].Streamer.ID != "new" {
		t.Fatal(got)
	}
	entries := []logging.Entry{{Sequence: 2, Time: now, Level: "WARN"}, {Sequence: 1, Time: now.Add(-time.Minute), Level: "WARN"}, {Sequence: 3, Time: now, Level: "INFO"}}
	for _, level := range []string{"All levels", "WARN"} {
		got := filteredLogs(entries, "", level)
		if got[0].Sequence != map[string]uint64{"All levels": 3, "WARN": 2}[level] {
			t.Fatal(got)
		}
	}
}

func TestNativeViewsAndNavigation(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log, err := logging.Open(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	a := newApplication(store, log)
	defer a.cancel()
	a.preview = true
	a.seedPreview()
	a.cfg.Theme = "dark"
	tt := ui.NewTester(a.view, 1400, 900)
	if !tt.HasText("Luna") {
		t.Fatalf("streamers missing: %q", tt.Texts())
	}
	a.order = "Name"
	tt.Frame()
	for _, page := range []string{"Logs", "Recordings", "Settings", "Streamers"} {
		if err = tt.Click(page); err != nil {
			t.Fatal(err)
		}
	}
	if a.order != "Name" {
		t.Fatal("sort reset across navigation")
	}
	a.page = "settings"
	tt.Frame()
	if tt.HasText("Captured native session (JSON)") || tt.HasText("Paste session") || tt.HasText("Import JSON…") {
		t.Fatal("obsolete native session input is still visible")
	}
	if !tt.HasText("BIGO account") || !tt.HasText("Paste cookie") {
		t.Fatal("single BIGO cookie input is missing")
	}
	a.page = "streamers"
	tt.Frame()
	if err = tt.Click("+  Add streamer"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("Add a streamer") {
		t.Fatal("add dialog missing")
	}
	if err = tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	tt.SetSize(1050, 650)
	tt.Frame()
	if r, ok := tt.Find("+  Add streamer"); !ok || r.X+r.W > 1050 || r.Y+r.H > 650 {
		t.Fatalf("compact header clipped: %+v", r)
	}
	tt.SetSize(1400, 900)
	if dir := os.Getenv("BIGO_UI_SNAPSHOTS"); dir != "" {
		if err = os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"dark", "light"} {
			a.cfg.Theme = mode
			for _, page := range []string{"streamers", "logs", "settings"} {
				a.page = page
				tt.Frame()
				f, e := os.Create(filepath.Join(dir, page+"-"+mode+".png"))
				if e != nil {
					t.Fatal(e)
				}
				e = png.Encode(f, tt.Image())
				f.Close()
				if e != nil {
					t.Fatal(e)
				}
			}
		}
	}
}

func TestFeedbackToastsExpireAcrossNavigation(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log, err := logging.Open(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	a := newApplication(store, log)
	defer a.cancel()
	a.preview = true
	a.toasts = []string{"Settings saved", "Streamer removed"}
	tt := ui.NewTester(a.view, 1400, 900)
	if !tt.HasText("Settings saved") || !tt.HasText("Streamer removed") {
		t.Fatal(tt.Texts())
	}
	a.page = "logs"
	tt.Frame()
	tt.Advance(10 * time.Second)
	if tt.HasText("Settings saved") || tt.HasText("Streamer removed") {
		t.Fatal("feedback did not expire")
	}
	a.page = "settings"
	tt.Frame()
	if a.settingsScroll.MaxY <= 0 {
		t.Fatal("settings should be scrollable at this size")
	}
	a.settingsScroll.Y = a.settingsScroll.MaxY
	tt.Frame()
	if tt.HasText("HttpOnly") {
		t.Fatal("technical cookie description remains")
	}
	if tt.HasText("Choose FFmpeg…") || tt.HasText("FFmpeg executable") {
		t.Fatal("manual FFmpeg path input remains")
	}
	a.ffmpegOpen = true
	tt.Frame()
	if !tt.HasText("Get ready to record") || !tt.HasText("Download and install") {
		t.Fatal("FFmpeg setup prompt is missing")
	}
	if err := tt.Click("Later"); err != nil {
		t.Fatal(err)
	}
	if a.ffmpegOpen {
		t.Fatal("setup prompt could not be dismissed")
	}
}
