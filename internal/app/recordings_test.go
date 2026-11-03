package app

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestStreamerContextMenuCoversFullRow(t *testing.T) {
	a := accountFixture(t)
	a.seedPreview()
	tt := ui.NewTester(a.view, 1400, 900)
	left, ok := tt.Find("Streamer")
	if !ok {
		t.Fatal("streamer table missing")
	}
	for _, name := range []string{"Luna", "Nora"} {
		label, _ := tt.Find(name)
		actions, _ := tt.Find("Actions for " + name)
		for x := left.X - 5; x < actions.X+actions.W+5; x += 11 {
			tt.RightClickAt(x, label.Y+label.H/2)
			menu := tt.Menu()
			if !slices.Contains(menu, "Rename…") || !slices.Contains(menu, "Remove streamer…") {
				t.Fatalf("streamer menu missing over %s at x=%v: %v", name, x, menu)
			}
			tt.CloseMenu()
		}
		// Choose from a non-identity column and verify the target row.
		status, _ := tt.Find("Status")
		tt.RightClickAt(status.X, label.Y+label.H/2)
		if err := tt.ChooseMenuItem("Rename…"); err != nil {
			t.Fatal(err)
		}
		row := slices.IndexFunc(a.snapshot.Rows, func(r monitor.Row) bool { return r.Streamer.DisplayName() == name })
		if !a.renameOpen || a.renameID != a.snapshot.Rows[row].Streamer.ID {
			t.Fatal("row context menu targeted another streamer")
		}
		if err := tt.Click("Cancel"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordingMenusCoverFullRowAndCopyPath(t *testing.T) {
	a := accountFixture(t)
	a.preview = false
	a.page = "recordings"
	now := time.Now()
	path := filepath.Join(t.TempDir(), "demo.mkv")
	processed := filepath.Join(t.TempDir(), "demo.processed.mp4")
	a.data.Recordings = []state.Recording{
		{ID: "done", Name: "Completed Demo", Path: path, ProcessedPath: processed, StartedAt: now.Add(-time.Hour), EndedAt: now, Bytes: 1024},
		{ID: "live", Name: "Active Demo", Path: filepath.Join(t.TempDir(), "active.mkv"), StartedAt: now},
	}
	tt := ui.NewTester(a.view, 1400, 900)
	left, _ := tt.Find("Streamer")
	file, _ := tt.Find("File")
	label, _ := tt.Find("Completed Demo")
	for x := left.X - 5; x < 1360; x += 11 {
		tt.RightClickAt(x, label.Y+label.H/2)
		menu := tt.Menu()
		for _, want := range []string{"Open recording", "Show in folder", "Copy file path", "Remove from history"} {
			if !slices.Contains(menu, want) {
				t.Fatalf("recording menu missing %q at x=%v: %v", want, x, menu)
			}
		}
		tt.CloseMenu()
	}
	tt.RightClickAt(file.X, label.Y+label.H/2)
	if err := tt.ChooseMenuItem("Copy file path"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != path {
		t.Fatal("copied the wrong recording path", tt.Clipboard())
	}
	tt.RightClickAt(file.X, label.Y+label.H/2)
	if !slices.Contains(tt.Menu(), "Open processed copy") || !slices.Contains(tt.Menu(), "Show processed copy in folder") {
		t.Fatal("processed copy actions missing", tt.Menu())
	}
	if err := tt.ChooseMenuItem("Copy processed file path"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != processed {
		t.Fatal("copied the wrong processed path")
	}
	label, _ = tt.Find("Active Demo")
	tt.RightClickAt(file.X, label.Y+label.H/2)
	if slices.Contains(tt.Menu(), "Open processed copy") {
		t.Fatal("processed copy action shown without an available copy")
	}
	for _, action := range []string{"Open recording", "Remove from history"} {
		if err := tt.ChooseMenuItem(action); err == nil {
			t.Fatal("active recording action was not disabled", action)
		}
	}
	tt.CloseMenu()
}

func TestBroadcastReplayMenuReflectsProcessingAndCompletion(t *testing.T) {
	a := accountFixture(t)
	a.preview = false
	a.page = "recordings"
	now := time.Now()
	r := state.Recording{ID: "raw", Name: "Guest Example", BroadcastID: "fixture-room", Path: filepath.Join(t.TempDir(), "guest.mka"), StartedAt: now.Add(-time.Minute), EndedAt: now}
	b := state.Broadcast{ID: r.BroadcastID, StreamerID: "host.example", EndedAt: now}
	set := func(b state.Broadcast) {
		t.Helper()
		if err := a.store.Update(func(d *state.Data) error {
			d.Recordings = []state.Recording{r}
			d.Broadcasts = []state.Broadcast{b}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		a.data = a.store.Data()
	}
	set(b)
	tt := ui.NewTester(a.view, 1400, 900)
	menu := func() []string {
		t.Helper()
		tt.CloseMenu()
		tt.Frame()
		row, ok := tt.Find(r.Name)
		if !ok {
			t.Fatal("broadcast group missing")
		}
		tt.RightClickAt(row.X+row.W/2, row.Y+row.H/2)
		return tt.Menu()
	}
	if !slices.Contains(menu(), "Assemble room replay") {
		t.Fatal("missing room replay action")
	}
	b.ReplayPending = true
	set(b)
	if !slices.Contains(menu(), "Room replay is processing…") {
		t.Fatal("processing status missing")
	}
	if err := tt.ChooseMenuItem("Room replay is processing…"); err == nil {
		t.Fatal("processing status is clickable")
	}
	b.ReplayPending = false
	b.ReplayPath = filepath.Join(t.TempDir(), "replay.mkv")
	set(b)
	items := menu()
	if !slices.Contains(items, "Open room replay") || !slices.Contains(items, "Rebuild room replay") {
		t.Fatal("completed replay actions missing", items)
	}
	b.ReplayPath = ""
	b.ReplayError = "fixture processing failure"
	set(b)
	if !slices.Contains(menu(), "Retry room replay") {
		t.Fatal("failed replay cannot be retried")
	}
	b.EndedAt = time.Time{}
	set(b)
	if !slices.Contains(menu(), "Retry room replay") {
		t.Fatal("finalized capture cannot be exported before room-end confirmation")
	}
	r.EndedAt = time.Time{}
	set(b)
	menu()
	if err := tt.ChooseMenuItem("Retry room replay"); err == nil {
		t.Fatal("accepted a replay while guest media is still recording")
	}
}

func TestClearRecordingHistoryConfirmationAndAvailability(t *testing.T) {
	a := accountFixture(t)
	a.preview = false // No account cookie or engine; only local history controls run.
	a.page = "recordings"
	now := time.Now()
	tt := ui.NewTester(a.view, 1050, 650)
	assertUnavailable := func() {
		t.Helper()
		if err := tt.Click("Clear history…"); err != nil {
			t.Fatal(err)
		}
		if a.clearHistoryOpen {
			t.Fatal("history clear enabled without completed recordings")
		}
	}
	assertUnavailable()
	a.data.Recordings = []state.Recording{{ID: "live", Name: "Active Demo", StartedAt: now}}
	tt.Frame()
	assertUnavailable()
	a.data.Recordings = append(a.data.Recordings, state.Recording{ID: "done", Name: "Completed Demo", StartedAt: now.Add(-time.Hour), EndedAt: now})
	tt.Frame()
	for _, label := range []string{"Clear history…", "Open recordings folder"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 220 || r.X+r.W > 1050 {
			t.Fatal("recording controls overflow compact window", label, r)
		}
	}
	if err := tt.Click("Clear history…"); err != nil {
		t.Fatal(err)
	}
	if !a.clearHistoryOpen || !tt.HasText("Files stay on disk. Active recordings stay in the list.") {
		t.Fatal("history confirmation or file-preservation explanation missing")
	}
	if err := tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	if a.clearHistoryOpen || len(a.data.Recordings) != 2 {
		t.Fatal("cancel changed history")
	}
}
