package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDefaultsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Config().NativeListening || s.Config().PollingBackup {
		t.Fatal("native must be the default without silent polling")
	}
	c := s.Config()
	c.Cookie = "temporary-cookie"
	if err = s.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(d *Data) error { d.Streamers = append(d.Streamers, Streamer{ID: "alice"}); return nil }); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Config().Cookie != c.Cookie || len(r.Data().Streamers) != 1 {
		t.Fatal("snapshot lost")
	}
	d := r.Data()
	d.Streamers[0].ID = "mutated"
	if r.Data().Streamers[0].ID != "alice" {
		t.Fatal("snapshot aliases state")
	}
}
func TestMalformedFileIsPreserved(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "config.json")
	raw := []byte("{broken")
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("accepted corrupt config")
	}
	b, _ := os.ReadFile(name)
	if string(b) != string(raw) {
		t.Fatal("overwrote corrupt config")
	}
}
func TestConcurrentAtomicUpdates(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.Update(func(d *Data) error { d.Streamers = append(d.Streamers, Streamer{ID: "x"}); return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(s.Data().Streamers) != 20 {
		t.Fatal("lost concurrent updates")
	}
	if _, err = Open(s.Directory()); err != nil {
		t.Fatal(err)
	}
}
