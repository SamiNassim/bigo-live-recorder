package monitor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/state"
)

func testRecording(counter *atomic.Int64) *fakeRecording {
	n := counter.Add(1)
	return &fakeRecording{done: make(chan struct{}), started: time.Now(), name: fmt.Sprintf("recording-%d", n)}
}

func TestPauseRecordingKeepsMonitoringAndResumesSameRoom(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &fakeClient{}
	var starts atomic.Int64
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return testRecording(&starts), nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if err := e.SetRecordingPaused("test", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { s := e.Snapshot(); return s.Recording == 0 && s.Rows[0].Status == "Live" })
	s := e.Snapshot()
	if !s.Rows[0].Live || !s.Rows[0].Streamer.RecordingPaused || s.Rows[0].Streamer.Paused || s.RecordingPaused != 1 || s.MonitoringPaused != 0 {
		t.Fatal(s)
	}
	if client.calls.Load() != 1 || client.presenceCalls.Load() == 0 {
		t.Fatal("paused recording resolved playback instead of presence")
	}
	e.mu.Lock()
	e.items["test"].row.NextCheck = time.Now().Add(-time.Hour)
	e.mu.Unlock()
	e.wakeUp()
	waitFor(t, func() bool { return client.presenceCalls.Load() >= 2 })
	if starts.Load() != 1 || client.calls.Load() != 1 || e.Snapshot().QueueTargets != 1 {
		t.Fatal("explicit polling did not keep paused recording in the presence queue")
	}
	if err := e.SetRecordingPaused("test", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return starts.Load() == 2 && e.Snapshot().Rows[0].Status == "Recording" })
	if client.calls.Load() != 2 {
		t.Fatal("did not resolve the same live room again", client.calls.Load())
	}
	history := store.Data().Recordings
	if len(history) != 2 || history[0].EndedAt.IsZero() || history[0].Path == history[1].Path {
		t.Fatal("resume must start a new file after finalizing the paused one", history)
	}
}

func TestRecordingPausedNativeTargetStillReceivesEventsWithoutPolling(t *testing.T) {
	store, log := fixture(t, true, false)
	cfg := store.Config()
	cfg.Cookie = "fixture-cookie"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(d *state.Data) error { d.Streamers[0].RecordingPaused = true; d.Streamers[0].UID = 123; return nil }); err != nil {
		t.Fatal(err)
	}
	var alive atomic.Bool
	client := &fakeClient{presenceAlive: &alive}
	var starts atomic.Int64
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return testRecording(&starts), nil
	})
	defer e.Close()
	waitFor(t, func() bool { s := e.Snapshot(); return len(s.Rows) == 1 && s.Rows[0].Status == "Offline" })
	e.mu.Lock()
	subscribed := e.items["test"].unsubscribe != nil && e.items["test"].subUID == 123
	e.mu.Unlock()
	if !subscribed || e.Snapshot().Listener.TotalTargets != 1 {
		t.Fatal("pausing recording removed the native subscription")
	}
	alive.Store(true)
	events := make(chan bigolistener.Notification, 1)
	go e.listen(e.ctx, "test", events)
	events <- bigolistener.Notification{Attributed: true}
	waitFor(t, func() bool { s := e.Snapshot(); return s.Rows[0].Status == "Live" && s.Rows[0].Live })
	if client.calls.Load() != 0 || starts.Load() != 0 || client.presenceCalls.Load() != 2 {
		t.Fatal("paused native event retrieved playback or started recording", client.calls.Load(), starts.Load(), client.presenceCalls.Load())
	}
	for range 5 {
		e.mu.Lock()
		e.items["test"].row.NextCheck = time.Now().Add(-time.Hour)
		e.mu.Unlock()
		e.wakeUp()
		time.Sleep(10 * time.Millisecond)
	}
	if client.presenceCalls.Load() != 2 || e.next() != "" {
		t.Fatal("paused native monitoring enabled hidden API polling")
	}
	if err := e.SetRecordingPaused("test", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return starts.Load() == 1 && e.Snapshot().Rows[0].Status == "Recording" })
	if client.calls.Load() != 1 {
		t.Fatal("resume waited for another native signal")
	}
	if err := e.SetMonitoringPaused("test", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Rows[0].Status == "Monitoring paused" })
	e.mu.Lock()
	stillSubscribed := e.items["test"].unsubscribe != nil
	e.mu.Unlock()
	if stillSubscribed || e.Snapshot().Listener.TotalTargets != 0 {
		t.Fatal("monitoring pause retained the native subscription")
	}
}

func TestPauseRecordingDuringPlaybackCheckNeverStartsRecorder(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &fakeClient{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var starts atomic.Int64
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return testRecording(&starts), nil
	})
	defer e.Close()
	select {
	case <-client.entered:
	case <-time.After(time.Second):
		t.Fatal("playback check did not start")
	}
	if err := e.SetRecordingPaused("test", true); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	waitFor(t, func() bool { return e.Snapshot().Rows[0].Status == "Live" })
	if starts.Load() != 0 {
		t.Fatal("recording started after pause was requested")
	}
	if err := e.SetRecordingPaused("test", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return starts.Load() == 1 })
}

func TestPauseCancelsInFlightRecorderStartupAndCanResume(t *testing.T) {
	store, log := fixture(t, false, false)
	entered := make(chan struct{})
	var starts atomic.Int64
	e := newEngine(store, log, &fakeClient{}, func(ctx context.Context, _ state.Streamer, _ bigo.Stream, _ state.Config) (activeRecording, error) {
		if starts.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &fakeRecording{done: make(chan struct{}), started: time.Now()}, nil
	})
	defer e.Close()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("recorder startup did not begin")
	}
	if err := e.SetRecordingPaused("test", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Rows[0].Status == "Live" })
	for _, entry := range log.Entries() {
		if entry.Level == "ERROR" {
			t.Fatal("intentional pause was reported as an error", entry)
		}
	}
	if err := e.SetRecordingPaused("test", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
}

type delayedRecording struct {
	fakeRecording
	stopped  chan struct{}
	stopOnce sync.Once
}

func (h *delayedRecording) Stop() { h.stopOnce.Do(func() { close(h.stopped) }) }

func TestRapidResumeWaitsForFinalizationBeforeStartingAnotherFile(t *testing.T) {
	store, log := fixture(t, false, false)
	first := &delayedRecording{fakeRecording: fakeRecording{done: make(chan struct{}), started: time.Now(), name: "first"}, stopped: make(chan struct{})}
	var starts atomic.Int64
	e := newEngine(store, log, &fakeClient{}, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		if starts.Add(1) == 1 {
			return first, nil
		}
		return &fakeRecording{done: make(chan struct{}), started: time.Now(), name: "second"}, nil
	})
	defer e.Close()
	// Always release delayed finalization before Close, including failure paths.
	defer first.once.Do(func() { close(first.done) })
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if err := e.SetRecordingPaused("test", true); err != nil {
		t.Fatal(err)
	}
	<-first.stopped
	if err := e.SetRecordingPaused("test", false); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatal("started a second recorder before finalizing")
	}
	first.once.Do(func() { close(first.done) })
	waitFor(t, func() bool { return starts.Load() == 2 && e.Snapshot().Rows[0].Status == "Recording" })
}

func TestMonitoringPauseRemainsIndependentOfRecordingPreference(t *testing.T) {
	store, log := fixture(t, false, false)
	var starts atomic.Int64
	client := &fakeClient{}
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return testRecording(&starts), nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if err := e.SetMonitoringPaused("test", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Rows[0].Status == "Monitoring paused" })
	if e.Snapshot().Rows[0].Streamer.RecordingPaused {
		t.Fatal("monitoring pause changed recording preference")
	}
	if err := e.SetRecordingPaused("test", true); err != nil {
		t.Fatal(err)
	}
	if err := e.SetMonitoringPaused("test", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Rows[0].Status == "Live" })
	if starts.Load() != 1 || !e.Snapshot().Rows[0].Streamer.RecordingPaused {
		t.Fatal("resuming monitoring ignored the recording pause")
	}
}

func TestRecordingPauseSurvivesEngineRestart(t *testing.T) {
	store, log := fixture(t, false, false)
	if err := store.Update(func(d *state.Data) error { d.Streamers[0].RecordingPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{}
	var starts atomic.Int64
	for range 2 {
		e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
			return testRecording(&starts), nil
		})
		waitFor(t, func() bool { s := e.Snapshot(); return len(s.Rows) == 1 && s.Rows[0].Status == "Live" })
		e.Close()
	}
	if starts.Load() != 0 || client.calls.Load() != 0 || client.presenceCalls.Load() != 2 {
		t.Fatal("restart lost the recording pause")
	}
}

type blockedPresenceClient struct {
	fakeClient
	entered chan struct{}
	release chan struct{}
}

func (c *blockedPresenceClient) Presence(ctx context.Context, id string) (bigo.Stream, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
	case <-ctx.Done():
		return bigo.Stream{}, ctx.Err()
	}
	return c.fakeClient.Presence(ctx, id)
}

func TestResumeDuringPresenceCheckResolvesPlaybackBeforeStarting(t *testing.T) {
	store, log := fixture(t, false, false)
	if err := store.Update(func(d *state.Data) error { d.Streamers[0].RecordingPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	client := &blockedPresenceClient{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var starts atomic.Int64
	e := newEngine(store, log, client, func(_ context.Context, _ state.Streamer, stream bigo.Stream, _ state.Config) (activeRecording, error) {
		if stream.URL() == "" {
			t.Error("attempted to record a presence-only response")
		}
		return testRecording(&starts), nil
	})
	defer e.Close()
	select {
	case <-client.entered:
	case <-time.After(time.Second):
		t.Fatal("presence check did not start")
	}
	if err := e.SetRecordingPaused("test", false); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if client.calls.Load() != 1 || starts.Load() != 1 {
		t.Fatal("resume did not resolve playback exactly once")
	}
}
