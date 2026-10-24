package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneExternallyDeletedRecordings(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := s.Config()
	cfg.RecordingPath = root
	if err = s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(root, "kept.mkv")
	if err = os.WriteFile(kept, []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	unavailable := filepath.Join(t.TempDir(), "unmounted", "missing.mkv")
	if err = s.Update(func(d *Data) error {
		d.Recordings = []Recording{
			{ID: "kept", Path: kept, EndedAt: time.Now()},
			{ID: "deleted", Path: filepath.Join(root, "deleted.mkv"), EndedAt: time.Now()},
			{ID: "deleted-folder", Path: filepath.Join(root, "deleted-folder", "media.mkv"), EndedAt: time.Now()},
			{ID: "active", Path: filepath.Join(root, "active.mkv")},
			{ID: "unavailable", Path: unavailable, EndedAt: time.Now()},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneMissingRecordings(); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if got := s.Data().Recordings; len(got) != 3 || got[0].ID != "kept" || got[1].ID != "active" || got[2].ID != "unavailable" {
		t.Fatal(got)
	}
	reopened, err := Open(s.Directory())
	if err != nil || len(reopened.Data().Recordings) != 3 {
		t.Fatal(err)
	}
	if n, err := s.PruneMissingRecordings(); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
