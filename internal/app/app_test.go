package app

import (
	"image/png"
	"os"
	"path/filepath"
	"slices"
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

func TestIndependentPauseAndLiveFilters(t *testing.T) {
	rows := []monitor.Row{
		{Streamer: state.Streamer{ID: "recording"}, Status: "Recording", Live: true},
		{Streamer: state.Streamer{ID: "live-paused", RecordingPaused: true}, Status: "Live", Live: true},
		{Streamer: state.Streamer{ID: "offline-paused", RecordingPaused: true}, Status: "Offline"},
		{Streamer: state.Streamer{ID: "monitoring-paused", Paused: true}, Status: "Monitoring paused", Live: true},
	}
	for filter, want := range map[string]int{"Recording paused": 2, "Monitoring paused": 1, "Live": 2, "Recording": 1, "Offline": 1} {
		if got := filteredRows(rows, "", filter, "All priorities", "Name"); len(got) != want {
			t.Fatal(filter, got)
		}
	}
}

func TestStreamerMenusAlwaysExposeRecordingPauseAndResume(t *testing.T) {
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
	tt := ui.NewTester(a.view, 1400, 900)
	for _, row := range a.snapshot.Rows {
		if err := tt.Click("Actions for " + row.Streamer.DisplayName()); err != nil {
			t.Fatal(err)
		}
		menu := tt.Menu()
		want := "Pause recording"
		if row.Streamer.RecordingPaused {
			want = "Resume recording"
		}
		if !slices.Contains(menu, want) || slices.Contains(menu, "Stop recording") {
			t.Fatal("recording controls missing or obsolete", menu)
		}
		monitoringAction := "Pause monitoring"
		if row.Streamer.Paused {
			monitoringAction = "Resume monitoring"
		}
		if !slices.Contains(menu, monitoringAction) {
			t.Fatal("independent monitoring control missing", menu)
		}
		tt.CloseMenu()
	}
	if !tt.HasText("Recording paused") || !tt.HasText("●  Live (last check)") || !tt.HasText("●  Monitoring paused") {
		t.Fatal("pause and presence states are not visible", tt.Texts())
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
	if warningIcon == nil {
		t.Fatal("account warning icon is missing")
	}
	if _, ok := tt.Find("BIGO account warning"); !ok || !tt.HasText("Make sure live notifications are enabled in your BIGO account settings.") || !tt.HasText("Another app or browser connected with this account can disconnect this app.") {
		t.Fatal("account warning icon or guidance is missing")
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
	if !tt.HasText("Choose FFmpeg…") {
		t.Fatal("FFmpeg path picker is missing")
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

func TestPollingQueueAvailability(t *testing.T) {
	a := &application{cfg: state.Defaults()}
	a.loadNumbers()
	tt := ui.NewTester(a.pollingQueue, 1000, 400)
	for _, mode := range []struct {
		name           string
		native, backup bool
		inactive       bool
	}{
		{"native only", true, false, true},
		{"native with backup", true, true, false},
		{"polling only", false, false, false},
		{"polling with saved backup", false, true, false},
		{"native only again", true, false, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			a.cfg.NativeListening, a.cfg.PollingBackup = mode.native, mode.backup
			before := []float64{a.normalSeconds, a.coldSeconds, a.delaySeconds}
			promote := a.cfg.AutoPromote
			tt.Frame()
			if got := []float64{a.normalSeconds, a.coldSeconds, a.delaySeconds}; !slices.Equal(got, before) || a.cfg.AutoPromote != promote {
				t.Fatal("changing monitoring mode reset queue settings")
			}
			for i, label := range []string{"Normal interval (seconds)", "Low-priority interval (seconds)", "Between requests (seconds)"} {
				r, ok := tt.Find(label)
				if !ok {
					t.Fatal("missing queue input", label)
				}
				increase, ok := tt.Find("Increase")
				if !ok {
					t.Fatal("missing number input controls")
				}
				// The label is above its input; all three inputs share a row.
				tt.ClickAt(r.X+20, increase.Y+increase.H/2)
				tt.Key(0, ui.KeyUp)
				want := before[i]
				if !mode.inactive {
					want++
				}
				if got := []float64{a.normalSeconds, a.coldSeconds, a.delaySeconds}[i]; got != want {
					t.Fatalf("%s = %v, want %v", label, got, want)
				}
			}
			// Tab past the final number input's two step buttons to the switch.
			for range 3 {
				tt.Key(0, ui.KeyTab)
			}
			tt.Key(0, ui.KeySpace)
			want := promote
			if !mode.inactive {
				want = !want
			}
			if a.cfg.AutoPromote != want {
				t.Fatalf("auto-promote = %v, want %v", a.cfg.AutoPromote, want)
			}
		})
	}
}
