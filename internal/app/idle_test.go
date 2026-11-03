package app

import (
	"fmt"
	"testing"
	"time"

	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestBackgroundRefreshKeepsHiddenPagesCurrentWithoutRedrawing(t *testing.T) {
	for _, page := range []string{"streamers", "logs", "recordings", "settings"} {
		t.Run(page, func(t *testing.T) {
			a := accountFixture(t)
			a.page = page
			a.refresh()
			a.log.Write("WARN", "fixture", "Synthetic warning")
			if changed := a.refreshVisible(); changed != (page == "logs") {
				t.Fatal("a log entry repainted an unrelated page or failed to repaint Logs")
			}
			if rows := a.logRows(); len(rows) != 1 || rows[0].Message != "Synthetic warning" {
				t.Fatal("hidden logs did not receive the new entry")
			}
			if err := a.store.Update(func(data *state.Data) error {
				data.Recordings = append(data.Recordings, state.Recording{ID: "fixture", Path: "fixture.mkv"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if changed := a.refreshVisible(); changed != (page == "recordings") {
				t.Fatal("history repainted an unrelated page or failed to repaint Recordings")
			}
			if len(a.recordingRows()) != 1 {
				t.Fatal("hidden recording history was not updated")
			}
			if err := a.store.Update(func(data *state.Data) error {
				data.Streamers = append(data.Streamers, state.Streamer{ID: "fixture"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if a.refreshVisible() {
				t.Fatal("an unrelated data revision forced a repaint")
			}
		})
	}
}

func TestHiddenTablesReleaseObsoleteSnapshotsAndMenus(t *testing.T) {
	a := accountFixture(t)
	a.log.Write("INFO", "fixture", "Synthetic event")
	if err := a.store.Update(func(data *state.Data) error {
		data.Recordings = []state.Recording{{ID: "fixture", Path: "fixture.mkv", StartedAt: time.Now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.refresh()
	a.page = "logs"
	tt := ui.NewTester(a.view, 1050, 650)
	a.page = "recordings"
	tt.Frame()
	a.page = "settings"
	tt.Frame()
	if !a.logRowsCache.valid || !a.recordRows.valid {
		t.Fatal("the hidden table snapshots were not cached")
	}
	a.log.ClearView()
	if err := a.store.Update(func(data *state.Data) error { data.Recordings = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if a.refreshVisible() {
		t.Fatal("clearing hidden tables repainted Settings")
	}
	if a.logRowsCache.source != nil || a.logRowsCache.rows != nil || a.recordRows.source != nil || a.recordRows.rows != nil {
		t.Fatal("obsolete table snapshots remain retained")
	}
	if a.recordTree.valid || a.recordTree.records != nil || a.recordTree.groups != nil {
		t.Fatal("a hidden recording tree retained obsolete paths")
	}
	if a.logList.Key != nil || a.logList.CellMenu != nil || a.recordList.List.Key != nil || a.recordList.List.CellMenu != nil {
		t.Fatal("a hidden row callback retained obsolete entries")
	}
	a.page = "logs"
	tt.Frame()
	if tt.HasText("Synthetic event") {
		t.Fatal("a cleared log returned after navigation")
	}
}

func TestOverscanClocksDoNotKeepRequestingFrames(t *testing.T) {
	a := accountFixture(t)
	for i := range 30 {
		a.snapshot.Rows = append(a.snapshot.Rows, monitor.Row{Streamer: state.Streamer{ID: fmt.Sprintf("fixture_%02d", i)}, Status: "Offline"})
	}
	frames := 0
	tt := ui.NewTester(func(c *ui.Context) { frames++; a.view(c) }, 1050, 650)
	_, last := a.streamList.Visible()
	if last < 0 || last+1 >= len(a.snapshot.Rows) {
		t.Fatal("no row immediately beyond the viewport")
	}
	a.snapshot.Rows[last+1].StartedAt = tt.Now().Add(-time.Minute)
	tt.Frame()
	before := frames
	tt.Advance(3 * time.Second)
	if frames != before {
		t.Fatal("an off-screen duration kept the table redrawing")
	}
}

func TestLongRecordingClocksRefreshOnlyWhenDisplayedMinutesChange(t *testing.T) {
	for _, page := range []string{"streamers", "recordings"} {
		t.Run(page, func(t *testing.T) {
			a := accountFixture(t)
			a.page = page
			started := time.Now().Add(-2*time.Hour - 10*time.Second)
			a.snapshot.Rows = []monitor.Row{{Streamer: state.Streamer{ID: "fixture"}, Status: "Recording", StartedAt: started}}
			a.data.Recordings = []state.Recording{{ID: "fixture", StartedAt: started}}
			frames := 0
			tt := ui.NewTester(func(c *ui.Context) { frames++; a.view(c) }, 1050, 650)
			before := frames
			tt.Advance(3 * time.Second)
			if frames != before {
				t.Fatal("a minute-only duration requested unnecessary frames")
			}
			tt.Advance(50 * time.Second)
			if frames == before || !tt.HasText("2h 01m") {
				t.Fatal("the visible minute did not advance")
			}
		})
	}
}

func TestLongCheckCountdownSleepsUntilItsDisplayedMinuteChanges(t *testing.T) {
	a := accountFixture(t)
	a.snapshot.Rows = []monitor.Row{{Streamer: state.Streamer{ID: "fixture"}, Status: "Offline", NextCheck: time.Now().Add(time.Hour + 30*time.Second)}}
	frames := 0
	tt := ui.NewTester(func(c *ui.Context) { frames++; a.view(c) }, 1050, 650)
	before := frames
	tt.Advance(3 * time.Second)
	if frames != before {
		t.Fatal("a minute-only countdown kept redrawing")
	}
	tt.Advance(28 * time.Second)
	if frames == before || !tt.HasText("In 59m 59s") {
		t.Fatal("the countdown did not refresh when seconds became visible")
	}
}

func TestBackgroundListenerStatusStillRefreshesEveryPage(t *testing.T) {
	a := accountFixture(t)
	a.engine = monitor.New(a.store, a.log)
	t.Cleanup(a.engine.Close)
	a.refresh()
	for _, page := range []string{"settings", "streamers", "recordings", "logs"} {
		a.page = page
		cfg := a.store.Config()
		cfg.NativeListening = !cfg.NativeListening
		if err := a.engine.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
		if !a.refreshVisible() {
			t.Fatal("a changed listener state did not repaint the sidebar")
		}
	}
}
