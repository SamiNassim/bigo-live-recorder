package monitor

import (
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/state"
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSnapshotReusesRowsUntilObservableStateChanges(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfg := store.Config()
	cfg.NativeListening = false
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := bigolistener.NewHub(ctx, nil, func(string) {}, func(string) {}, nil)
	defer hub.Close()
	e := &Engine{store: store, hub: hub, items: map[string]*item{}}

	for i := range 50 {
		id := fmt.Sprintf("creator_%d", i)
		e.items[id] = &item{row: Row{Streamer: state.Streamer{ID: id}, Status: "Offline"}}
	}
	previous := e.Snapshot()
	for range 100 {
		next, changed := e.SnapshotIfChanged(previous)
		if changed || &next.Rows[0] != &previous.Rows[0] {
			t.Fatal("unchanged map order invalidated the snapshot")
		}
	}
	if n := testing.AllocsPerRun(100, func() { e.SnapshotIfChanged(previous) }); n != 0 {
		t.Fatal("idle snapshot allocates", n)
	}
	e.items["creator_0"].row.NextCheck = time.Now()
	next, changed := e.SnapshotIfChanged(previous)
	if !changed {
		t.Fatal("queue change was missed")
	}
	if &next.Rows[0] == &previous.Rows[0] {
		t.Fatal("changed rows reused mutable snapshot storage")
	}
	e.items["creator_0"].row.Streamer.RecordingPaused = true
	if _, changed := e.SnapshotIfChanged(next); !changed {
		t.Fatal("recording-pause change was missed")
	}
	previous = e.Snapshot()

	hub.Configure(false, "", "synthetic-device")
	if _, changed := e.SnapshotIfChanged(previous); !changed {
		t.Fatal("listener status change was missed")
	}

	previous = e.Snapshot()
	delete(e.items, "creator_1")
	if _, changed := e.SnapshotIfChanged(previous); !changed {
		t.Fatal("removed streamer was missed")
	}
}

func TestInitialCheckProgressCountsThisRunAndExcludesPausedMonitoring(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e := &Engine{store: store, items: map[string]*item{
		"waiting":          {row: Row{Streamer: state.Streamer{ID: "waiting"}, Status: "Waiting", Startup: true}},
		"checking":         {row: Row{Streamer: state.Streamer{ID: "checking"}, Status: "Checking", Startup: true}},
		"offline":          {initial: true, row: Row{Streamer: state.Streamer{ID: "offline"}, Status: "Offline", LastCheck: now, Startup: true}},
		"recording-paused": {initial: true, row: Row{Streamer: state.Streamer{ID: "recording-paused", RecordingPaused: true}, Status: "Live", LastCheck: now, Startup: true}},
		"retrying":         {row: Row{Streamer: state.Streamer{ID: "retrying"}, Status: "Needs attention", LastCheck: now, Startup: true}},
		"paused":           {row: Row{Streamer: state.Streamer{ID: "paused", Paused: true}, Status: "Monitoring paused", Startup: true}},
		"added":            {row: Row{Streamer: state.Streamer{ID: "added"}, Status: "Checking"}},
	}}
	previous := e.Snapshot()
	if got := previous.InitialChecks; got != (CheckProgress{Completed: 3, Total: 5}) {
		t.Fatal("incorrect initial progress", got)
	}
	// Retrying an already checked streamer must not move startup progress back.
	e.items["retrying"].row.Status = "Checking"
	next, changed := e.SnapshotIfChanged(previous)
	if !changed || next.InitialChecks != previous.InitialChecks {
		t.Fatal("a retry changed initial-check progress", next.InitialChecks)
	}
	e.items["checking"].row.LastCheck = now
	next, changed = e.SnapshotIfChanged(next)
	if !changed || next.InitialChecks != (CheckProgress{Completed: 4, Total: 5}) {
		t.Fatal("a completed off-screen check was missed", next.InitialChecks)
	}
	e.items["waiting"].row.Streamer.Paused = true
	next, changed = e.SnapshotIfChanged(next)
	if !changed || next.InitialChecks != (CheckProgress{Completed: 4, Total: 4}) {
		t.Fatal("paused monitoring remained in the startup queue", next.InitialChecks)
	}
}

func TestStartupBatchExcludesAddedAndResumedStreamers(t *testing.T) {
	store, log := fixture(t, true, false)
	if err := store.Update(func(data *state.Data) error {
		data.Streamers = append(data.Streamers, state.Streamer{ID: "paused_at_launch", Paused: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e := newEngine(store, log, &fakeClient{offline: true}, nil)
	defer e.Close()
	e.reconcile()
	if got := e.Snapshot().InitialChecks; got != (CheckProgress{Total: 1}) {
		t.Fatal("saved monitors were not captured as the launch batch", got)
	}
	if err := e.Add("added_after_launch", "Added streamer"); err != nil {
		t.Fatal(err)
	}
	if err := e.SetMonitoringPaused("paused_at_launch", false); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.Snapshot().InitialChecks; got != (CheckProgress{Total: 1}) {
		t.Fatal("adding or resuming a monitor expanded the launch batch", got)
	}
	if err := e.SetMonitoringPaused("test", true); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if err := e.SetMonitoringPaused("test", false); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.Snapshot().InitialChecks; got != (CheckProgress{}) {
		t.Fatal("resuming monitoring restarted launch progress", got)
	}
}
