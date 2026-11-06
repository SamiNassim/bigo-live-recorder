package recording

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"
)

// ReplayProgress measures the current stage, not elapsed or remaining time.
// Percent is -1 when the stage has no measurable total.
type ReplayProgress struct {
	Stage   string
	Percent int
}

type replayCommandProgress struct {
	frames, microseconds int64
}

func readReplayProgress(r io.Reader, report func(replayCommandProgress)) {
	scanner := bufio.NewScanner(r)
	p := replayCommandProgress{-1, -1}
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		if key == "progress" {
			if value == "continue" || value == "end" {
				report(p)
			}
			p = replayCommandProgress{-1, -1}
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch key {
		case "frame":
			p.frames = n
		case "out_time_us":
			p.microseconds = n
		}
	}
	// Progress is advisory. Malformed telemetry must not block or fail an
	// otherwise successful export, even if a line exceeds the scanner limit.
	if scanner.Err() != nil {
		io.Copy(io.Discard, r)
	}
}

// Completion comes from successful commands and file validation, never from
// progress=end: FFmpeg can emit that record before exiting with an error.
func replayPercent(done, total float64) int {
	if total <= 0 || math.IsNaN(done) || math.IsNaN(total) {
		return 0
	}
	return int(min(99, max(0, math.Floor(done/total*100))))
}

func replayTimeProgress(report func(ReplayProgress), stage string, duration float64) func(replayCommandProgress) {
	return func(p replayCommandProgress) {
		if p.microseconds >= 0 {
			report(ReplayProgress{stage, replayPercent(float64(p.microseconds)/1e6, duration)})
		}
	}
}
