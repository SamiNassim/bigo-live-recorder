package app

import (
	"cmp"
	"slices"

	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
)

// Each table retains one derived view. Compare values as well as filters so
// edits within an existing source slice cannot leave stale rows or menus.
type tableCache[T comparable] struct {
	source []T
	rows   []T
	key    [4]string
	valid  bool
}

func (cache *tableCache[T]) invalidate(source []T) bool {
	if cache.valid && !slices.Equal(cache.source, source) {
		*cache = tableCache[T]{}
		return true
	}
	return false
}

func (cache *tableCache[T]) get(source []T, key [4]string, build func([]T) []T) []T {
	changed := !cache.valid || !slices.Equal(cache.source, source)
	if changed || cache.key != key {
		if changed {
			cache.source = slices.Clone(source)
		}
		cache.rows = build(source)
		cache.key, cache.valid = key, true
	}
	return cache.rows
}

func (a *application) streamerRows() []monitor.Row {
	return a.streamRows.get(a.snapshot.Rows, [4]string{a.query, a.status, a.priority, a.order}, func(rows []monitor.Row) []monitor.Row {
		return filteredRows(rows, a.query, a.status, a.priority, a.order)
	})
}

func (a *application) logRows() []logging.Entry {
	return a.logRowsCache.get(a.logs, [4]string{a.logQuery, a.level}, func(entries []logging.Entry) []logging.Entry {
		return filteredLogs(entries, a.logQuery, a.level)
	})
}

func (a *application) recordingRows() []state.Recording {
	return a.recordRows.get(a.data.Recordings, [4]string{}, func(source []state.Recording) []state.Recording {
		rows := slices.Clone(source)
		slices.SortFunc(rows, func(a, b state.Recording) int {
			if order := b.StartedAt.Compare(a.StartedAt); order != 0 {
				return order
			}
			return cmp.Compare(a.ID, b.ID)
		})
		return rows
	})
}
