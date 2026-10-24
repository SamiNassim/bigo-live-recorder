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
