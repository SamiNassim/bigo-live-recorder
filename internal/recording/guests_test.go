package recording

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/state"
)

type fixtureGuestRoom struct {
	guests chan bigo.Participant
	once   sync.Once
	end    error
}

func (r *fixtureGuestRoom) Next(ctx context.Context) (bigo.Participant, error) {
	select {
	case guest, ok := <-r.guests:
		if !ok {
			if r.end != nil {
				return bigo.Participant{}, r.end
			}
			return bigo.Participant{}, io.EOF
		}
		return guest, nil
	case <-ctx.Done():
		return bigo.Participant{}, ctx.Err()
	}
}

func TestGuestRoomModeChangeFinalizesCaptureWithoutClosingBroadcast(t *testing.T) {
	room := &fixtureGuestRoom{guests: make(chan bigo.Participant), end: bigo.ErrRoomModeChanged}
	room.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startGuestRoom(ctx, cancel, "", state.Streamer{ID: "host.example"}, state.Defaults(), room, bigo.StreamRecovery{})
	r := h.Result()
	if !r.RoomModeChanged || r.RoomEnded || r.Err != nil {
		t.Fatal("mode change was confused with recording failure or room closure", r)
	}
}
func (r *fixtureGuestRoom) Close() error { r.once.Do(func() { close(r.guests) }); return nil }

func TestGuestMissingVideoHeadersDoesNotStopTheRoom(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := fileFixture(t, ffmpeg, "libx264", "64x96")
	good, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the advertised tracks, but no elementary media or codec headers.
	var bad []byte
	for offset := 0; offset+188 <= len(good); offset += 188 {
		packet := good[offset : offset+188]
		pid := uint16(packet[1]&31)<<8 | uint16(packet[2])
		if pid == 0 || pid == 4096 {
			bad = append(bad, packet...)
		}
	}
	room := &fixtureGuestRoom{guests: make(chan bigo.Participant, 2)}
	for i, data := range [][]byte{bad, good} {
		room.guests <- bigo.Participant{UID: 900000001 + uint32(i), Capture: &fixtureCapture{parts: []*bigo.CapturePart{{ReadCloser: io.NopCloser(bytes.NewReader(data))}}}}
	}
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	h := startGuestRoom(ctx, cancel, ffmpeg, state.Streamer{ID: "host.example"}, cfg, room, bigo.StreamRecovery{})
	defer h.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	finalized := make(map[uint32]Result)
	for len(finalized) < 2 {
		for _, change := range h.DrainFileChanges() {
			if change.Result != nil {
				finalized[change.Guest.UID] = *change.Result
			}
		}
		select {
		case <-h.Done():
			t.Fatal("a guest without video headers stopped the room", h.Result())
		case <-ctx.Done():
			t.Fatal("guest finalization stalled", ctx.Err())
		case <-tick.C:
		}
	}
	failed, saved := finalized[900000001], finalized[900000002]
	if failed.Err == nil || fault.RequiresAction(failed.Err) || !strings.Contains(fault.Details(failed.Err), "dimensions not set") {
		t.Fatal("guest source failure was not retained correctly", failed)
	}
	if saved.Err != nil || saved.Bytes == 0 {
		t.Fatal("healthy guest recording failed", saved)
	}
}

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
	room := &fixtureGuestRoom{guests: make(chan bigo.Participant, 3)}
	for i, uid := range []uint32{900000001, 900000002, 900000003} {
		payload := data
		if i > 0 {
			payload = audioData
		}
		capture := &fixtureCapture{parts: []*bigo.CapturePart{{ReadCloser: io.NopCloser(bytes.NewReader(payload)), AudioOnly: i > 0}}}
		room.guests <- bigo.Participant{UID: uid, Capture: capture}
	}
	room.Close()
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.FolderPerStreamer = true
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	streamer := state.Streamer{ID: "demo", Name: "Demo"}
	broadcast := NewBroadcast(streamer, bigo.Stream{UID: 900000010, RoomID: "8589934634"}, cfg, time.Now())
	h := startGuestRoom(ctx, cancel, ffmpeg, streamer, cfg, room, bigo.StreamRecovery{}, Options{Broadcast: broadcast, GuestProfile: func(_ context.Context, uid uint32) (bigo.Profile, error) {
		if uid == 900000001 {
			return bigo.Profile{UID: uid, ID: "guest.one", Name: "Guest One"}, nil
		}
		if uid == 900000003 {
			return bigo.Profile{UID: uid, Name: "Guest Without ID"}, errors.New("public ID unavailable")
		}
		return bigo.Profile{UID: uid, ID: "guest.two", Name: "Guest Two"}, nil
	}})
	result := h.Result()
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	starts, ends := map[string]int{}, map[string]int{}
	for _, change := range h.DrainFileChanges() {
		if change.BroadcastID != broadcast.ID || (change.Guest.ID == "" && change.Guest.UID != 900000003) || change.Guest.Name == "" {
			t.Fatal("guest or broadcast metadata missing", change)
		}
		if change.Result == nil {
			starts[change.Path]++
		} else {
			ends[change.Path]++
			if change.Result.Err != nil || change.Result.Bytes == 0 {
				t.Fatal("guest finalization failed", change.Result)
			}
		}
	}
	if len(starts) != 3 || len(ends) != 3 {
		t.Fatal("lost guest history", starts, ends)
	}
	video, audio := false, false
	unidentified := false
	for path, count := range starts {
		if count != 1 || ends[path] != 1 {
			t.Fatal("duplicate guest lifecycle events")
		}
		if !strings.HasPrefix(filepath.Base(path), "demo_") || filepath.Dir(path) != broadcast.Directory {
			t.Fatal("guest filename or folder lost the monitored ID")
		}
		video = video || strings.Contains(path, "guest.one_Guest One_") && strings.HasSuffix(path, ".mkv")
		audio = audio || strings.Contains(path, "guest.two_Guest Two_") && strings.HasSuffix(path, ".mka")
		unidentified = unidentified || strings.Contains(path, "unidentified-guest_Guest Without ID_") && strings.HasSuffix(path, ".mka")
		if strings.Contains(filepath.Base(path), "900000003") {
			t.Fatal("routing UID leaked into an unidentified guest filename")
		}
	}
	if !video || !audio || !unidentified || h.File() != "" {
		t.Fatal("wrong guest file type or active status")
	}
}

func TestGuestRoomStopWhileWaitingForMediaIsPrompt(t *testing.T) {
	room := &fixtureGuestRoom{guests: make(chan bigo.Participant)}
	ctx, cancel := context.WithCancel(context.Background())
	h := startGuestRoom(ctx, cancel, "", state.Streamer{ID: "demo"}, state.Defaults(), room, bigo.StreamRecovery{})
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
