package monitor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/state"
)

type presenceSourceClient struct {
	sourceClient
	presence func(context.Context, string) (bigo.Stream, error)
}

func (c *presenceSourceClient) Presence(ctx context.Context, id string) (bigo.Stream, error) {
	return c.presence(ctx, id)
}

func webOfflineFixture() bigo.Stream {
	return bigo.Stream{UID: 123, RoomID: "0"}
}

func TestNativePlaybackInfoResolvesAppOnlyBroadcast(t *testing.T) {
	var webCalls, presenceCalls int
	presence := sourceFixture()
	presence.HLS, presence.Avatar = "", "https://images.example/avatar.jpg"
	client := &presenceSourceClient{
		sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) {
			webCalls++
			return webOfflineFixture(), nil
		}},
		presence: func(context.Context, string) (bigo.Stream, error) {
			presenceCalls++
			return presence, nil
		},
	}
	e := &Engine{client: client}
	source, err := e.playbackInfo(context.Background(), state.Streamer{ID: "demo"}, state.Defaults())
	if err != nil || source.Alive != 1 || source.RoomID != presence.RoomID || source.UID != presence.UID || source.Name != presence.Name || source.Avatar != presence.Avatar {
		t.Fatal("web offline status hid the native broadcast", source, err)
	}
	if webCalls != 1 || presenceCalls != 1 || source.URL() != "" {
		t.Fatal("unexpected requests or invented web playback", webCalls, presenceCalls, source)
	}
}

func TestPlaybackInfoPreservesWorkingWebAndOtherMonitoringModes(t *testing.T) {
	for _, test := range []struct {
		name     string
		web      bigo.Stream
		err      error
		native   bool
		quality  string
		override string
	}{
		{name: "working web room", web: sourceFixture(), native: true, quality: "origin"},
		{name: "withheld URL with valid room", web: bigo.Stream{Alive: 1, UID: 123, RoomID: "8589934634"}, err: bigo.ErrURLWithheld, native: true, quality: "origin"},
		{name: "polling only", web: webOfflineFixture(), quality: "origin"},
		{name: "web quality", web: webOfflineFixture(), native: true, quality: "auto"},
		{name: "streamer web override", web: webOfflineFixture(), native: true, quality: "origin", override: "auto"},
		{name: "invalid session", web: webOfflineFixture(), err: bigo.ErrLoginRequired, native: true, quality: "origin"},
		{name: "rate limit", web: webOfflineFixture(), err: &bigo.HTTPError{Status: 429}, native: true, quality: "origin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &presenceSourceClient{
				sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) { return test.web, test.err }},
				presence: func(context.Context, string) (bigo.Stream, error) {
					t.Fatal("unnecessary presence request")
					return bigo.Stream{}, nil
				},
			}
			e := &Engine{client: client}
			cfg := state.Defaults()
			cfg.NativeListening, cfg.StreamQuality = test.native, test.quality
			source, err := e.playbackInfo(context.Background(), state.Streamer{ID: "demo", Quality: test.override}, cfg)
			if !errors.Is(err, test.err) || source.Alive != test.web.Alive || source.RoomID != test.web.RoomID || source.HLS != test.web.HLS {
				t.Fatal("working source or monitoring policy changed", source, err)
			}
		})
	}
}

func TestNativePlaybackInfoRejectsConflictingIdentityAndPresenceErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		uid  uint32
		err  error
	}{
		{name: "different account", uid: 456},
		{name: "presence failure", err: &bigo.HTTPError{Status: 503}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &presenceSourceClient{
				sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) { return webOfflineFixture(), nil }},
				presence: func(context.Context, string) (bigo.Stream, error) {
					return bigo.Stream{UID: test.uid, Alive: 1, RoomID: "8589934634"}, test.err
				},
			}
			e := &Engine{client: client}
			source, err := e.playbackInfo(context.Background(), state.Streamer{ID: "demo"}, state.Defaults())
			if err == nil || source.Alive != 0 || source.RoomID != "0" {
				t.Fatal("failed presence was treated as a usable native room", source, err)
			}
		})
	}
}

func TestNativePlaybackInfoDoesNotOverridePositiveWebPresence(t *testing.T) {
	web := sourceFixture()
	web.RoomID = "0"
	client := &presenceSourceClient{
		sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) { return web, nil }},
		presence:     func(context.Context, string) (bigo.Stream, error) { return bigo.Stream{UID: 123}, nil },
	}
	e := &Engine{client: client}
	source, err := e.playbackInfo(context.Background(), state.Streamer{ID: "demo"}, state.Defaults())
	if err != nil || source.Alive != 1 || source.URL() != web.URL() {
		t.Fatal("negative presence discarded a working web stream", source, err)
	}
}

func TestNativeResumeFromPausedLiveStartsAppOnlyRecording(t *testing.T) {
	store, log := fixture(t, true, false)
	cfg := store.Config()
	cfg.Cookie = "synthetic-cookie"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(data *state.Data) error { data.Streamers[0].RecordingPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	var webCalls, presenceCalls, starts atomic.Int64
	presence := sourceFixture()
	presence.HLS = ""
	client := &presenceSourceClient{
		sourceClient: sourceClient{check: func(context.Context, string) (bigo.Stream, error) {
			webCalls.Add(1)
			return webOfflineFixture(), nil
		}},
		presence: func(context.Context, string) (bigo.Stream, error) {
			presenceCalls.Add(1)
			return presence, nil
		},
	}
	e := newEngine(store, log, client, func(_ context.Context, _ state.Streamer, source bigo.Stream, _ state.Config) (activeRecording, error) {
		if source.Alive != 1 || source.RoomID != presence.RoomID || source.UID != presence.UID {
			t.Error("resume lost the app-only room", source)
		}
		return testRecording(&starts), nil
	})
	defer e.Close()
	waitFor(t, func() bool {
		snapshot := e.Snapshot()
		return len(snapshot.Rows) == 1 && snapshot.Rows[0].Status == "Live"
	})
	if e.Snapshot().Recording != 0 || webCalls.Load() != 0 {
		t.Fatal("recording pause resolved playback or started recording")
	}
	if err := e.SetRecordingPaused("test", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if starts.Load() != 1 || webCalls.Load() != 1 || presenceCalls.Load() != 2 {
		t.Fatal("resuming native-only monitoring did not resolve one current broadcast", starts.Load(), webCalls.Load(), presenceCalls.Load())
	}
}

func TestNativeRecoveryResolvesAppOnlyRoom(t *testing.T) {
	presence := sourceFixture()
	presence.HLS = ""
	client := &presenceSourceClient{
		sourceClient: sourceClient{
			check: func(context.Context, string) (bigo.Stream, error) { return webOfflineFixture(), nil },
			native: func(_ context.Context, source bigo.Stream) (bigo.Stream, error) {
				if source.Alive != 1 || source.RoomID != presence.RoomID {
					t.Fatal("recovery received the offline web room", source)
				}
				source.HLS, source.Quality = "https://cdn.example/native.m3u8", "native"
				return source, nil
			},
		},
		presence: func(context.Context, string) (bigo.Stream, error) { return presence, nil },
	}
	e := &Engine{client: client, log: &logging.Log{}}
	source, err := e.refreshPlayback(context.Background(), state.Streamer{ID: "demo"}, state.Defaults())
	if err != nil || source.RoomID != presence.RoomID || source.Quality != "native" {
		t.Fatal("recovery ended an app-only broadcast", source, err)
	}
}
