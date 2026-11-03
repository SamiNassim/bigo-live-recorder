package recording

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bigo-live-recorder/internal/state"
	"bigo-live-recorder/internal/timeline"
)

func TestPacketTimingParserUsesActualPackets(t *testing.T) {
	input := `#tb 0: 1/1000
#media_type 0: video
#tb 1: 1/1000
#media_type 1: audio
0, 0, 80, 40, 123, abcd
1, -7, -7, 20, 10, 1234
0, 40, 40, 40, 123, efab
1, 13, 13, 20, 10, 5678
`
	tracks, err := readPacketTimes(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].First != .04 || tracks[0].End != .12 || tracks[1].First != -.007 || tracks[1].End != .033 {
		t.Fatalf("tracks=%+v", tracks)
	}
}

func TestReplayHandlesResolutionChangesInsideOneOriginal(t *testing.T) {
	ff, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	for i, size := range []string{"64x96", "96x64"} {
		color := "red"
		if i == 1 {
			color = "blue"
		}
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=" + color + ":s=" + size + ":r=25", "-t", "0.4", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-f", "mpegts", filepath.Join(dir, fmt.Sprintf("input%d.ts", i))}
		if out, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
			t.Fatalf("resolution fixture: %v: %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "input.txt"), []byte("file 'input0.ts'\nfile 'input1.ts'\n"), 0644)
	source := filepath.Join(dir, "changing.mkv")
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "1", "-i", filepath.Join(dir, "input.txt"), "-c", "copy", source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("join changing source: %v: %s", err, out)
	}
	tracks, err := inspectReplayMedia(ctx, ff, source)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	duration := tracks[0].End - tracks[0].First
	p := timeline.Plan{Duration: duration, Slots: 1, Clips: []timeline.Clip{{File: "changing.mkv", UID: 900000001, ID: "guest.example", Video: true, Start: 0, End: duration, FileStart: tracks[0].First, FileEnd: tracks[0].End}}}
	if err := renderReplayPart(ctx, ff, dir, work, "part.mkv", p, 0, duration); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-ss", "0.65", "-i", filepath.Join(work, "part.mkv"), "-vf", "crop=2:2:958:498,scale=1:1", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
	if err != nil || len(out) != 3 || out[2] < 150 {
		t.Fatalf("lost video after resolution change: %v rgb=%v", err, out)
	}
}

func TestReplayRendersCameraChangesAndKeepsOriginals(t *testing.T) {
	ff, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	epoch := time.Unix(1900000000, 0)
	j, err := timeline.Open(dir, "fixture-room")
	if err != nil {
		t.Fatal(err)
	}
	checksums := make(map[string][32]byte)
	makeSource := func(name string, uid, base uint32, start, duration float64, color string) {
		t.Helper()
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
		if color != "" {
			args = append(args, "-f", "lavfi", "-i", "color=c="+color+":s=64x96:r=25")
		}
		frequency := 440
		if uid == 900000002 {
			frequency = 880
		}
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=f=%d:r=48000", frequency), "-t", fmt.Sprintf("%.3f", duration))
		if uid == 900000002 {
			args = append(args, "-af", "aselect='not(between(t,0.15,0.45))'")
		}
		if color != "" {
			args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1")
		}
		path := filepath.Join(dir, name)
		args = append(args, "-c:a", "pcm_s16le", "-f", "matroska", path)
		if out, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
			t.Fatalf("create source: %v: %s", err, out)
		}
		data, _ := os.ReadFile(path)
		checksums[path] = sha256.Sum256(data)
		w, err := j.Create(timeline.Entry{File: name, UID: uid, ID: fmt.Sprintf("guest-%d", uid-900000000), Name: "Camera 'Guest'; [x]", Epoch: epoch}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; float64(i)*.02 < duration-.001; i++ {
			at := float64(i) * .02
			if uid == 900000002 && at >= .15 && at <= .45 {
				continue
			}
			source := base + uint32((start+at)*1000)
			w.Write(timeline.Frame{Received: time.Duration((start + at + .05 + float64(i%5)*.001) * float64(time.Second)), SourcePTS: source, SourceDTS: source, PTS: int64(at * 90000), DTS: int64(at * 90000), Duration: 20 * time.Millisecond, Codec: 7})
		}
		if color != "" {
			for i := 0; float64(i)*.04 < duration-.001; i++ {
				at := float64(i) * .04
				source := base + uint32((start+at)*1000)
				w.Write(timeline.Frame{Received: time.Duration((start + at + .07) * float64(time.Second)), SourcePTS: source, SourceDTS: source, PTS: int64(at * 90000), DTS: int64(at * 90000), Video: true, Key: i == 0})
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	makeSource("prefix.mka", 900000001, 40000000, 0, .4, "")
	makeSource("camera-one.mkv", 900000001, 40000000, .4, 1.2, "red")
	makeSource("camera-two.mkv", 900000001, 40000000, 1.2, 1.2, "blue")
	makeSource("audio.mka", 900000002, 1700000000, .8, 1.2, "")
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath = ff
	b := state.Broadcast{ID: "fixture-room", StreamerID: "host.example", Directory: dir, StartedAt: epoch, EndedAt: epoch.Add(3 * time.Second)}
	// Use one-second batches to exercise concatenation, seeks and AAC
	// continuity across several boundaries without a minute-long fixture.
	path, err := assembleReplay(ctx, b, cfg, 25)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(path), b.StreamerID+"_") || !strings.HasSuffix(path, "_replay.mkv") {
		t.Fatal(path)
	}
	for source, want := range checksums {
		data, err := os.ReadFile(source)
		if err != nil || sha256.Sum256(data) != want {
			t.Fatal("original changed", source, err)
		}
	}
	// The guest's audio prefix must not manufacture video. Camera pre-roll
	// overlaps only in the originals; the combined replay changes camera once.
	for _, sample := range []struct {
		at      float64
		channel int
		minimum int
	}{{.2, 0, 0}, {.7, 0, 150}, {1.8, 2, 150}} {
		out, err := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.3f", sample.at), "-i", path, "-vf", "crop=2:2:478:498,scale=1:1", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
		if err != nil || len(out) != 3 {
			t.Fatalf("read replay frame: %v bytes=%v", err, out)
		}
		if sample.minimum == 0 {
			if int(out[0])+int(out[1])+int(out[2]) > 160 {
				t.Fatalf("invented camera during audio-only interval: %v", out)
			}
		} else if int(out[sample.channel]) < sample.minimum {
			t.Fatalf("wrong camera at %.2f: %v", sample.at, out)
		}
	}
	// Guest two is silent across the one-second batch boundary. Seeking
	// inside that gap must not move their next 880 Hz tone to the batch start.
	energy := func(at float64) float64 {
		t.Helper()
		pcm, err := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.3f", at), "-i", path, "-t", "0.08", "-map", "0:a:0", "-ac", "1", "-ar", "48000", "-f", "s16le", "pipe:1").Output()
		if err != nil {
			t.Fatal(err)
		}
		var re, im float64
		for i := 0; i+1 < len(pcm); i += 2 {
			sample := float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) / 32768
			angle := 2 * math.Pi * 880 * float64(i/2) / 48000
			re += sample * math.Cos(angle)
			im += sample * math.Sin(angle)
		}
		return math.Hypot(re, im) / float64(max(1, len(pcm)/2))
	}
	quiet, active := energy(1.04), energy(1.45)
	if active < .01 || quiet > active*.08 {
		t.Fatalf("audio silence moved across a batch boundary: quiet=%g active=%g", quiet, active)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := Replay(cancelled, b, cfg); err == nil {
		t.Fatal("accepted canceled replay")
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatal("cancellation removed the existing replay", err)
	}
}
