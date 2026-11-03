package monitor

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/state"
	"bigo-live-recorder/internal/timeline"
)

func replayEngine(t *testing.T) (*Engine, state.Broadcast) {
	t.Helper()
	store, log := fixture(t, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{store: store, log: log, ctx: ctx, cancel: cancel, replayJobs: make(map[string]bool), replaySlot: make(chan struct{}, 1)}
	t.Cleanup(e.Close)
	b := state.Broadcast{ID: "fixture-room", StreamerID: "test", Directory: t.TempDir(), StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(), Files: 1}
	j, err := timeline.Open(b.Directory, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	if err := store.Update(func(d *state.Data) error {
		d.Broadcasts = []state.Broadcast{b}
		d.Recordings = []state.Recording{{ID: "raw", BroadcastID: b.ID, Path: filepath.Join(b.Directory, "raw.mka"), EndedAt: time.Now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return e, b
}

func TestReplayJobsAreDeduplicatedAndDoNotRestoreClearedHistory(t *testing.T) {
	e, b := replayEngine(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	e.replay = func(ctx context.Context, b state.Broadcast, _ state.Config) (string, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		path := filepath.Join(b.Directory, "replay.mkv")
		return path, os.WriteFile(path, []byte("fixture-replay"), 0644)
	}
	if err := e.QueueReplay(b.ID); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := e.QueueReplay(b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ClearRecordingHistory(); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool { return !e.store.Data().Broadcasts[0].ReplayPending })
	if calls.Load() != 1 || len(e.store.Data().Recordings) != 0 {
		t.Fatal("duplicate job or cleared history restored")
	}
	if e.store.Data().Broadcasts[0].ReplayPath == "" {
		t.Fatal("replay path was not preserved")
	}
}

func TestReplayShutdownKeepsPendingJobAndDoesNotWaitForExport(t *testing.T) {
	e, b := replayEngine(t)
	started := make(chan struct{})
	e.replay = func(ctx context.Context, _ state.Broadcast, _ state.Config) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err := e.QueueReplay(b.ID); err != nil {
		t.Fatal(err)
	}
	<-started
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for replay processing")
	}
	if !e.store.Data().Broadcasts[0].ReplayPending {
		t.Fatal("interrupted job cannot resume on the next launch")
	}
}

func TestReplayCannotStartBeforeRoomAndFilesFinish(t *testing.T) {
	e, b := replayEngine(t)
	e.replay = func(context.Context, state.Broadcast, state.Config) (string, error) {
		t.Error("started replay before recording finalized")
		return "", nil
	}
	e.store.Update(func(d *state.Data) error { d.Broadcasts[0].EndedAt = time.Time{}; return nil })
	if err := e.QueueReplay(b.ID); err == nil {
		t.Fatal("accepted an open room")
	}
	e.store.Update(func(d *state.Data) error {
		d.Broadcasts[0].EndedAt = b.EndedAt
		d.Recordings[0].EndedAt = time.Time{}
		return nil
	})
	if err := e.QueueReplay(b.ID); err == nil {
		t.Fatal("accepted an active raw recording")
	}
}
