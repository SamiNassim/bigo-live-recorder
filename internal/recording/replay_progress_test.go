package recording

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReplayCommandStreamsProgressAndCancelsWithoutWaitingForCompletion(t *testing.T) {
	ff, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	measured := false
	err = runReplayCommand(ctx, ff, t.TempDir(), []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-re", "-f", "lavfi",
		"-i", "color=c=black:s=16x16:r=25", "-t", "3", "-f", "null", "-",
	}, func(p replayCommandProgress) {
		if p.frames > 0 && p.frames < 75 && p.microseconds > 0 {
			measured = true
			cancel()
		}
	})
	if !measured || !errors.Is(err, context.Canceled) {
		t.Fatal("progress was buffered until completion or cancellation was lost", measured, err)
	}
}

func TestReplayProgressReadsCompleteRecordsAndIgnoresInvalidTimes(t *testing.T) {
	input := "frame= 25\nout_time_us=500000\nout_time_ms=500000\nspeed=2.0x\nprogress=continue\n" +
		"frame=N/A\nout_time_us=-9223372036854775808\nprogress=continue\n" +
		"frame=50\nout_time_us=1000000\nprogress=end\nframe=900\n"
	var got []replayCommandProgress
	readReplayProgress(strings.NewReader(input), func(p replayCommandProgress) { got = append(got, p) })
	want := []replayCommandProgress{{25, 500000}, {-1, -1}, {50, 1000000}}
	if !slices.Equal(got, want) {
		t.Fatal("incorrect progress records", got)
	}
	var progress []ReplayProgress
	report := replayTimeProgress(func(p ReplayProgress) { progress = append(progress, p) }, "Joining", 2)
	for _, p := range got {
		report(p)
	}
	if !slices.Equal(progress, []ReplayProgress{{"Joining", 25}, {"Joining", 50}}) {
		t.Fatal("invalid time was used or microseconds were misread", progress)
	}
}

func TestReplayProgressNeverClaimsCompletionFromTelemetry(t *testing.T) {
	for _, tc := range []struct {
		done, total float64
		want        int
	}{{0, 100, 0}, {42.9, 100, 42}, {100, 100, 99}, {101, 100, 99}, {-1, 100, 0}, {1, 0, 0}, {math.NaN(), 100, 0}} {
		if got := replayPercent(tc.done, tc.total); got != tc.want {
			t.Fatalf("progress(%v, %v) = %d, want %d", tc.done, tc.total, got, tc.want)
		}
	}
	// A malformed, oversized line must not prevent FFmpeg from draining stdout.
	input := strings.NewReader(strings.Repeat("x", 1<<17) + "\nprogress=end\n")
	readReplayProgress(input, func(replayCommandProgress) { t.Error("accepted malformed telemetry") })
	if input.Len() != 0 {
		t.Fatal("stopped draining progress output")
	}
}
