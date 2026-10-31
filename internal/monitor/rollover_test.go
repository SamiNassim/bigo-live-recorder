package monitor

import (
	"context"
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
}

func (h *rollingRecording) File() string       { h.mu.Lock(); defer h.mu.Unlock(); return h.path }
func (h *rollingRecording) Started() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.stamp }
func (h *rollingRecording) DrainFileChanges() []recording.FileChange {
	h.mu.Lock()
	defer h.mu.Unlock()
	changes := h.changes
	h.changes = nil
	return changes
}
func (h *rollingRecording) Result() recording.Result {
	<-h.done
	return recording.Result{Path: h.File(), Bytes: 2222, EndedAt: time.Now()}
}

func TestRolloverPersistsIndependentFilesWithoutRestartingMonitoring(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &fakeClient{}
	h := &rollingRecording{fakeRecording: fakeRecording{done: make(chan struct{})}, path: "/fixture/first.mkv", stamp: time.Now()}
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
	if s.Recording != 1 || s.Rows[0].Path != "/fixture/second.mkv" || s.Rows[0].StartedAt != stamp {
		t.Fatal("active file did not advance", s)
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
