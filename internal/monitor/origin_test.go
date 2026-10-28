package monitor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/state"
)

func TestNativePreferenceCanRecordWhenWebURLIsWithheld(t *testing.T) {
	store, log := fixture(t, true, false)
	cfg := store.Config()
	cfg.Cookie = "fixture-cookie"
	cfg.StreamQuality = "origin"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int64
	source := sourceFixture()
	source.HLS = ""
	client := &sourceClient{check: func(context.Context, string) (bigo.Stream, error) { return source, bigo.ErrURLWithheld }}
	e := newEngine(store, log, client, func(_ context.Context, _ state.Streamer, got bigo.Stream, _ state.Config) (activeRecording, error) {
		if got.RoomID != source.RoomID || got.UID != source.UID {
			t.Error("withheld web response lost room identity")
		}
		return testRecording(&starts), nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if starts.Load() != 1 {
		t.Fatal("withheld HLS URL blocked native playback")
	}
}

type sourceClient struct {
	fakeClient
	check  func(context.Context, string) (bigo.Stream, error)
	native func(context.Context, bigo.Stream) (bigo.Stream, error)
}

func (c *sourceClient) Check(ctx context.Context, id string) (bigo.Stream, error) {
	return c.check(ctx, id)
}

func (c *sourceClient) OriginStream(ctx context.Context, source bigo.Stream, _ *bigolistener.Hub) (bigo.Stream, error) {
	return c.native(ctx, source)
}

func sourceFixture() bigo.Stream {
	return bigo.Stream{Alive: 1, UID: 123, Name: "Demo", RoomID: "8589934634", HLS: "https://cdn.example/web.m3u8"}
}

func TestNativePlaybackUsesPreviouslyResolvedUID(t *testing.T) {
	t.Parallel()
	source := sourceFixture()
	source.UID = 0
	client := &sourceClient{native: func(_ context.Context, got bigo.Stream) (bigo.Stream, error) {
		if got.UID != 321 || got.RoomID != source.RoomID {
			t.Fatal("native lookup lost the saved UID or current room")
		}
		return got, nil
	}}
	e := &Engine{client: client}
	got, err := e.playbackSource(context.Background(), state.Streamer{ID: "demo", UID: 321}, source, state.Defaults(), nil)
	if err != nil || got.UID != 321 {
		t.Fatalf("native source: uid=%d err=%v", got.UID, err)
	}
}

func TestPlaybackPreferenceUsesStreamerOverride(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, global, streamer string
		native                 bool
		wantCalls              int
	}{
		{"global origin", "origin", "", true, 1},
		{"unset preference", "", "", true, 1},
		{"streamer origin", "auto", "origin", true, 1},
		{"streamer auto", "origin", "auto", true, 0},
		{"streamer hd", "origin", "hd", true, 0},
		{"polling only", "origin", "", false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &sourceClient{native: func(_ context.Context, source bigo.Stream) (bigo.Stream, error) {
				calls++
				if source.RoomID != sourceFixture().RoomID || source.UID != sourceFixture().UID {
					t.Fatal("native lookup received a stale room or changed account identity")
				}
				source.HLS, source.Quality = "https://cdn.example/native.m3u8", "native"
				return source, nil
			}}
			e := &Engine{client: client, log: &logging.Log{}}
			cfg := state.Defaults()
			cfg.StreamQuality, cfg.NativeListening = test.global, test.native
			got, err := e.playbackSource(context.Background(), state.Streamer{ID: "demo", Quality: test.streamer}, sourceFixture(), cfg, nil)
			if err != nil || calls != test.wantCalls || got.UID != 123 || got.Name != "Demo" {
				t.Fatalf("selection: calls=%d source=%+v err=%v", calls, got, err)
			}
		})
	}
}

func TestRecoveryRefreshesCurrentRoomBeforeNativeLookupAndCanUseWeb(t *testing.T) {
	t.Parallel()
	webCalls, nativeCalls := 0, 0
	fresh := sourceFixture()
	fresh.RoomID = "12884901931"
	fresh.HLS = "https://cdn.example/refreshed.m3u8"
	client := &sourceClient{
		check: func(context.Context, string) (bigo.Stream, error) {
			webCalls++
			return fresh, nil
		},
		native: func(_ context.Context, source bigo.Stream) (bigo.Stream, error) {
			nativeCalls++
			if source.RoomID != fresh.RoomID || source.HLS != fresh.HLS {
				t.Fatal("recovery reused the previous room or URL")
			}
			return bigo.Stream{}, bigo.ErrOriginUnavailable
		},
	}
	e := &Engine{client: client, log: &logging.Log{}}
	cfg := state.Defaults()
	cfg.NativeListening, cfg.PollingBackup, cfg.StreamQuality = true, false, "origin"
	got, err := e.refreshPlayback(context.Background(), state.Streamer{ID: "demo"}, cfg)
	if err != nil || got.HLS != fresh.HLS || webCalls != 1 || nativeCalls != 1 {
		t.Fatalf("recovery: source=%+v err=%v web=%d native=%d", got, err, webCalls, nativeCalls)
	}
	if got.Quality == "origin" {
		t.Fatal("web fallback was relabeled as Origin")
	}
}

func TestPlaybackCancellationNeverStartsFallback(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	client := &sourceClient{native: func(context.Context, bigo.Stream) (bigo.Stream, error) {
		cancel()
		return bigo.Stream{}, bigolistener.ErrNoConnection
	}}
	log := &logging.Log{}
	e := &Engine{client: client, log: log}
	if _, err := e.playbackSource(ctx, state.Streamer{ID: "demo"}, sourceFixture(), state.Defaults(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup started fallback: %v", err)
	}
	if len(log.Entries()) != 0 {
		t.Fatal("cancellation was logged as a failed native source")
	}
	client.check = func(context.Context, string) (bigo.Stream, error) {
		t.Fatal("canceled recovery performed an API check")
		return bigo.Stream{}, nil
	}
	if _, err := e.refreshPlayback(ctx, state.Streamer{ID: "demo"}, state.Defaults()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled recovery returned %v", err)
	}
}

func TestOfflineRecoveryAndFailedWebResponse(t *testing.T) {
	t.Parallel()
	client := &sourceClient{native: func(context.Context, bigo.Stream) (bigo.Stream, error) {
		t.Fatal("offline response triggered a media lookup")
		return bigo.Stream{}, nil
	}}
	e := &Engine{client: client, log: &logging.Log{}}
	got, err := e.playbackSource(context.Background(), state.Streamer{ID: "demo"}, bigo.Stream{UID: 123}, state.Defaults(), nil)
	if err != nil || got.Alive != 0 {
		t.Fatalf("offline recovery: %+v, %v", got, err)
	}
	client.native = func(context.Context, bigo.Stream) (bigo.Stream, error) {
		return bigo.Stream{}, bigo.ErrOriginUnavailable
	}
	_, err = e.playbackSource(context.Background(), state.Streamer{ID: "demo"}, sourceFixture(), state.Defaults(), bigo.ErrLoginRequired)
	if !errors.Is(err, bigo.ErrLoginRequired) || !errors.Is(err, bigo.ErrOriginUnavailable) {
		t.Fatalf("failed web response was silently accepted: %v", err)
	}
}

func TestResolvedUIDSubscribesBeforeRecordingStarts(t *testing.T) {
	store, log := fixture(t, true, false)
	cfg := store.Config()
	cfg.Cookie = "synthetic-cookie"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &Engine{store: store, log: log, client: &fakeClient{}, ctx: ctx, cancel: cancel, events: make(chan Event, 1), wake: make(chan struct{}, 1), items: map[string]*item{"test": {row: Row{Streamer: store.Data().Streamers[0]}}}}
	e.hub = bigolistener.NewHub(ctx, e.messagingSession, nil, nil)
	defer e.hub.Close()
	e.configure(cfg)
	handle := &fakeRecording{done: make(chan struct{}), started: time.Now()}
	defer handle.Stop()
	e.start = func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
		if status := e.hub.Status(); status.TotalTargets != 1 || e.items["test"].subUID != 123 {
			t.Fatalf("source selection started before the first native subscription: %+v", status)
		}
		return handle, nil
	}
	e.check("test")
	if e.items["test"].handle != handle {
		t.Fatal("recording did not start")
	}
	e.reconcile()
	if e.items["test"].subUID != 123 {
		t.Fatal("reconciliation replaced the discovered subscription")
	}
}

func TestProductionStarterIsSetBeforeWorkerLaunch(t *testing.T) {
	store, log := fixture(t, false, false)
	e := newEngine(store, log, &fakeClient{offline: true}, nil)
	defer e.Close()
	if e.start == nil {
		t.Fatal("worker was launched without a production recording starter")
	}
}
