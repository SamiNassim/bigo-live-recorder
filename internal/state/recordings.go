package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// PruneMissingRecordings reconciles finalized history with external deletions.
// It never deletes media. Active files, permission errors and unavailable
// recording folders are preserved, including recordings on disconnected disks.
func (s *Store) PruneMissingRecordings() (int, error) {
	data, root := s.Data(), s.Config().RecordingPath
	available := make(map[string]bool)
	directoryAvailable := func(path string) bool {
		if ok, seen := available[path]; seen {
			return ok
		}
		info, err := os.Stat(path)
		ok := err == nil && info.IsDir()
		available[path] = ok
		return ok
	}
	missing := make(map[string]string)
	for _, r := range data.Recordings {
		if r.EndedAt.IsZero() || r.Path == "" {
			continue
		}
		if _, err := os.Stat(r.Path); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		// A missing per-streamer directory is a deletion only when its known
		// recording root is still mounted. Older recording roots need their
		// immediate parent to be available before they can be reconciled.
		rel, err := filepath.Rel(root, r.Path)
		underRoot := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
		if directoryAvailable(filepath.Dir(r.Path)) || underRoot && directoryAvailable(root) {
			missing[r.ID] = r.Path
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	removed := 0
	err := s.Update(func(d *Data) error {
		before := len(d.Recordings)
		d.Recordings = slices.DeleteFunc(d.Recordings, func(r Recording) bool {
			return !r.EndedAt.IsZero() && missing[r.ID] == r.Path && r.Path != ""
		})
		removed = before - len(d.Recordings)
		return nil
	})
	return removed, err
}
