package state

import (
	"errors"
	"testing"
)

func TestDataSnapshotsTrackOnlySuccessfulDataChanges(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, revision := s.DataSince(0)
	if revision == 0 {
		t.Fatal("initial snapshot has no revision")
	}
	cfg := s.Config()
	cfg.Notifications = !cfg.Notifications
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWindow(Window{Width: 1200, Height: 800}); err != nil {
		t.Fatal(err)
	}
	if s.DataRevision() != revision {
		t.Fatal("config or window writes invalidated data")
	}
	if err := s.Update(func(d *Data) error {
		d.Streamers = append(d.Streamers, Streamer{ID: "synthetic"})
		return errors.New("rejected")
	}); err == nil {
		t.Fatal("failed update succeeded")
	}
	if s.DataRevision() != revision {
		t.Fatal("failed update invalidated data")
	}
	if err := s.Update(func(d *Data) error { d.Streamers = append(d.Streamers, Streamer{ID: "synthetic"}); return nil }); err != nil {
		t.Fatal(err)
	}
	data, next := s.DataSince(revision)
	if next <= revision || len(data.Streamers) != 1 {
		t.Fatal("successful update was missed")
	}
	data.Streamers[0].ID = "edited"
	if s.Data().Streamers[0].ID != "synthetic" {
		t.Fatal("snapshot aliases mutable store data")
	}
	if n := testing.AllocsPerRun(100, func() { s.DataSince(next) }); n != 0 {
		t.Fatal("unchanged snapshots allocate", n)
	}
}
