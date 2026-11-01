package monitor

import (
	"context"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/state"
)

func TestIdentifiedAlertBypassesEarlierGenericCooldown(t *testing.T) {
	store, log := fixture(t, true, false)
	cfg := store.Config()
	cfg.Cookie = "fixture-cookie"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{offline: true}
	e := newEngine(store, log, client, nil)
	defer e.Close()
	waitFor(t, func() bool { return client.calls.Load() == 1 && e.Snapshot().Rows[0].Status == "Offline" })
	events := make(chan bigolistener.Notification, 1)
	go e.listen(e.ctx, "test", events)
	events <- bigolistener.Notification{}
	waitFor(t, func() bool { return client.calls.Load() == 2 && e.Snapshot().Rows[0].Status == "Offline" })
	events <- bigolistener.Notification{Attributed: true}
	waitFor(t, func() bool { return client.calls.Load() == 3 && e.Snapshot().Rows[0].Status == "Offline" })
	for range 5 {
		events <- bigolistener.Notification{Attributed: true}
	}
	time.Sleep(80 * time.Millisecond)
	if client.calls.Load() != 3 {
		t.Fatal("duplicate identified alerts bypassed their own cooldown", client.calls.Load())
	}
}

type interruptedCheckClient struct {
	fakeClient
	checking chan struct{}
	finish   chan struct{}
}

func (c *interruptedCheckClient) Check(ctx context.Context, _ string) (bigo.Stream, error) {
	switch c.calls.Add(1) {
	case 1:
		return bigo.Stream{UID: 123}, nil
	case 2:
		close(c.checking)
		select {
		case <-c.finish:
			return bigo.Stream{UID: 123}, nil
		case <-ctx.Done():
			return bigo.Stream{}, ctx.Err()
		}
	default:
		return bigo.Stream{UID: 123, Alive: 1, RoomID: "fixture-room", HLS: "https://example.test/live.m3u8"}, nil
	}
}

func TestIdentifiedAlertDuringFallbackCheckIsNotClearedByOldResponse(t *testing.T) {
	store, log := fixture(t, true, false)
	cfg := store.Config()
	cfg.Cookie = "fixture-cookie"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	client := &interruptedCheckClient{checking: make(chan struct{}), finish: make(chan struct{})}
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return &fakeRecording{done: make(chan struct{}), started: time.Now()}, nil
	})
	defer e.Close()
	waitFor(t, func() bool { return client.calls.Load() == 1 && e.Snapshot().Rows[0].Status == "Offline" })
	events := make(chan bigolistener.Notification, 1)
	go e.listen(e.ctx, "test", events)
	events <- bigolistener.Notification{}
	select {
	case <-client.checking:
	case <-time.After(3 * time.Second):
		t.Fatal("fallback check did not start")
	}
	events <- bigolistener.Notification{Attributed: true}
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.items["test"].lastAttributed.IsZero()
	})
	close(client.finish)
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if client.calls.Load() != 3 {
		t.Fatal("new identified alert did not resolve playback exactly once", client.calls.Load())
	}
}
