package monitor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

type fakeClient struct {
	calls         atomic.Int64
	presenceCalls atomic.Int64
	presenceAlive *atomic.Bool
	offline       bool
	entered       chan struct{}
	release       chan struct{}
}

func (c *fakeClient) Configure(string, time.Duration) {}
func (c *fakeClient) Messaging(context.Context, string) (bigo.MessagingCredentials, error) {
	return bigo.MessagingCredentials{}, bigo.ErrLoginRequired
}
func (c *fakeClient) Presence(context.Context, string) (bigo.Stream, error) {
	c.presenceCalls.Add(1)
	alive := !c.offline
	if c.presenceAlive != nil {
		alive = c.presenceAlive.Load()
	}
	if alive {
		return bigo.Stream{UID: 123, Alive: 1}, nil
	}
	return bigo.Stream{UID: 123}, nil
}
func (c *fakeClient) Check(ctx context.Context, id string) (bigo.Stream, error) {
	c.calls.Add(1)
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		select {
		case <-c.release:
		case <-ctx.Done():
			return bigo.Stream{}, ctx.Err()
		}
	}
	if c.offline {
		return bigo.Stream{UID: 123}, nil
	}
	return bigo.Stream{Alive: 1, UID: 123, RoomID: "room", HLS: "https://example.com/live.m3u8"}, nil
}

type fakeRecording struct {
	done    chan struct{}
	once    sync.Once
	started time.Time
	name    string
}

func (h *fakeRecording) File() string {
	if h.name != "" {
		return "/test/" + h.name + ".mkv"
	}
	return "/test/recording.mkv"
}
func (h *fakeRecording) Started() time.Time    { return h.started }
func (h *fakeRecording) Stop()                 { h.once.Do(func() { close(h.done) }) }
func (h *fakeRecording) Done() <-chan struct{} { return h.done }
func (h *fakeRecording) Result() recording.Result {
	<-h.done
	return recording.Result{Path: h.File(), Bytes: 1234, EndedAt: time.Now()}
}

func fixture(t *testing.T, native, backup bool) (*state.Store, *logging.Log) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := store.Config()
	c.NativeListening = native
	c.PollingBackup = backup
	if err = store.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(d *state.Data) error { d.Streamers = append(d.Streamers, state.Streamer{ID: "test"}); return nil }); err != nil {
		t.Fatal(err)
	}
	log, err := logging.Open(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return store, log
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for monitor")
}

func TestNeverChecksActiveRecordingAndFinalizesOnClose(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &fakeClient{}
	h := &fakeRecording{done: make(chan struct{}), started: time.Now()}
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	for range 5 {
		e.mu.Lock()
		e.items["test"].initial = false
		e.items["test"].pending = true
		e.items["test"].row.NextCheck = time.Now().Add(-time.Hour)
		e.mu.Unlock()
		e.wakeUp()
	}
	time.Sleep(80 * time.Millisecond)
	if client.calls.Load() != 1 {
		t.Fatalf("checked active recording %d times", client.calls.Load())
	}
	e.Close()
	if r := store.Data().Recordings; len(r) != 1 || r[0].EndedAt.IsZero() || r[0].Bytes != 1234 {
		t.Fatalf("history not finalized: %+v", r)
	}
}

func TestNativeWithoutSessionNeverFallsBack(t *testing.T) {
	store, log := fixture(t, true, false)
	client := &fakeClient{}
	e := newEngine(store, log, client, nil)
	defer e.Close()
	e.reconcile()
	if e.next() != "" || client.calls.Load() != 0 {
		t.Fatal("native missing session performed API polling")
	}
}

func TestCookieBootstrapsNativeOnlyWithoutHiddenPolling(t *testing.T) {
	store, log := fixture(t, true, false)
	cfg := store.Config()
	cfg.Cookie = "web-cookie"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{offline: true}
	e := newEngine(store, log, client, nil)
	defer e.Close()
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.items["test"] != nil && e.items["test"].initial
	})
	for range 5 {
		e.mu.Lock()
		e.items["test"].row.NextCheck = time.Now().Add(-time.Hour)
		e.mu.Unlock()
		e.wakeUp()
	}
	time.Sleep(80 * time.Millisecond)
	if client.calls.Load() != 1 || e.next() != "" {
		t.Fatalf("failed native credentials enabled polling: checks=%d", client.calls.Load())
	}
}

func TestPauseWhileCheckIsInFlight(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &fakeClient{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var starts atomic.Int64
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		starts.Add(1)
		return nil, nil
	})
	defer e.Close()
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("check not started")
	}
	if err := e.Edit("test", func(s *state.Streamer) { s.Paused = true }); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	waitFor(t, func() bool {
		snapshot := e.Snapshot()
		return len(snapshot.Rows) == 1 && snapshot.Rows[0].Status == "Monitoring paused"
	})
	if starts.Load() != 0 {
		t.Fatal("recorded a paused streamer after an in-flight check")
	}
}
