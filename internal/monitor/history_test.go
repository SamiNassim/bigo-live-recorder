package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

type historyRecording struct {
	fakeRecording
	result recording.Result
}

func (h *historyRecording) File() string { return h.result.Path }
func (h *historyRecording) Result() recording.Result {
	<-h.done
	return h.result
}

func TestRecordingFinalizationSurvivesHistoryWriteFailure(t *testing.T) {
	store, log := fixture(t, false, false)
	path := filepath.Join(t.TempDir(), "partial.mkv")
	media := []byte("partial media")
	if err := os.WriteFile(path, media, 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute)
	result := recording.Result{Path: path, StartedAt: started, EndedAt: time.Now(),
		Bytes: int64(len(media)), Err: errors.New("fixture muxer failure")}
	h := &historyRecording{fakeRecording: fakeRecording{done: make(chan struct{}), started: started}, result: result}
	client := &fakeClient{}
	e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	defer e.Close()
	waitFor(t, func() bool {
		return len(store.Data().Recordings) == 1 && e.Snapshot().Recording == 1
	})

	// Disable further checks so no unrelated write can save the history.
	cfg := store.Config()
	cfg.NativeListening = true
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(store.Directory(), "data.json")
	backup := name + ".saved"
	if err := os.Rename(name, backup); err != nil {
		t.Fatal(err)
	}
	// A directory in place of the file blocks atomic replacement on every platform.
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	revision := store.DataRevision()
	h.Stop()
	e.wakeUp()
	waitFor(t, func() bool { return e.Snapshot().Recording == 0 })
	data, newRevision := store.DataSince(revision)
	if len(data.Recordings) != 1 || newRevision == revision {
		t.Fatal("finalization did not publish a new history snapshot")
	}
	r := data.Recordings[0]
	if !r.EndedAt.Equal(result.EndedAt) || r.Bytes != result.Bytes || r.Error != result.Err.Error() {
		t.Fatalf("history did not reflect the completed recorder: %+v", r)
	}
	if err := store.RemoveRecording(r.ID); err == nil || len(store.Data().Recordings) != 1 {
		t.Fatal("failed history removal did not preserve the pending finalization")
	}

	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		reopened, err := state.Open(store.Directory())
		if err != nil {
			t.Fatal(err)
		}
		saved := reopened.Data().Recordings
		if len(saved) == 1 && saved[0].EndedAt.Equal(result.EndedAt) && saved[0].Bytes == result.Bytes && saved[0].Error == result.Err.Error() {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("recording history was not saved after writes became possible")
		case <-tick.C:
		}
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(media) {
		t.Fatalf("history recovery changed the media: %q, %v", got, err)
	}
}
