package recording

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
)

// This local-only integration test exercises the production stall interval and
// real FFmpeg process. No BIGO account, remote stream or browser is involved.
func TestFFmpegKeepsOneMKVAcrossPlaybackRecovery(t *testing.T) {
	if os.Getenv("BIGO_FFMPEG_RECOVERY_TEST") != "1" {
		t.Skip("set BIGO_FFMPEG_RECOVERY_TEST=1 for the ~20-second local FFmpeg test")
	}
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	generate := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-t", "6", "-an", "-c:v", "mpeg2video", "-g", "10", "-f", "hls", "-hls_time", "1", "-hls_list_size", "0", "-hls_segment_filename", filepath.Join(fixtures, "segment%d.ts"), filepath.Join(fixtures, "generated.m3u8"))
	configureCommand(generate)
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate media: %v: %s", err, out)
	}
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/frozen.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\nsegment0.ts\nsegment1.ts\nsegment2.ts\n")
		case "/refreshed.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:3\nsegment3.ts\nsegment4.ts\nsegment5.ts\n#EXT-X-ENDLIST\n")
		default:
			http.ServeFile(w, r, filepath.Join(fixtures, filepath.Base(r.URL.Path)))
		}
	}))
	defer server.Close()
	cfg := state.Defaults()
	cfg.RecordingPath, cfg.FolderPerStreamer = t.TempDir(), false
	events := make(chan string, 20)
	h, err := Start(ctx, state.Streamer{ID: "fixture", Name: "Recovery fixture"}, bigo.Stream{Alive: 1, RoomID: "fixture-room", HLS: server.URL + "/frozen.m3u8"}, cfg, bigo.StreamRecovery{
		Refresh: func(context.Context) (bigo.Stream, error) {
			if checks.Add(1) == 1 {
				return bigo.Stream{Alive: 1, RoomID: "fixture-room", HLS: server.URL + "/refreshed.m3u8"}, nil
			}
			return bigo.Stream{}, nil
		},
		Report: func(_, message string) { events <- message },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	result := h.Result()
	if result.Err != nil || result.Bytes == 0 || result.Path != h.File() {
		t.Fatalf("recording: %+v", result)
	}
	files, err := filepath.Glob(filepath.Join(cfg.RecordingPath, "*.mkv"))
	if err != nil || len(files) != 1 {
		t.Fatal("recovery split the recording", files, err)
	}
	if checks.Load() != 2 {
		t.Fatal("expected one stalled-playback refresh and one end verification", checks.Load())
	}
	resumed := false
	for len(events) > 0 {
		if strings.Contains(<-events, "continuing the same recording file") {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("missing recovery success event")
	}
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0", "-count_frames", "-show_entries", "stream=nb_read_frames", "-of", "json", result.Path)
		configureCommand(cmd)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Streams []struct {
				Frames string `json:"nb_read_frames"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(out, &response); err != nil || len(response.Streams) != 1 {
			t.Fatal(string(out), err)
		}
		frames, err := strconv.Atoi(response.Streams[0].Frames)
		if err != nil || frames != 60 {
			t.Fatal("media missing or duplicated across recovery", frames, err)
		}
		t.Log("One MKV contains all 60 frames across a frozen playlist and playback refresh")
	}
}
