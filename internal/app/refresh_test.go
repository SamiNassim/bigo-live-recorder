package app

import (
	"testing"
	"time"

	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestOnlyVisibleLiveClocksKeepRequestingFrames(t *testing.T) {
	a := accountFixture(t)
	a.snapshot.Rows = []monitor.Row{{Streamer: state.Streamer{ID: "synthetic"}, Status: "Recording", StartedAt: time.Now().Add(-time.Minute)}}
	frames := 0
	tt := ui.NewTester(func(c *ui.Context) { frames++; a.view(c) }, 1050, 650)
	before := frames
	tt.Advance(3 * time.Second)
	if frames <= before {
		t.Fatal("visible recording duration stopped refreshing")
	}
	a.page = "settings"
	tt.Frame()
	before = frames
	tt.Advance(3 * time.Second)
	if frames != before {
		t.Fatal("an inactive page kept redrawing the interface", frames-before)
	}
	a.page = "recordings"
	a.data.Recordings = []state.Recording{{ID: "synthetic", StartedAt: time.Now().Add(-time.Minute)}}
	tt.Frame()
	before = frames
	tt.Advance(3 * time.Second)
	if frames <= before {
		t.Fatal("active recording history duration stopped refreshing")
	}
	a.data.Recordings[0].EndedAt = time.Now()
	tt.Frame()
	// A frame may already be armed by the previously active recording.
	tt.Advance(2 * time.Second)
	before = frames
	tt.Advance(3 * time.Second)
	if frames != before {
		t.Fatal("finished recording history kept redrawing", frames-before)
	}
}
