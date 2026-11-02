package recording

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
)

type fixtureGuestRoom struct {
	guests chan bigo.Participant
	once   sync.Once
}

func (r *fixtureGuestRoom) Next(ctx context.Context) (bigo.Participant, error) {
	select {
	case guest, ok := <-r.guests:
		if !ok {
			return bigo.Participant{}, io.EOF
		}
		return guest, nil
	case <-ctx.Done():
		return bigo.Participant{}, ctx.Err()
	}
}
func (r *fixtureGuestRoom) Close() error { r.once.Do(func() { close(r.guests) }); return nil }

func TestGuestFilesRemainIndependentAndFinalizeExactlyOnce(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	fixture := fileFixture(t, ffmpeg, "libx264", "64x96")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(t.TempDir(), "audio.ts")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-i", fixture, "-map", "0:a:0", "-c", "copy", "-f", "mpegts", audioPath).CombinedOutput(); err != nil {
		t.Fatalf("audio fixture: %v: %s", err, out)
	}
	audioData, err := os.ReadFile(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	room := &fixtureGuestRoom{guests: make(chan bigo.Participant, 2)}
	for i, uid := range []uint32{900000001, 900000002} {
		payload := data
		if i == 1 {
			payload = audioData
		}
		capture := &fixtureCapture{parts: []*bigo.CapturePart{{ReadCloser: io.NopCloser(bytes.NewReader(payload)), AudioOnly: i == 1}}}
		room.guests <- bigo.Participant{UID: uid, Capture: capture}
	}
	room.Close()
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.FolderPerStreamer = true
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h := startGuestRoom(ctx, cancel, ffmpeg, state.Streamer{ID: "demo", Name: "Demo"}, cfg, room, nil)
	result := h.Result()
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	starts, ends := map[string]int{}, map[string]int{}
	for _, change := range h.DrainFileChanges() {
		if change.Result == nil {
			starts[change.Path]++
		} else {
			ends[change.Path]++
			if change.Result.Err != nil || change.Result.Bytes == 0 {
				t.Fatal("guest finalization failed", change.Result)
			}
		}
	}
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatal("lost guest history", starts, ends)
	}
	video, audio := false, false
	for path, count := range starts {
		if count != 1 || ends[path] != 1 {
			t.Fatal("duplicate guest lifecycle events")
		}
		if !strings.HasPrefix(filepath.Base(path), "demo_") || filepath.Base(filepath.Dir(path)) != "demo_Demo" {
			t.Fatal("guest filename or folder lost the monitored ID")
		}
		video = video || strings.Contains(path, "guest-900000001_") && strings.HasSuffix(path, ".mkv")
		audio = audio || strings.Contains(path, "guest-900000002_") && strings.HasSuffix(path, ".mka")
	}
	if !video || !audio || h.File() != "" {
		t.Fatal("wrong guest file type or active status")
	}
}

func TestGuestRoomStopWhileWaitingForMediaIsPrompt(t *testing.T) {
	room := &fixtureGuestRoom{guests: make(chan bigo.Participant)}
	ctx, cancel := context.WithCancel(context.Background())
	h := startGuestRoom(ctx, cancel, "", state.Streamer{ID: "demo"}, state.Defaults(), room, nil)
	if h.File() != "" || !h.Started().IsZero() {
		t.Fatal("created a fictitious initial recording")
	}
	h.Stop()
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("room shutdown blocked without media")
	}
	if h.Result().Err != nil || len(h.DrainFileChanges()) != 0 {
		t.Fatal("empty room shutdown created an error or file")
	}
}
