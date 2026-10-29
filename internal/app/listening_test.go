package app

import (
	"testing"
	"time"

	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestNativeNextCheckShowsListeningOnlyForActiveMonitoring(t *testing.T) {
	for _, test := range []struct {
		name, status                                          string
		native, connected, paused, recordingPaused, scheduled bool
		want                                                  string
	}{
		{name: "connected", native: true, connected: true, want: "Listening"},
		{name: "recording", status: "Recording", native: true, connected: true, want: "Listening"},
		{name: "recording paused", native: true, connected: true, recordingPaused: true, want: "Listening"},
		{name: "monitoring paused", native: true, connected: true, paused: true, want: "—"},
		{name: "disconnected", native: true, want: "—"},
		{name: "polling only", connected: true, want: "—"},
		{name: "scheduled check", native: true, connected: true, scheduled: true, want: "In "},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := accountFixture(t)
			cfg := a.store.Config()
			cfg.NativeListening = test.native
			if err := a.store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			a.snapshot.Listener = bigolistener.RuntimeStatus{Connected: test.connected}
			row := monitor.Row{Streamer: state.Streamer{ID: "fixture", Paused: test.paused, RecordingPaused: test.recordingPaused}, Status: test.status}
			if test.scheduled {
				row.NextCheck = time.Now().Add(30 * time.Second)
			}
			a.snapshot.Rows = []monitor.Row{row}
			tt := ui.NewTester(a.view, 1050, 650)
			if !tt.HasText(test.want) || tt.HasText("Listening") != (test.want == "Listening") {
				t.Fatal("incorrect Next check state", tt.Texts())
			}
			// A settings draft must not change the status of the running listener.
			a.cfg.NativeListening = !test.native
			tt.Frame()
			if tt.HasText("Listening") != (test.want == "Listening") {
				t.Fatal("an unsaved preference changed the monitoring status")
			}
		})
	}
}

func TestListeningStateDoesNotScheduleClockRedraws(t *testing.T) {
	a := accountFixture(t)
	a.snapshot.Listener.Connected = true
	a.snapshot.Rows = []monitor.Row{{Streamer: state.Streamer{ID: "fixture"}, Status: "Offline"}}
	frames := 0
	tt := ui.NewTester(func(c *ui.Context) { frames++; a.view(c) }, 1050, 650)
	before := frames
	tt.Advance(5 * time.Second)
	if frames != before || !tt.HasText("Listening") {
		t.Fatal("Listening scheduled unnecessary redraws or disappeared")
	}
}
