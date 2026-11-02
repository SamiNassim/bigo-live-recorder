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

func TestAudioRoomStaysLiveWithoutRecordingOrImplicitPolling(t *testing.T) {
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
			var checks, starts, inspections atomic.Int64
			var video atomic.Bool
			source := func() bigo.Stream {
				s := sourceFixture()
				if !video.Load() {
					s.HLS, s.RoomStatus = "", 4
				}
				return s
			}
			client := &roomSourceClient{
				presenceSourceClient: presenceSourceClient{
					sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) {
						checks.Add(1)
						s := source()
						if s.HLS == "" {
							return s, bigo.ErrURLWithheld
						}
						return s, nil
					}},
					presence: func(context.Context, string) (bigo.Stream, error) { checks.Add(1); return source(), nil },
				},
				roomKind: func(context.Context, bigo.Stream) (bigo.RoomKind, error) {
					inspections.Add(1)
					return bigo.RoomAudioMultiGuest, nil
				},
			}
			e := newEngine(store, log, client, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
				return testRecording(&starts), nil
			})
			defer e.Close()
			waitFor(t, func() bool { rows := e.Snapshot().Rows; return len(rows) == 1 && rows[0].Status == "Live" })
			row := e.Snapshot().Rows[0]
			if !row.Live || row.RoomKind != bigo.RoomAudioMultiGuest || row.Error != "" || starts.Load() != 0 || inspections.Load() != 1 || len(store.Data().Recordings) != 0 {
				t.Fatal("audio-only room was recorded or marked failed", row)
			}
			e.mu.Lock()
			if e.items["test"].pending || !e.items["test"].retryUntil.IsZero() {
				t.Error("audio-only room scheduled a recording retry")
			}
			e.items["test"].row.NextCheck = time.Now().Add(-time.Hour)
			e.mu.Unlock()
			if e.next() != "" {
				t.Fatal("audio-only room enabled implicit polling")
			}
			e.check("test")
			announcements := 0
			for _, entry := range log.Entries() {
				if strings.Contains(entry.Message, "audio-only multi-guest room") {
					announcements++
					if entry.Level != "INFO" {
						t.Error("room type logged as failure")
					}
				}
			}
			if announcements != 1 || starts.Load() != 0 {
				t.Fatal("repeated check spammed logs or started recording", announcements)
			}
			video.Store(true)
			e.check("test")
			row = e.Snapshot().Rows[0]
			if row.RoomKind != bigo.RoomUnknown || row.Streamer.RecordingPaused != paused {
				t.Fatal("normal video retained an audio-only label or changed pause preference", row)
			}
			want := int64(1)
			if paused {
				want = 0
			}
			if starts.Load() != want {
				t.Fatal("normal video did not resume according to recording preference", starts.Load())
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
