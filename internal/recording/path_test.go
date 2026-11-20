package recording

import (
	"path/filepath"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
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

func TestSplitRecordingFoldersAreIndependentOfStreamerFolders(t *testing.T) {
	streamer := state.Streamer{ID: "900000001", Name: "Demo"}
	source := bigo.Stream{UID: 900000001, RoomID: "fixture-room"}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, test := range []struct {
		name            string
		streamer, split bool
	}{
		{"flat", false, false},
		{"streamer only", true, false},
		{"broadcast only", false, true},
		{"streamer and broadcast", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := state.Defaults()
			cfg.RecordingPath = t.TempDir()
			cfg.SplitMinutes = 30
			cfg.FolderPerStreamer, cfg.FolderPerSplitRecording = test.streamer, test.split
			want := cfg.RecordingPath
			if test.streamer {
				want = filepath.Join(want, "900000001_Demo")
			}
			guestDirectory := filepath.Join(want, "900000001_2026-01-02_03-04-05_room-fixture-room")
			if test.split {
				want = filepath.Join(want, "900000001_2026-01-02_03-04-05_stream-fixture-room")
			}
			broadcast := NewSegmentedBroadcast(streamer, source, cfg, stamp)
			if broadcast.Directory != want || !broadcast.Segmented || broadcast.SharedDirectory != !test.split || broadcast.ID != `split:"900000001":"fixture-room"` {
				t.Fatal("split folder preference changed the directory or broadcast identity", broadcast)
			}
			if guest := NewBroadcast(streamer, source, cfg, stamp); guest.Directory != guestDirectory {
				t.Fatal("split folder preference changed the multi-guest folder", guest)
			}
		})
	}
}
