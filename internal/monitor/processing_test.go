package monitor

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/state"
)

func TestProcessingNeverBlocksNextRecordingOrShutdown(t *testing.T) {
	store, log := fixture(t, false, false)
	cfg := store.Config()
	cfg.PostProcess = true
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int64
	e := newEngine(store, log, &fakeClient{}, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return testRecording(&starts), nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	processing := make(chan struct{}, 1)
	e.mu.Lock()
	e.process = func(ctx context.Context, _ string, cfg state.Config) (string, error) {
		processing <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	e.items["test"].handle.Stop()
	e.mu.Unlock()
	e.wakeUp()
	select {
	case <-processing:
	case <-time.After(3 * time.Second):
		t.Fatal("processing did not start")
	}
	waitFor(t, func() bool { return e.Snapshot().Recording == 0 })
	e.mu.Lock()
	e.items["test"].row.NextCheck = time.Now().Add(-time.Hour)
	e.mu.Unlock()
	e.wakeUp()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if starts.Load() != 2 {
		t.Fatal("post-processing blocked the next recording")
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for post-processing")
	}
}

func TestProcessedCopyHistoryIsUpdatedButNeverRestoredAfterClearing(t *testing.T) {
	for _, clear := range []bool{false, true} {
		store, log := fixture(t, true, false)
		e := newEngine(store, log, &fakeClient{}, nil)
		if err := store.Update(func(d *state.Data) error {
			d.Recordings = []state.Recording{{ID: "done", Path: "original.mkv", EndedAt: time.Now()}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		started, release := make(chan struct{}), make(chan struct{})
		e.mu.Lock()
		e.process = func(ctx context.Context, _ string, _ state.Config) (string, error) {
			close(started)
			select {
			case <-release:
				return "original.processed.mp4", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		e.processCopy("test", "original.mkv", store.Config())
		e.mu.Unlock()
		<-started
		if clear {
			if n, err := store.ClearRecordingHistory(); err != nil || n != 1 {
				e.Close()
				t.Fatal(n, err)
			}
		}
		close(release)
		waitFor(t, func() bool {
			return slices.ContainsFunc(log.Entries(), func(entry logging.Entry) bool { return entry.Message == "Processed copy ready; original MKV kept" })
		})
		e.Close()
		rows := store.Data().Recordings
		if clear && len(rows) != 0 {
			t.Fatal("cleared recording reappeared after processing")
		}
		if !clear && (len(rows) != 1 || rows[0].Path != "original.mkv" || rows[0].ProcessedPath != "original.processed.mp4") {
			t.Fatal("history lost original or processed path", rows)
		}
	}
}

func TestInvalidOutputOptionsCannotBeSavedWhenProcessingEnabled(t *testing.T) {
	store, log := fixture(t, true, false)
	e := newEngine(store, log, &fakeClient{}, nil)
	defer e.Close()
	cfg := store.Config()
	cfg.PostProcess = true
	cfg.FFmpegArgs = "-i alternate.mkv -c copy"
	if err := e.SaveConfig(cfg); err == nil {
		t.Fatal("unsafe options were saved")
	}
	if store.Config().PostProcess {
		t.Fatal("failed validation changed preferences")
	}
}
