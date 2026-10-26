package recording

import (
	"path/filepath"
	"testing"
	"time"

	"bigo-live-recorder/internal/state"
)

func TestRecordingPathsStartWithStableIdentity(t *testing.T) {
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	streamer := state.Streamer{ID: "900000001", Name: "Display Name", CustomName: "Custom Name"}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, folders := range []bool{false, true} {
		cfg.FolderPerStreamer = folders
		path := recordingPath(streamer, cfg, stamp, "synthetic")
		want := "900000001_2026-01-02_03-04-05_synthetic.mkv"
		if filepath.Base(path) != want {
			t.Fatal("filename does not use stable identity", filepath.Base(path))
		}
		directory := cfg.RecordingPath
		if folders {
			directory = filepath.Join(directory, "900000001_Custom Name")
		}
		if filepath.Dir(path) != directory {
			t.Fatal("folder does not start with stable identity", filepath.Base(filepath.Dir(path)))
		}
	}
}
