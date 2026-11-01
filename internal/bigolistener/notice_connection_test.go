package bigolistener

import (
	"context"
	"crypto/rc4"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type noticeConnectionFixture struct {
	events chan liveEvent
	fails  chan error
}

func (*noticeConnectionFixture) Close() error                                   { return nil }
func (*noticeConnectionFixture) addFollow(context.Context, uint32) error        { return nil }
func (*noticeConnectionFixture) enableLiveNotice(context.Context, uint32) error { return nil }
func (c *noticeConnectionFixture) waitLiveEvent(ctx context.Context) (liveEvent, error) {
	select {
	case event := <-c.events:
		return event, nil
	case err := <-c.fails:
		return liveEvent{}, err
	case <-ctx.Done():
		return liveEvent{}, ctx.Err()
	}
}

func TestUnpairedNoticeExpiresWithoutReconnectingOrPolling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c := &noticeConnectionFixture{events: make(chan liveEvent, 1), fails: make(chan error, 1)}
	c.events <- hintFixture(1, "")
	var dials atomic.Int32
	h := &Hub{ctx: ctx, enabled: true, usable: true, configKey: "fixture", wake: make(chan struct{}, 1), watchers: map[uint64]*watcher{
		1: {publicID: "public-one", targetUID: 111, events: make(chan Notification, 1)},
	}, source: testSessionSource}
	h.dial = func(context.Context, Session) (hubClient, string, error) { dials.Add(1); return c, "fixture", nil }
	snapshot, _ := h.snapshot()
	done := make(chan error, 1)
	go func() {
		done <- h.connectAndListen(ctx, snapshot, map[uint32]bool{}, map[uint32]bool{}, map[uint32]targetRejection{})
	}()
	started := time.Now()
	select {
	case notice := <-h.watchers[1].events:
		if notice.Attributed || time.Since(started) < noticePairGrace-100*time.Millisecond {
			t.Fatal("unresolved hint was dispatched before its grace period")
		}
	case <-ctx.Done():
		t.Fatal("passive event wait never expired the fallback")
	}
	if dials.Load() != 1 {
		t.Fatal("notification deadline reconnected the listener")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPassiveReadDeadlinesDoNotPostponeHeartbeat(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	cipher, err := rc4.NewCipher([]byte("synthetic-key"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(linkdPingEvery)
	c := &linkdClient{conn: client, recvCipher: cipher, nextPing: deadline}
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_, err := c.waitLiveEvent(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || !c.nextPing.Equal(deadline) {
			t.Fatal("short notification wait postponed the native heartbeat", err, c.nextPing)
		}
	}
}

type interruptedNoticeFixture struct {
	noticeConnectionFixture
	emitted bool
	end     func()
	err     error
}

func (c *interruptedNoticeFixture) waitLiveEvent(context.Context) (liveEvent, error) {
	if !c.emitted {
		c.emitted = true
		return hintFixture(1, "first"), nil
	}
	if c.end != nil {
		c.end()
	}
	return liveEvent{}, c.err
}

func TestPendingNoticeSurvivesDisconnectButNotShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "shutdown"}[shutdown], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &interruptedNoticeFixture{err: io.EOF}
			if shutdown {
				c.end = cancel
			}
			events := make(chan Notification, 1)
			h := &Hub{ctx: ctx, enabled: true, usable: true, configKey: "fixture", wake: make(chan struct{}, 1), watchers: map[uint64]*watcher{
				1: {publicID: "public-one", targetUID: 111, events: events},
			}, source: testSessionSource}
			h.dial = func(context.Context, Session) (hubClient, string, error) { return c, "fixture", nil }
			snapshot, _ := h.snapshot()
			err := h.connectAndListen(ctx, snapshot, map[uint32]bool{}, map[uint32]bool{}, map[uint32]targetRejection{})
			if shutdown {
				if !errors.Is(err, context.Canceled) || len(events) != 0 {
					t.Fatal("shutdown dispatched a stale fallback", err)
				}
			} else if !errors.Is(err, io.EOF) || len(events) != 1 || (<-events).Attributed {
				t.Fatal("disconnect lost the unresolved live hint", err)
			}
		})
	}
}
