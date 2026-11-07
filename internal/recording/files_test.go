package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
	"bigo-live-recorder/internal/timeline"
)

func TestFinalizationRecordsWhetherGuestMediaWasSaved(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	dir := t.TempDir()
	journal, err := timeline.Open(dir, "fixture-room")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	media, err := os.ReadFile(fileFixture(t, ffmpeg, "libx264", "64x96"))
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, media} {
		part := &bigo.CapturePart{
			ReadCloser: io.NopCloser(bytes.NewReader(data)),
			Directory:  dir,
			Timing:     &bigo.CaptureTiming{Epoch: time.Unix(1900000000, 0)},
		}
		writer, err := startFile(ffmpeg, state.Streamer{ID: "host.example"}, state.Defaults(), part, captureOptions{timing: journal})
		if err != nil {
			t.Fatal(err)
		}
		result := writer.finish(<-writer.copied, false)
		entries, err := timeline.Read(dir)
		if err != nil {
			t.Fatal(err)
		}
		end := entries[len(entries)-1]
		wantNoMedia := len(data) == 0
		if end.Kind != "file_end" || end.File != filepath.Base(result.Path) || end.NoMedia != wantNoMedia || !end.At.Equal(result.EndedAt) {
			t.Fatal("incorrect finalization evidence", end)
		}
		if (result.Bytes == 0) != wantNoMedia || (result.Err != nil) != wantNoMedia {
			t.Fatal("incorrect recording result", result)
		}
	}
}

func TestNativeCaptureSavesVeryShortVideo(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	generate := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "color=c=black:s=64x96:r=25", "-frames:v", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-threads", "1",
		"-bsf:v", "filter_units=remove_types=6", "-muxdelay", "0", "-f", "mpegts", "pipe:1")
	configureCommand(generate)
	transport, err := generate.Output()
	if err != nil {
		t.Fatal("generate short video", err)
	}
	// The native transport emits program tables and media without an SDT.
	// Keep the fixture short enough to reproduce ambiguous input probing.
	var input []byte
	for offset := 0; offset+188 <= len(transport); offset += 188 {
		packet := transport[offset : offset+188]
		pid := uint16(packet[1]&31)<<8 | uint16(packet[2])
		if pid != 17 {
			input = append(input, packet...)
		}
	}
	if len(input) != 4*188 {
		t.Fatal("expected two tables and two video packets", len(input))
	}
	part := &bigo.CapturePart{ReadCloser: io.NopCloser(bytes.NewReader(input)), Native: true}
	capture := &fixtureCapture{parts: []*bigo.CapturePart{part}}
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	h, err := startCapture(ctx, cancel, ffmpeg, state.Streamer{ID: "host.example"}, cfg, capture, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := h.Result()
	if result.Err != nil || result.Bytes == 0 {
		t.Fatal("short native recording was not saved", result.Err)
	}
	// The capture cancels its context on completion. Inspect the saved file
	// with a fresh deadline.
	probeCtx, stopProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopProbe()
	tracks, err := inspectReplayMedia(probeCtx, ffmpeg, result.Path)
	if err != nil || len(tracks) != 1 || !tracks[0].Video || tracks[0].Packets != 2 {
		t.Fatal("short native recording lost video", tracks, err)
	}
}

type fixtureCapture struct {
	mu      sync.Mutex
	parts   []*bigo.CapturePart
	opened  []*bigo.CapturePart
	wait    bool
	waiting chan struct{}
}

func (c *fixtureCapture) Next(ctx context.Context) (*bigo.CapturePart, error) {
	c.mu.Lock()
	if len(c.parts) > 0 {
		p := c.parts[0]
		c.parts = c.parts[1:]
		c.opened = append(c.opened, p)
		c.mu.Unlock()
		return p, nil
	}
	wait := c.wait
	c.mu.Unlock()
	if wait {
		if c.waiting != nil {
			close(c.waiting)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, io.EOF
}
func (c *fixtureCapture) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.opened {
		p.Close()
	}
	return nil
}

func fileFixture(t *testing.T, ffmpeg, codec, size string, offset ...time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=10:duration=0.6", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=0.6", "-c:v", codec, "-preset", "ultrafast", "-bf", "0", "-g", "3", "-c:a", "aac", "-muxdelay", "0"}
	if codec == "libx265" {
		args = append(args, "-x265-params", "log-level=error:pools=1:frame-threads=1:repeat-headers=1")
	}
	if len(offset) > 0 {
		args = append(args, "-output_ts_offset", strconv.FormatFloat(offset[0].Seconds(), 'f', 3, 64))
	}
	args = append(args, "-mpegts_flags", "+initial_discontinuity")
	args = append(args, "-f", "mpegts", path)
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	configureCommand(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, out)
	}
	return path
}

func decodedHashes(t *testing.T, ffmpeg, path string) ([]string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", path, "-map", "0:v:0", "-f", "framemd5", "-")
	configureCommand(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("decode recording video", err)
	}
	var video []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		video = append(video, strings.TrimSpace(fields[len(fields)-1]))
	}
	cmd = exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", path, "-map", "0:a:0", "-f", "hash", "-hash", "sha256", "-")
	configureCommand(cmd)
	out, err = cmd.Output()
	if err != nil {
		t.Fatal("decode recording audio", err)
	}
	return video, strings.TrimSpace(string(out))
}

func TestFFmpegRolloverKeepsEveryVideoFrameAndAudioSample(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	paths := []string{fileFixture(t, ffmpeg, "libx264", "64x96"), fileFixture(t, ffmpeg, "libx265", "80x120"), fileFixture(t, ffmpeg, "libx264", "64x96")}
	capture := &fixtureCapture{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		capture.parts = append(capture.parts, &bigo.CapturePart{ReadCloser: io.NopCloser(bytes.NewReader(b)), Native: true, Reason: "Video encoding changed; continuing in a new recording"})
	}
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.FolderPerStreamer = false
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := startCapture(ctx, cancel, ffmpeg, state.Streamer{ID: "demo", Name: "Demo"}, cfg, capture, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	last := h.Result()
	if last.Err != nil {
		t.Fatal(last.Err)
	}
	results := make(map[string]Result)
	results[last.Path] = last
	changes := h.DrainFileChanges()
	var order []string
	for _, c := range changes {
		if c.Result != nil {
			results[c.Path] = *c.Result
		} else {
			order = append(order, c.Path)
		}
	}
	if len(results) != 3 || len(order) != 2 {
		t.Fatal("rollover lost file history", len(results), len(order))
	}
	outputs := append([]string{h.Path}, order...)
	for i, path := range outputs {
		if r := results[path]; r.Err != nil || r.Bytes == 0 || r.EndedAt.IsZero() {
			t.Fatal("file was not finalized", r)
		}
		if !strings.HasPrefix(filepath.Base(path), "demo_") || strings.Contains(filepath.Base(path), "part") {
			t.Fatal("ordinary filename changed", path)
		}
		wantVideo, wantAudio := decodedHashes(t, ffmpeg, paths[i])
		gotVideo, gotAudio := decodedHashes(t, ffmpeg, path)
		if len(gotVideo) != 6 || strings.Join(gotVideo, ",") != strings.Join(wantVideo, ",") || gotAudio != wantAudio {
			t.Fatal("rollover changed or lost received media", i, len(gotVideo), len(wantVideo), gotAudio, wantAudio)
		}
	}
	if h.File() != last.Path {
		t.Fatal("active file path did not advance")
	}
}

func TestStopWhileWaitingForNextFormatFinalizesPromptly(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	path := fileFixture(t, ffmpeg, "libx264", "64x96")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	capture := &fixtureCapture{parts: []*bigo.CapturePart{{ReadCloser: io.NopCloser(bytes.NewReader(b)), Native: true}}, wait: true, waiting: make(chan struct{})}
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.FolderPerStreamer = false
	ctx, cancel := context.WithCancel(context.Background())
	h, err := startCapture(ctx, cancel, ffmpeg, state.Streamer{ID: "demo"}, cfg, capture, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-capture.waiting:
	case <-time.After(5 * time.Second):
		h.Stop()
		t.Fatal("recording never reached the file transition")
	}
	h.Stop()
	select {
	case <-h.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop stalled during a file transition")
	}
	if r := h.Result(); r.Err != nil && !errors.Is(r.Err, context.Canceled) {
		t.Fatal(r.Err)
	}
}

func TestMKVResolutionChangesKeepAllFrames(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	for _, codec := range []string{"libx264", "libx265"} {
		t.Run(codec, func(t *testing.T) {
			a, b, c := fileFixture(t, ffmpeg, codec, "64x96", 0), fileFixture(t, ffmpeg, codec, "80x120", 600*time.Millisecond), fileFixture(t, ffmpeg, codec, "64x96", 1200*time.Millisecond)
			var data []byte
			for _, p := range []string{a, b, c} {
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, raw...)
			}
			capture := &fixtureCapture{parts: []*bigo.CapturePart{{ReadCloser: io.NopCloser(bytes.NewReader(data)), Native: true}}}
			cfg := state.Defaults()
			cfg.RecordingPath = t.TempDir()
			cfg.FolderPerStreamer = false
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			recordCtx, stopRecord := context.WithCancel(ctx)
			h, err := startCapture(recordCtx, stopRecord, ffmpeg, state.Streamer{ID: "demo"}, cfg, capture, nil)
			if err != nil {
				t.Fatal(err)
			}
			r := h.Result()
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			files, _ := filepath.Glob(filepath.Join(cfg.RecordingPath, "*.mkv"))
			if len(files) != 1 {
				t.Fatal("resolution changes created multiple files", len(files))
			}
			cmd := exec.CommandContext(ctx, probe, "-v", "error", "-select_streams", "v:0", "-show_frames", "-show_entries", "frame=width,height", "-of", "json", r.Path)
			configureCommand(cmd)
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			var result struct{ Frames []struct{ Width, Height int } }
			if err = json.Unmarshal(out, &result); err != nil {
				t.Fatal(err)
			}
			counts := map[int]int{}
			for _, f := range result.Frames {
				counts[f.Width]++
			}
			if counts[64] != 12 || counts[80] != 6 {
				t.Fatal("resolution changes lost video", counts)
			}
		})
	}
}
