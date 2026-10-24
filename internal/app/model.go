package app

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/monitor"
)

func filteredRows(rows []monitor.Row, query, status, priority, order string) []monitor.Row {
	out := make([]monitor.Row, 0, len(rows))
	query = strings.ToLower(strings.TrimSpace(query))
	for _, r := range rows {
		if query != "" && !strings.Contains(strings.ToLower(r.Streamer.DisplayName()+" "+r.Streamer.ID), query) {
			continue
		}
		if !matchesStatus(r, status) {
			continue
		}
		if priority == "Normal priority" && r.Streamer.LowPriority || priority == "Low priority" && !r.Streamer.LowPriority {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b monitor.Row) int {
		var n int
		switch order {
		case "Name":
			n = strings.Compare(strings.ToLower(a.Streamer.DisplayName()), strings.ToLower(b.Streamer.DisplayName()))
		case "Date added":
			n = b.Streamer.AddedAt.Compare(a.Streamer.AddedAt)
		case "Status":
			n = strings.Compare(a.Status, b.Status)
		default:
			n = b.Streamer.LastRecordedAt.Compare(a.Streamer.LastRecordedAt)
		}
		if n == 0 {
			n = cmp.Compare(a.Streamer.ID, b.Streamer.ID)
		}
		return n
	})
	return out
}

func matchesStatus(r monitor.Row, status string) bool {
	switch status {
	case "All statuses":
		return true
	case "Recording paused":
		return r.Streamer.RecordingPaused
	case "Monitoring paused":
		return r.Streamer.Paused
	case "Live":
		return r.Live && !r.Streamer.Paused
	default:
		return status == r.Status
	}
}

// Filter before virtualization, never after an arbitrary unfiltered tail limit.
func filteredLogs(entries []logging.Entry, query, level string) []logging.Entry {
	out := make([]logging.Entry, 0, len(entries))
	query = strings.ToLower(strings.TrimSpace(query))
	for _, e := range entries {
		if level != "All levels" && e.Level != level {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(e.Streamer+" "+e.Message), query) {
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b logging.Entry) int {
		if n := b.Time.Compare(a.Time); n != 0 {
			return n
		}
		return cmp.Compare(b.Sequence, a.Sequence)
	})
	return out
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("Jan 02, 15:04:05")
}
func duration(d time.Duration) string {
	s := max(int(d.Seconds()), 0)
	if s >= 3600 {
		return fmt.Sprintf("%dh %02dm", s/3600, (s/60)%60)
	}
	if s >= 60 {
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}
func bytesLabel(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
