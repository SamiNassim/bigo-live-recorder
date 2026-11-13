package monitor

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

type rollingRecording struct {
	fakeRecording
	mu      sync.Mutex
	path    string
	stamp   time.Time
	changes []recording.FileChange
	first   *recording.FileChange
}

func (h *rollingRecording) File() string       { h.mu.Lock(); defer h.mu.Unlock(); return h.path }
func (h *rollingRecording) Started() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.stamp }
func (h *rollingRecording) FirstFile() (string, time.Time) {
	if h.first != nil {
		return h.first.Path, h.first.StartedAt
	}
	return h.File(), h.Started()
}
func (h *rollingRecording) DrainFileChanges() []recording.FileChange {
	h.mu.Lock()
	defer h.mu.Unlock()
	changes := h.changes
	h.changes = nil
	return changes
}

func TestRolloverBeforeStartReturnsKeepsFirstFileInHistory(t *testing.T) {
	store, log := fixture(t, false, false)
	stamp := time.Now()
	first := recording.FileChange{Path: "/fixture/first.mkv", StartedAt: stamp}
	second := recording.FileChange{Path: "/fixture/second.mkv", StartedAt: stamp.Add(time.Minute)}
	h := &rollingRecording{fakeRecording: fakeRecording{done: make(chan struct{})}, path: second.Path, stamp: second.StartedAt, first: &first,
		changes: []recording.FileChange{second, {Path: first.Path, Result: &recording.Result{Path: first.Path, Bytes: 1111, EndedAt: second.StartedAt}}}}
	e := newEngine(store, log, &fakeClient{}, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	defer e.Close()
	waitFor(t, func() bool {
		saved := store.Data().Recordings
		return len(saved) == 2 && saved[0].Path == first.Path && saved[0].Bytes == 1111 && saved[1].Path == second.Path
	})
}
func (h *rollingRecording) Result() recording.Result {
	<-h.done
	return recording.Result{Path: h.File(), Bytes: 2222, EndedAt: time.Now()}
}

func TestRolloverPersistsIndependentFilesWithoutRestartingMonitoring(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &fakeClient{}
	started := time.Now()
	h := &rollingRecording{fakeRecording: fakeRecording{done: make(chan struct{})}, path: "/fixture/first.mkv", stamp: started}
	var starts atomic.Int32
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		starts.Add(1)
		return h, nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	stamp := time.Now().Add(time.Second)
	h.mu.Lock()
	h.path, h.stamp = "/fixture/second.mkv", stamp
	h.changes = []recording.FileChange{{Path: h.path, StartedAt: stamp}, {Path: "/fixture/first.mkv", Result: &recording.Result{Path: "/fixture/first.mkv", Bytes: 1111, EndedAt: time.Now()}}}
	h.mu.Unlock()
	e.wakeUp()
	waitFor(t, func() bool { d := store.Data(); return len(d.Recordings) == 2 && !d.Recordings[0].EndedAt.IsZero() })
	s := e.Snapshot()
	if s.Recording != 1 || s.Rows[0].Path != "/fixture/second.mkv" || s.Rows[0].StartedAt != started {
		t.Fatal("active file did not advance with the original recording clock", s)
	}
	if starts.Load() != 1 || client.calls.Load() != 1 || len(e.Events()) != 1 {
		t.Fatal("file transition restarted monitoring or sent a second live notification", starts.Load(), client.calls.Load(), len(e.Events()))
	}
	e.Close()
	d := store.Data()
	if len(d.Recordings) != 2 || d.Recordings[0].Bytes != 1111 || d.Recordings[1].Bytes != 2222 || d.Recordings[1].EndedAt.IsZero() || !d.Streamers[0].LastRecordedAt.Equal(stamp) {
		t.Fatal("rollover history was not finalized", d.Recordings)
	}
}

func TestTimedSplitsKeepOneBroadcastClockAndHistory(t *testing.T) {
	store, log := fixture(t, false, false)
	streamer := store.Data().Streamers[0]
	cfg := store.Config()
	cfg.SplitMinutes = 30
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	source := bigo.Stream{Alive: 1, RoomID: "fixture-room"}
	start := time.Now().Add(-time.Hour)
	b := recording.NewSegmentedBroadcast(streamer, source, cfg, start)
	b.StartedAt = start
	first := filepath.Join(b.Directory, "first.mkv")
	second := filepath.Join(b.Directory, "second.mkv")
	h := &broadcastRecording{broadcast: b, rollingRecording: rollingRecording{
		fakeRecording: fakeRecording{done: make(chan struct{})}, path: first, stamp: start,
	}}
	e := newEngine(store, log, &fakeClient{}, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	nextStart := start.Add(30 * time.Minute)
	h.mu.Lock()
	h.path, h.stamp = second, nextStart
	h.changes = []recording.FileChange{
		{Path: second, BroadcastID: b.ID, StartedAt: nextStart},
		{Path: first, BroadcastID: b.ID, Result: &recording.Result{Path: first, Bytes: 100, EndedAt: nextStart}},
	}
	h.mu.Unlock()
	e.wakeUp()
	waitFor(t, func() bool { return len(store.Data().Recordings) == 2 })
	row := e.Snapshot().Rows[0]
	if row.Path != second || !row.StartedAt.Equal(start) {
		t.Fatal("stream clock moved at the timed split", row)
	}
	e.Close()
	data := store.Data()
	if len(data.Broadcasts) != 1 || data.Broadcasts[0].EndedAt.IsZero() || data.Broadcasts[0].Files != 2 {
		t.Fatal("timed capture was not finalized as one broadcast", data.Broadcasts)
	}
	for _, r := range data.Recordings {
		if r.BroadcastID != b.ID || filepath.Dir(r.Path) != b.Directory || r.EndedAt.IsZero() {
			t.Fatal("clip was not saved under the broadcast", r)
		}
	}
	options, err := e.broadcastOptions(context.Background(), streamer, source, cfg)
	if err != nil || options.Broadcast != data.Broadcasts[0] {
		t.Fatal("same room did not reuse the saved broadcast", options.Broadcast, err)
	}
	source.RoomID = "next-room"
	options, err = e.broadcastOptions(context.Background(), streamer, source, cfg)
	if err != nil || options.Broadcast.ID == b.ID || options.Broadcast.Directory == b.Directory {
		t.Fatal("new room reused the previous broadcast", options.Broadcast, err)
	}
}
