package monitor

import (
	"context"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

func TestGuestRoomWaitsForRealFilesAndNotifiesOnlyOnce(t *testing.T) {
	store, log := fixture(t, false, false)
	source := sourceFixture()
	source.RoomKind = bigo.RoomHybridMultiGuest
	client := &sourceClient{check: func(context.Context, string) (bigo.Stream, error) { return source, nil }}
	h := &rollingRecording{fakeRecording: fakeRecording{done: make(chan struct{})}}
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	defer e.Close()
	waitFor(t, func() bool {
		rows := e.Snapshot().Rows
		return len(rows) == 1 && rows[0].Status == "Waiting for guest media"
	})
	if len(store.Data().Recordings) != 0 || len(e.Events()) != 0 || !store.Data().Streamers[0].LastRecordedAt.IsZero() {
		t.Fatal("empty room created a recording or notification")
	}
	stamp := time.Now()
	first, second := "/fixture/guest-a.mkv", "/fixture/guest-b.mka"
	h.mu.Lock()
	h.path, h.stamp = second, stamp.Add(time.Second)
	h.changes = []recording.FileChange{{Path: first, StartedAt: stamp}, {Path: second, StartedAt: h.stamp}}
	h.mu.Unlock()
	e.wakeUp()
	waitFor(t, func() bool { return len(store.Data().Recordings) == 2 && e.Snapshot().Recording == 1 })
	if len(e.Events()) != 1 {
		t.Fatal("guest files sent multiple live notifications")
	}
	h.mu.Lock()
	h.path, h.stamp = "", time.Time{}
	h.changes = []recording.FileChange{{Path: first, Result: &recording.Result{Path: first, EndedAt: time.Now(), Bytes: 111}}, {Path: second, Result: &recording.Result{Path: second, EndedAt: time.Now(), Bytes: 222}}}
	h.mu.Unlock()
	e.wakeUp()
	waitFor(t, func() bool {
		rows := e.Snapshot().Rows
		return len(rows) == 1 && rows[0].Status == "Waiting for guest media"
	})
	third := "/fixture/guest-c.mka"
	h.mu.Lock()
	h.path, h.stamp = third, stamp.Add(2*time.Second)
	h.changes = []recording.FileChange{{Path: third, StartedAt: h.stamp}}
	h.mu.Unlock()
	e.wakeUp()
	waitFor(t, func() bool { return len(store.Data().Recordings) == 3 && e.Snapshot().Recording == 1 })
	if len(e.Events()) != 1 {
		t.Fatal("a returning guest sent another live notification")
	}
	e.Close()
	for _, saved := range store.Data().Recordings {
		if saved.Path == "" || saved.EndedAt.IsZero() {
			t.Fatal("guest history was not finalized")
		}
	}
}
