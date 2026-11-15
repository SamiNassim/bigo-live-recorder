package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// RemoveRecording removes one finalized history entry, never its media file.
// Active entries are retained so finalization can still update their metadata.
func (s *Store) RemoveRecording(id string) error {
	return s.Update(func(d *Data) error {
		i := slices.IndexFunc(d.Recordings, func(r Recording) bool { return r.ID == id })
		if i < 0 {
			return errors.New("recording is no longer in history")
		}
		if d.Recordings[i].EndedAt.IsZero() {
			return errors.New("active recordings remain in history until finalized")
		}
		d.Recordings = slices.Delete(d.Recordings, i, i+1)
		return nil
	})
}

// ClearRecordingHistory forgets finalized recordings without deleting media.
// The update is serialized with recording starts and finalization.
func (s *Store) ClearRecordingHistory() (int, error) {
	removed := 0
	err := s.Update(func(d *Data) error {
		before := len(d.Recordings)
		d.Recordings = slices.DeleteFunc(d.Recordings, func(r Recording) bool { return !r.EndedAt.IsZero() })
		removed = before - len(d.Recordings)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// PruneMissingRecordings reconciles finalized history with external deletions.
// It never deletes media. Active files, permission errors and unavailable
// recording folders are preserved, including recordings on disconnected disks.
func (s *Store) PruneMissingRecordings() (int, error) {
	data := s.Data()
	isMissing := missingFromAvailableFolder(s.Config().RecordingPath)
	missing := make(map[string]string)
	for _, r := range data.Recordings {
		if !r.EndedAt.IsZero() && isMissing(r.Path) {
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

func missingFromAvailableFolder(root string) func(string) bool {
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
	return func(path string) bool {
		if path == "" || filepath.Clean(path) == filepath.Clean(root) {
			return false
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
		// A missing per-streamer directory is a deletion only when its known
		// recording root is still mounted. Older recording roots need their
		// immediate parent to be available before they can be reconciled.
		rel, err := filepath.Rel(root, path)
		underRoot := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
		return directoryAvailable(filepath.Dir(path)) || underRoot && directoryAvailable(root)
	}
}

// MissingBroadcasts finds unreferenced broadcasts with no accessible media
// left at their recorded locations. Callers protect captures still in use.
func (s *Store) MissingBroadcasts() []Broadcast {
	data := s.Data()
	referenced := recordingBroadcasts(data.Recordings)
	isMissing := missingFromAvailableFolder(s.Config().RecordingPath)
	var missing []Broadcast
	for _, b := range data.Broadcasts {
		if !referenced[b.ID] && !b.ReplayPending && isMissing(b.Directory) &&
			(b.ReplayPath == "" || isMissing(b.ReplayPath)) {
			missing = append(missing, b)
		}
	}
	return missing
}

// RemoveMissingBroadcasts forgets candidates that still match the scanned
// snapshot and have no recording references. Media is never deleted.
func (s *Store) RemoveMissingBroadcasts(missing []Broadcast) error {
	if len(missing) == 0 {
		return nil
	}
	candidates := make(map[string]Broadcast, len(missing))
	for _, b := range missing {
		candidates[b.ID] = b
	}
	return s.Update(func(d *Data) error {
		referenced := recordingBroadcasts(d.Recordings)
		d.Broadcasts = slices.DeleteFunc(d.Broadcasts, func(b Broadcast) bool {
			candidate, ok := candidates[b.ID]
			return ok && candidate == b && !referenced[b.ID] && !b.ReplayPending
		})
		return nil
	})
}

func recordingBroadcasts(recordings []Recording) map[string]bool {
	referenced := make(map[string]bool)
	for _, r := range recordings {
		if r.BroadcastID != "" {
			referenced[r.BroadcastID] = true
		}
	}
	return referenced
}
