package state

import (
	"encoding/json"
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
	if r.Config().NativeDeviceID == "" || r.Config().NativeDeviceID != c.NativeDeviceID {
		t.Fatal("listener device identity was not persisted")
	}
	d := r.Data()
	d.Streamers[0].ID = "mutated"
	if r.Data().Streamers[0].ID != "alice" {
		t.Fatal("snapshot aliases state")
	}
}

func TestLegacyNativeSessionIsRemovedWithoutLosingCookie(t *testing.T) {
	dir := t.TempDir()
	c := Defaults()
	c.Cookie = "valid-web-cookie"
	c.Theme = "dark"
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "native_device_id")
	fields["native_session"] = "obsolete-captured-account-secret"
	raw, _ = json.Marshal(fields)
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Config(); got.Cookie != c.Cookie || got.Theme != c.Theme || got.NativeDeviceID == "" || !got.NativeListening || got.PollingBackup {
		t.Fatalf("migration changed account/preferences: %+v", got)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var migrated map[string]json.RawMessage
	if err = json.Unmarshal(raw, &migrated); err != nil {
		t.Fatal(err)
	}
	if _, exists := migrated["native_session"]; exists {
		t.Fatal("obsolete native credential remains on disk")
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

func TestWindowStateDoesNotOverwritePreferences(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	draft := s.Config()
	w := Window{X: 40, Y: 60, Width: 1300, Height: 850}
	if err = s.SaveWindow(w); err != nil {
		t.Fatal(err)
	}
	draft.Theme = "dark"
	if err = s.SaveConfig(draft); err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	if r.Config().Theme != "dark" || r.Config().Window != w {
		t.Fatalf("lost window or preference: %+v", r.Config())
	}
}
