package recording

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
)

func TestWebFileSizeSplitsKeepEveryFrameAndAudioPacket(t *testing.T) {
	for _, name := range []string{"http_proxy", "HTTP_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	// Include source generation, paced input, and draining every live muxer.
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	source := filepath.Join(t.TempDir(), "source.ts")
	generate := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=s=64x96:r=5:d=40",
		"-f", "lavfi", "-i", "sine=duration=40", "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-g", "5",
		"-c:a", "aac", "-f", "mpegts", source)
	configureCommand(generate)
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate size fixture: %v: %s", err, out)
	}
	feed := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-readrate", "10", "-i", source, "-map", "0", "-c", "copy", "-f", "mpegts", "pipe:1")
	configureCommand(feed)
	input, err := feed.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	feed.Stderr = &stderr
	if err := feed.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); input.Close(); feed.Wait() })
	cfg := state.Defaults()
	cfg.RecordingPath = filepath.Join(t.TempDir(), "clips 100%")
	cfg.SplitBytes = 128 << 10 // Small synthetic limit exercises real cuts.
	cfg.FolderPerStreamer, cfg.FolderPerSplitRecording = false, false
	streamer := state.Streamer{ID: "host.example"}
	broadcast := NewSegmentedBroadcast(streamer, bigo.Stream{RoomID: "fixture-room"}, cfg, time.Now())
	capture := participantCapture{Capture: &fixtureCapture{parts: []*bigo.CapturePart{{ReadCloser: input}}}, broadcast: broadcast}
	h, err := startCapture(ctx, func() {}, ffmpeg, streamer, cfg, capture, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	last := h.Result()
	if err := feed.Wait(); err != nil {
		t.Fatal("synthetic feed failed", err, stderr.String())
	}
	if last.Err != nil || last.Bytes == 0 {
		t.Fatal("size capture failed", last, stderr.String())
	}
	paths := []string{h.Path}
	for _, change := range h.DrainFileChanges() {
		if change.BroadcastID != broadcast.ID || filepath.Dir(change.Path) != cfg.RecordingPath {
			t.Fatal("size cut lost its broadcast or storage location", change)
		}
		if change.Result == nil {
			paths = append(paths, change.Path)
		} else if change.Result.Err != nil || change.Result.Bytes < cfg.SplitBytes {
			t.Fatal("clip was cut early or failed", change.Result)
		}
	}
	if len(paths) < 2 {
		t.Fatal("file size limit never rotated the output")
	}
	var video []string
	counts := map[bool]int64{}
	for _, path := range paths {
		frames, _ := decodedHashes(t, ffmpeg, path)
		video = append(video, frames...)
		tracks, err := inspectReplayMedia(t.Context(), ffmpeg, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, track := range tracks {
			counts[track.Video] += track.Packets
		}
	}
	want, _ := decodedHashes(t, ffmpeg, source)
	if strings.Join(video, ",") != strings.Join(want, ",") {
		for i := range min(len(video), len(want)) {
			if video[i] != want[i] {
				at := -1
				for j, hash := range want {
					if hash == video[i] {
						at = j
						break
					}
				}
				t.Log("first differing frame", i, "matches source frame", at, "clips", len(paths))
				break
			}
		}
		t.Fatal("size cuts changed or lost video frames", len(video), len(want))
	}
	tracks, err := inspectReplayMedia(t.Context(), ffmpeg, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, track := range tracks {
		if counts[track.Video] != track.Packets {
			t.Fatal("size cuts changed the received packet count", track.Video, counts[track.Video], track.Packets)
		}
	}
}

func TestWebSizeCaptureCancellationBeforeMedia(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	capture, err := openWebSizeCapture(ctx, ffmpeg, &bigo.CapturePart{ReadCloser: reader})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := capture.Next(ctx); done <- err }()
	cancel()
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled startup lost its cause", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled size capture left startup blocked")
	}
}

func TestWebSizeSegmentsKeepSourceOrderAndDrainBeforeEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	capture := &webSizeCapture{ctx: ctx, cancel: cancel, source: &bigo.CapturePart{}, parts: make(chan *bigo.CapturePart), progress: make(chan struct{}), done: make(chan struct{})}
	go func() {
		<-ctx.Done()
		capture.mu.Lock()
		if capture.writer != nil {
			capture.writer.CloseWithError(ctx.Err())
		}
		capture.mu.Unlock()
	}()
	secondStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int64(0)
		if r.URL.Path == "/1" {
			index = 1
			close(secondStarted)
		}
		capture.segment(w, r, index)
	}))
	defer server.Close()
	sent := make(chan error, 2)
	send := func(index, body string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/"+index, strings.NewReader(body))
		if err == nil {
			var response *http.Response
			response, err = server.Client().Do(req)
			if err == nil {
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					err = errors.New("segment transfer failed")
				}
			}
		}
		sent <- err
	}
	go send("1", "second")
	select {
	case <-secondStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go send("0", "first")
	go func() {
		capture.drain(2)
		capture.mu.Lock()
		if capture.writer != nil {
			capture.writer.Close()
		}
		capture.mu.Unlock()
	}()
	part, err := capture.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(part)
	part.Close()
	if err != nil || string(data) != "firstsecond" {
		t.Fatal("segments were reordered or EOF preceded the last transfer", string(data), err)
	}
	for range 2 {
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
	}
}
