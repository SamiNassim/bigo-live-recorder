package timeline

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceClockFitHandlesWrapDriftAndDelayedPackets(t *testing.T) {
	c := &mediaClock{base: 0xffff1000, buckets: make(map[int64]clockAnchor)}
	const drift = 1.0002
	for i := 0; i < 7200; i++ {
		stamp := c.base + uint32(float64(i)*1000*drift)
		jitter := float64(i%13) * .004
		if i%31 == 0 {
			jitter += 2
		} // A late packet is not a new media clock.
		c.add(stamp, float64(i)+.1+jitter)
	}
	c.fit()
	if math.Abs(c.slope-1/drift) > .00002 {
		t.Fatalf("clock drift was not corrected: %f", c.slope)
	}
	sourceEnd := 7199 * 1000 * drift
	end := c.offset + c.slope*c.source(c.base+uint32(sourceEnd))
	if math.Abs(end-7199.1) > .025 {
		t.Fatalf("clock drift accumulated: %.6f", end)
	}
}

func TestUntimedOriginalPreventsIncompleteReplay(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir, "fixture-room")
	os.WriteFile(filepath.Join(dir, "timed.mka"), []byte("media"), 0644)
	w, _ := j.Create(Entry{File: "timed.mka", UID: 900000001, Epoch: time.Unix(1900000000, 0)}, nil)
	for i := range 50 {
		w.Write(Frame{Received: time.Second + time.Duration(i)*20*time.Millisecond, SourcePTS: uint32(i * 20), SourceDTS: uint32(i * 20), PTS: int64(i * 1800), DTS: int64(i * 1800), Duration: 20 * time.Millisecond})
	}
	w.Close()
	j.Close()
	os.WriteFile(filepath.Join(dir, "older.mka"), []byte("older media"), 0644)
	_, err := Reconcile(context.Background(), dir, func(context.Context, string) ([]TrackInfo, error) {
		return []TrackInfo{{First: 0, End: 1, Packets: 50}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "no timing evidence") {
		t.Fatalf("silently assembled an incomplete broadcast: %v", err)
	}
}

func TestReconcileKeepsSourceTimingAndTrimsPreroll(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, "fixture-room")
	if err != nil {
		t.Fatal(err)
	}
	epoch := time.Unix(1900000000, 0)
	write := func(name string, uid, base uint32, start, end int, latency time.Duration, video bool) {
		t.Helper()
		os.WriteFile(filepath.Join(dir, name), []byte("fixture-media"), 0644)
		w, err := j.Create(Entry{File: name, UID: uid, ID: "guest-example", Epoch: epoch}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := start; i < end; i++ {
			room := time.Duration(i) * 20 * time.Millisecond
			jitter := time.Duration(i%11) * time.Millisecond
			w.Write(Frame{Received: room + latency + jitter, SourcePTS: base + uint32(i*20), SourceDTS: base + uint32(i*20), PTS: int64((i - start) * 1800), DTS: int64((i - start) * 1800), Duration: 20 * time.Millisecond, Video: video})
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write("one.mkv", 900000001, 40000000, 0, 150, 100*time.Millisecond, true)
	write("two.mkv", 900000001, 40000000, 100, 250, 100*time.Millisecond, true)
	write("audio.mka", 900000002, 1700000000, 50, 200, 130*time.Millisecond, false)
	j.Close()
	probe := func(_ context.Context, path string) ([]TrackInfo, error) {
		return []TrackInfo{{Video: !strings.HasSuffix(path, ".mka"), First: 0, End: 3, Packets: 150}}, nil
	}
	p, err := Reconcile(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Clips) != 3 || p.Slots != 2 {
		t.Fatalf("plan=%+v", p)
	}
	var first, second Clip
	for _, c := range p.Clips {
		if c.File == "one.mkv" {
			first = c
		}
		if c.File == "two.mkv" {
			second = c
		}
	}
	if second.Start < first.End-1e-8 || second.Start > first.End+.05 {
		t.Fatalf("overlap/gap: first=%+v second=%+v", first, second)
	}
	if second.FileStart < .9 || second.FileStart > 1.1 {
		t.Fatalf("pre-roll not trimmed in replay: %+v", second)
	}
	if p.Duration < 4.9 || p.Duration > 5.1 {
		t.Fatalf("duration=%f", p.Duration)
	}
}

func TestCancelledReconciliation(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir, "fixture-room")
	j.Append(Entry{Kind: "file", File: "one.mkv", Epoch: time.Now()})
	j.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Reconcile(ctx, dir, func(context.Context, string) ([]TrackInfo, error) { t.Fatal("probed canceled job"); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestReconcileSnapshotSkipsOnlyFinalizedEmptyAttempts(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, "fixture-room")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	epoch := time.Unix(1900000000, 0)
	write := func(name string, at time.Time, empty, closed bool) {
		t.Helper()
		payload := []byte("fixture-media")
		if empty {
			payload = nil
		}
		if err := os.WriteFile(filepath.Join(dir, name), payload, 0644); err != nil {
			t.Fatal(err)
		}
		w, err := j.Create(Entry{File: name, UID: 900000001, ID: "guest.example", Epoch: epoch, At: at}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 50 {
			w.Write(Frame{Received: time.Second + time.Duration(i)*20*time.Millisecond, SourcePTS: uint32(i * 20), SourceDTS: uint32(i * 20), PTS: int64(i * 1800), DTS: int64(i * 1800), Duration: 20 * time.Millisecond})
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if closed {
			if err := j.Append(Entry{Kind: "file_end", File: name, At: at.Add(time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("saved.mka", epoch, false, true)
	write("empty.mka", epoch, true, true)
	write("new-session.mka", epoch.Add(time.Minute), true, false)
	calls := 0
	probe := func(_ context.Context, path string) ([]TrackInfo, error) {
		calls++
		if filepath.Base(path) != "saved.mka" {
			t.Fatal("probed an empty attempt or a newer active session", path)
		}
		return []TrackInfo{{First: 0, End: 1, Packets: 50}}, nil
	}
	p, err := ReconcileThrough(context.Background(), dir, probe, epoch.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(p.Files) != 1 || len(p.Clips) != 1 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "empty.mka") {
		t.Fatal("incorrect capture snapshot", p)
	}
	if info, err := os.Stat(filepath.Join(dir, "empty.mka")); err != nil || info.Size() != 0 {
		t.Fatal("empty original was changed or removed", err)
	}
	_, err = ReconcileThrough(context.Background(), dir, probe, epoch.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "still being finalized") {
		t.Fatal("accepted an unfinished empty recording", err)
	}
}
