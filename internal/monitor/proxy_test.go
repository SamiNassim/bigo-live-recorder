package monitor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/network"
	"bigo-live-recorder/internal/state"
)

func TestChangingProxyRestartsActiveRecordingWithNewSettings(t *testing.T) {
	store, log := fixture(t, false, false)
	var starts atomic.Int32
	configs := make(chan network.Config, 4)
	engine := newEngine(store, log, &fakeClient{}, func(_ context.Context, _ state.Streamer, _ bigo.Stream, cfg state.Config) (activeRecording, error) {
		configs <- cfg.Proxy
		starts.Add(1)
		return &fakeRecording{done: make(chan struct{}), started: time.Now()}, nil
	})
	defer engine.Close()
	waitFor(t, func() bool { return engine.Snapshot().Recording == 1 })
	if (<-configs).Enabled {
		t.Fatal("proxy enabled for ordinary recording")
	}
	cfg := store.Config()
	cfg.Proxy = network.Config{Enabled: true, URL: "socks5://127.0.0.1:1080"}
	if err := engine.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return starts.Load() == 2 && engine.Snapshot().Recording == 1 })
	if got := <-configs; got != cfg.Proxy {
		t.Fatal("recording restarted with the old proxy")
	}
	// Unrelated settings must not interrupt a healthy recording.
	cfg.Notifications = !cfg.Notifications
	if err := engine.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	for _, item := range engine.items {
		if item.handle == nil {
			t.Error("unrelated setting stopped recording")
			continue
		}
		select {
		case <-item.handle.Done():
			t.Error("unrelated setting canceled recording")
		default:
		}
	}
	engine.mu.Unlock()
}
