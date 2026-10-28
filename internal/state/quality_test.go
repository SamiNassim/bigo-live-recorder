package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingSettingsAdoptOriginWithoutChangingPreferences(t *testing.T) {
	dir := t.TempDir()
	cfg := Defaults()
	cfg.Cookie = "synthetic-cookie"
	cfg.NativeListening = false
	cfg.PollingBackup = true
	cfg.PostProcess = true
	cfg.OutputExtension = "mp4"
	cfg.FFmpegArgs = "-c copy -movflags +faststart"
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "stream_quality")
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Config(); got != cfg {
		t.Fatal("loading the new quality default changed existing preferences")
	}
	cfg.StreamQuality = "auto"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Config(); got != cfg {
		t.Fatal("the saved Web preference was lost after reopening")
	}
}
