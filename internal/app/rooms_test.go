package app

import (
	"testing"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestAudioMultiGuestRoomIsVisibleAsLive(t *testing.T) {
	a := accountFixture(t)
	a.snapshot.Listener.Connected = true
	a.snapshot.Rows = []monitor.Row{{Streamer: state.Streamer{ID: "demo-room"}, Status: "Live", Live: true, RoomKind: bigo.RoomAudioMultiGuest}}
	tt := ui.NewTester(a.view, 1050, 650)
	for _, text := range []string{"Audio-only multi-guest room", "●  Live (last check)", "Listening"} {
		if !tt.HasText(text) {
			t.Fatal("room type or monitoring status missing", text, tt.Texts())
		}
	}
	a.status = "Live"
	tt.Frame()
	if !tt.HasText("demo-room") {
		t.Fatal("audio-only room was excluded from live streamers")
	}
	a.snapshot.Rows[0].Streamer.RecordingPaused = true
	tt.Frame()
	if !tt.HasText("Recording paused") || !tt.HasText("Audio-only multi-guest room") {
		t.Fatal("room type hid the independent recording preference")
	}
	a.snapshot.Rows[0].RoomKind = bigo.RoomUnknown
	a.snapshot.Rows[0].Streamer.RecordingPaused = false
	a.snapshot.Rows[0].Status = "Recording"
	tt.Frame()
	if tt.HasText("Audio-only multi-guest room") || !tt.HasText("●  Recording") {
		t.Fatal("room type was not cleared for video recording")
	}
}
