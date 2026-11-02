package monitor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/state"
)

type roomSourceClient struct {
	presenceSourceClient
	roomKind func(context.Context, bigo.Stream) (bigo.RoomKind, error)
}

func (c *roomSourceClient) RoomKind(ctx context.Context, source bigo.Stream, _ *bigolistener.Hub) (bigo.RoomKind, error) {
	return c.roomKind(ctx, source)
}

func TestAudioRoomRecordsUnlessPausedWithoutImplicitPolling(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "recording enabled", true: "recording paused"}[paused], func(t *testing.T) {
			store, log := fixture(t, true, false)
			cfg := store.Config()
			cfg.Cookie = "synthetic-cookie"
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if err := store.Update(func(d *state.Data) error { d.Streamers[0].RecordingPaused = paused; return nil }); err != nil {
				t.Fatal(err)
			}
			var starts, inspections atomic.Int64
			source := sourceFixture()
			source.HLS = ""
			source.RoomStatus = 4
			client := &roomSourceClient{presenceSourceClient: presenceSourceClient{sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) { return source, bigo.ErrURLWithheld }}, presence: func(context.Context, string) (bigo.Stream, error) { return source, nil }}, roomKind: func(context.Context, bigo.Stream) (bigo.RoomKind, error) {
				inspections.Add(1)
				return bigo.RoomAudioMultiGuest, nil
			}}
			e := newEngine(store, log, client, func(_ context.Context, _ state.Streamer, s bigo.Stream, _ state.Config) (activeRecording, error) {
				if s.RoomKind != bigo.RoomAudioMultiGuest {
					t.Error("lost room subtype")
				}
				return testRecording(&starts), nil
			})
			defer e.Close()
			wantStatus := "Recording"
			wantStarts := int64(1)
			if paused {
				wantStatus = "Live"
				wantStarts = 0
			}
			waitFor(t, func() bool { rows := e.Snapshot().Rows; return len(rows) == 1 && rows[0].Status == wantStatus })
			row := e.Snapshot().Rows[0]
			if !row.Live || row.RoomKind != bigo.RoomAudioMultiGuest || row.Error != "" || starts.Load() != wantStarts || inspections.Load() != 1 {
				t.Fatal("unexpected audio-room recording state", row)
			}
			e.mu.Lock()
			e.items["test"].row.NextCheck = time.Now().Add(-time.Hour)
			e.mu.Unlock()
			if e.next() != "" {
				t.Fatal("audio room enabled implicit polling")
			}
			for _, entry := range log.Entries() {
				if strings.Contains(entry.Message, "skipped") {
					t.Fatal("audio recording still described as skipped")
				}
			}
		})
	}
}

func TestUnidentifiedAppRoomKeepsNormalRecordingPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		native  bool
		roomErr error
	}{
		{"camera-capable", true, nil}, {"metadata unavailable", true, errors.New("unavailable")}, {"polling only", false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, log := fixture(t, test.native, false)
			cfg := store.Config()
			cfg.Cookie = "synthetic-cookie"
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			var starts, inspections atomic.Int64
			source := sourceFixture()
			source.RoomStatus = 4
			client := &roomSourceClient{presenceSourceClient: presenceSourceClient{sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) { return source, nil }}}, roomKind: func(context.Context, bigo.Stream) (bigo.RoomKind, error) {
				inspections.Add(1)
				return bigo.RoomUnknown, test.roomErr
			}}
			e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
				return testRecording(&starts), nil
			})
			defer e.Close()
			waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
			if starts.Load() != 1 || (!test.native && inspections.Load() != 0) {
				t.Fatal("room lookup altered normal recording or polling policy")
			}
		})
	}
}
