package app

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
)

func tableFixture() *application {
	now := time.Unix(1700000000, 0)
	return &application{
		status: "All statuses", priority: "All priorities", order: "Last recorded", level: "All levels",
		snapshot: monitor.Snapshot{Rows: []monitor.Row{
			{Streamer: state.Streamer{ID: "alpha", Name: "Alpha", LastRecordedAt: now}, Status: "Offline"},
			{Streamer: state.Streamer{ID: "beta", Name: "Beta", LastRecordedAt: now.Add(time.Minute)}, Status: "Recording"},
		}},
		logs: []logging.Entry{
			{Sequence: 1, Time: now, Level: "WARN", Message: "Earlier warning"},
			{Sequence: 2, Time: now.Add(time.Minute), Level: "INFO", Message: "Later event"},
		},
		data: state.Data{Recordings: []state.Recording{
			{ID: "older", Path: "older.mkv", StartedAt: now},
			{ID: "newer", Path: "newer.mkv", StartedAt: now.Add(time.Minute)},
		}},
	}
}

func TestTableViewsReuseUnchangedRowsAcrossNavigation(t *testing.T) {
	a := tableFixture()
	streamers, logs, recordings := a.streamerRows(), a.logRows(), a.recordingRows()
	for _, page := range []string{"settings", "logs", "recordings", "streamers"} {
		a.page = page
		if &a.streamerRows()[0] != &streamers[0] || &a.logRows()[0] != &logs[0] || &a.recordingRows()[0] != &recordings[0] {
			t.Fatal("navigation rebuilt an unchanged table")
		}
	}
	if count := testing.AllocsPerRun(100, func() { a.streamerRows(); a.logRows(); a.recordingRows() }); count != 0 {
		t.Fatalf("unchanged table lookup allocates %.1f objects", count)
	}
}

func TestTableViewsObserveFiltersEditsAndRemovedRows(t *testing.T) {
	a := tableFixture()
	if a.streamerRows()[0].Streamer.ID != "beta" || a.recordingRows()[0].ID != "newer" || a.logRows()[0].Sequence != 2 {
		t.Fatal("initial ordering is incorrect")
	}
	a.order = "Name"
	if a.streamerRows()[0].Streamer.ID != "alpha" {
		t.Fatal("changing sort order reused a stale view")
	}
	a.snapshot.Rows[0].Streamer.Name = "Zulu"
	if a.streamerRows()[0].Streamer.ID != "beta" {
		t.Fatal("an in-place rename did not reorder the table")
	}
	a.query = "Zulu"
	if rows := a.streamerRows(); len(rows) != 1 || rows[0].Streamer.ID != "alpha" {
		t.Fatal("search did not use the edited name")
	}
	a.snapshot.Rows[0].Streamer.Paused = true
	a.status = "Monitoring paused"
	if len(a.streamerRows()) != 1 {
		t.Fatal("monitoring changes did not update the filtered view")
	}
	a.priority = "Low priority"
	if len(a.streamerRows()) != 0 {
		t.Fatal("priority filtering reused stale rows")
	}
	a.level = "WARN"
	if entries := a.logRows(); len(entries) != 1 || entries[0].Sequence != 1 {
		t.Fatal("warning filtering reused the all-level view")
	}
	a.logs = append(a.logs, logging.Entry{Sequence: 3, Time: a.logs[1].Time.Add(time.Minute), Level: "WARN", Message: "New warning"})
	if a.logRows()[0].Sequence != 3 {
		t.Fatal("new logs were not shown first")
	}
	a.logQuery = "earlier"
	if entries := a.logRows(); len(entries) != 1 || entries[0].Sequence != 1 {
		t.Fatal("log search reused stale results")
	}
	a.data.Recordings[1].ProcessedPath = "newer.processed.mkv"
	a.data.Recordings[1].EndedAt = time.Unix(1700000200, 0)
	if recording := a.recordingRows()[0]; recording.ProcessedPath == "" || recording.EndedAt.IsZero() {
		t.Fatal("recording finalization or processing updates were hidden")
	}
	a.snapshot.Rows, a.logs, a.data.Recordings = nil, nil, nil
	if len(a.streamerRows()) != 0 || len(a.logRows()) != 0 || len(a.recordingRows()) != 0 {
		t.Fatal("clearing a list retained removed rows")
	}
}

func BenchmarkTableViews(b *testing.B) {
	a := tableFixture()
	for i := range 5000 {
		a.logs = append(a.logs, logging.Entry{Sequence: uint64(i + 3), Time: time.Unix(int64(i), 0), Level: "INFO", Message: fmt.Sprintf("Synthetic event %d", i)})
	}
	for i := range 2000 {
		a.data.Recordings = append(a.data.Recordings, state.Recording{ID: fmt.Sprint(i), StartedAt: time.Unix(int64(i), 0)})
	}
	a.streamerRows()
	a.logRows()
	a.recordingRows()
	b.Run("rebuild", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			filteredRows(a.snapshot.Rows, a.query, a.status, a.priority, a.order)
			filteredLogs(a.logs, a.logQuery, a.level)
			rows := slices.Clone(a.data.Recordings)
			slices.SortFunc(rows, func(a, b state.Recording) int { return b.StartedAt.Compare(a.StartedAt) })
		}
	})
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			a.streamerRows()
			a.logRows()
			a.recordingRows()
		}
	})
}
