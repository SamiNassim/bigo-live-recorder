package recording

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"bigo-live-recorder/internal/timeline"
)

func TestReplaySpeakersFollowIndividualAudioAcrossBatchBoundaries(t *testing.T) {
	ff, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dir, work := t.TempDir(), t.TempDir()
	p := timeline.Plan{Duration: 3.32, Slots: 2}
	checksums := make(map[string][32]byte)
	for i, interval := range [][2]float64{{.4, 1.8}, {.9, 2.1}} {
		pcm := speechPCM(3, func(at float64) float64 {
			if at < interval[0] || at >= interval[1] {
				return 0
			}
			return speechFixture(at)
		})
		raw := filepath.Join(dir, fmt.Sprintf("input%d.pcm", i))
		if err := os.WriteFile(raw, pcm, 0600); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("guest%d.mka", i)
		path := filepath.Join(dir, name)
		cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-f", "s16le", "-ar", "16000", "-ac", "1", "-i", raw, "-af", "asetpts=PTS+8/TB", "-c:a", "pcm_s16le", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create timed audio: %v: %s", err, out)
		}
		tracks, err := inspectReplayMedia(ctx, ff, path)
		if err != nil || len(tracks) != 1 || tracks[0].First < 7.9 {
			t.Fatalf("invalid shifted audio fixture: %v %+v", err, tracks)
		}
		p.Clips = append(p.Clips, timeline.Clip{File: name, UID: uint32(900000001 + i), ID: fmt.Sprintf("guest-%d.example", i), Name: "Demo Guest", Slot: i, Start: .3, End: 3.3, FileStart: tracks[0].First, FileEnd: tracks[0].End})
		data, _ := os.ReadFile(path)
		checksums[path] = sha256.Sum256(data)
	}
	for i, span := range [][2]float64{{0, 2}, {2, 3.32}} {
		if err := renderReplayPart(ctx, ff, dir, work, fmt.Sprintf("part%d.mkv", i), p, span[0], span[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		at     float64
		active [2]bool
	}{{.15, [2]bool{false, false}}, {.9, [2]bool{true, false}}, {1.6, [2]bool{true, true}}, {2.05, [2]bool{true, true}}, {2.95, [2]bool{false, false}}} {
		part, seek := 0, check.at
		if seek >= 2 {
			part, seek = 1, seek-2
		}
		for guest, want := range check.active {
			out, err := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.3f", seek), "-i", filepath.Join(work, fmt.Sprintf("part%d.mkv", part)), "-vf", fmt.Sprintf("crop=4:4:%d:100,scale=1:1", guest*960), "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
			if err != nil || len(out) != 3 {
				t.Fatalf("read speaker indicator: %v %v", err, out)
			}
			active := int(out[2])-int(out[0]) > 20
			if active != want {
				t.Fatalf("guest %d at %.2f: active=%t want=%t pixel=%v", guest, check.at, active, want, out)
			}
		}
	}
	for path, want := range checksums {
		data, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(data) != want {
			t.Fatal("speaker analysis modified an original guest file", err)
		}
	}
}
