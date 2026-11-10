package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
)

type nativeClockReader struct{ io.ReadCloser }

func (nativeClockReader) NativeTimestamps() bool { return true }

func TestRecordingPreservesOnlyNativeNormalizedTimestamps(t *testing.T) {
	for _, native := range []bool{false, true} {
		var stream io.ReadCloser = io.NopCloser(bytes.NewReader(nil))
		if native {
			stream = nativeClockReader{stream}
		}
		args := recordingArgs(stream, "recording.mkv")
		if slices.Contains(args, "-copyts") != native || slices.Contains(args, "-start_at_zero") != native || slices.Contains(args, "mpegts") != native {
			t.Fatal("wrong input policy", native, args)
		}
		if slices.Contains(args, "-bsf:v") == native {
			t.Fatal("video timestamp repair should apply only to the web feed", args)
		}
	}
}

func TestWebRecordingRepairsVideoPresentationTimestamp(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixtures := t.TempDir()
	source := filepath.Join(fixtures, "video.ts")
	// Move one PTS far forward while its DTS and surrounding packets stay valid.
	generate := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
		"-f", "lavfi", "-i", "testsrc2=size=64x96:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "4",
		"-c:v", "libx264", "-g", "24", "-bf", "2", "-c:a", "aac",
		"-bsf:v", `setts=pts='if(eq(N,60),PTS+3600/TB,PTS)'`, "-f", "mpegts", source)
	configureCommand(generate)
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate media: %v: %s", err, out)
	}
	const playlist = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nvideo.ts\n#EXT-X-ENDLIST\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.m3u8" {
			io.WriteString(w, playlist)
			return
		}
		http.ServeFile(w, r, source)
	}))
	defer server.Close()
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	capture, err := Start(ctx, state.Streamer{ID: "fixture"}, bigo.Stream{HLS: server.URL + "/live.m3u8"}, cfg, bigo.StreamRecovery{})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Stop()
	result := capture.Result()
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	inspect := exec.CommandContext(ctx, probe, "-v", "error", "-show_packets",
		"-show_entries", "packet=stream_index,pts_time:format=duration", "-of", "json", result.Path)
	configureCommand(inspect)
	out, err := inspect.Output()
	if err != nil {
		t.Fatal(err)
	}
	var media struct {
		Packets []struct {
			Stream int     `json:"stream_index"`
			PTS    float64 `json:"pts_time,string"`
		} `json:"packets"`
		Format struct {
			Duration float64 `json:"duration,string"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &media); err != nil {
		t.Fatal(err)
	}
	var counts [2]int
	var lastVideoPTS float64
	var reordered bool
	for _, packet := range media.Packets {
		counts[packet.Stream]++
		if packet.Stream == 0 {
			reordered = reordered || packet.PTS < lastVideoPTS
			lastVideoPTS = packet.PTS
		}
	}
	if media.Format.Duration < 4 || media.Format.Duration > 4.2 || counts[0] != 96 || counts[1] < 170 || !reordered {
		t.Fatalf("unexpected recording timeline: duration %g, packets %v, reordered video %t", media.Format.Duration, counts, reordered)
	}
}

// A real timestamp gap needs no wall-clock wait. Verify FFmpeg retains it for
// both tracks instead of independently correcting the already normalized input.
func TestFFmpegPreservesNativeAudioVideoGap(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "gap.ts")
	generate := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2",
		"-vf", `setpts=PTS+20/TB*gte(T\,1)`,
		"-af", `asetpts=PTS+20/TB*gte(T\,1)`,
		"-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-g", "10", "-fps_mode", "passthrough",
		"-c:a", "aac", "-mpegts_copyts", "1", "-muxdelay", "0", "-f", "mpegts", source)
	configureCommand(generate)
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate media: %v: %s", err, out)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output := filepath.Join(dir, "recording.mkv")
	cmd := exec.CommandContext(ctx, ffmpeg, recordingArgs(nativeClockReader{input}, output)...)
	configureCommand(cmd)
	cmd.Stdin = input
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("record media: %v: %s", err, out)
	}
	cmd = exec.CommandContext(ctx, probe, "-v", "error", "-show_packets", "-show_entries", "packet=stream_index,pts_time,duration_time", "-of", "json", output)
	configureCommand(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Packets []struct {
			Stream   int    `json:"stream_index"`
			PTS      string `json:"pts_time"`
			Duration string `json:"duration_time"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	var last, end, gap [2]float64
	var count [2]int
	for _, packet := range result.Packets {
		i := packet.Stream
		if i < 0 || i >= len(last) {
			t.Fatal("unexpected media track", i)
		}
		pts, err := strconv.ParseFloat(packet.PTS, 64)
		if err != nil {
			t.Fatal(err)
		}
		duration, _ := strconv.ParseFloat(packet.Duration, 64)
		gap[i] = max(gap[i], pts-last[i])
		last[i], end[i] = pts, pts+duration
		count[i]++
	}
	if count[0] != 20 || count[1] < 80 || gap[0] < 19 || gap[1] < 19 || math.Abs(end[0]-end[1]) > 0.1 {
		t.Fatal("timestamp correction changed the recording timeline", count, gap, end)
	}
}
