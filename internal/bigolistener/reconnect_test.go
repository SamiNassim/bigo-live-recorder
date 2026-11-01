package bigolistener

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the worker, not just connectAndListen: an interrupted child wait
// must never cancel the worker or poison the next ticket/connection context.
func TestWorkerReconnectsAfterInterruptedConnection(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, io.EOF} {
		t.Run(failure.Error(), func(t *testing.T) {
			var tickets atomic.Int32
			var attempts atomic.Int32
			warnings := make(chan struct{}, 4)
			waiting := make(chan struct{}, 1)
			ctx, cancel := context.WithCancel(context.Background())
			h := &Hub{
				ctx: ctx, cancel: cancel, enabled: true, usable: true, configKey: "cookie",
				wake:     make(chan struct{}, 1),
				watchers: map[uint64]*watcher{1: {publicID: "test", targetUID: 123, events: make(chan Notification, 1)}},
				source: func(ctx context.Context, _ string) (Session, error) {
					if err := ctx.Err(); err != nil {
						return Session{}, err
					}
					tickets.Add(1)
					return validTestSession(), nil
				},
				warning: func(string) { warnings <- struct{}{} },
				info: func(message string) {
					if strings.Contains(message, "connection restarting") {
						warnings <- struct{}{}
					}
				},
			}
			h.dial = func(ctx context.Context, _ Session) (hubClient, string, error) {
				if err := ctx.Err(); err != nil {
					return nil, "", err
				}
				switch attempts.Add(1) {
				case 1:
					return &fakeHubClient{waitErr: failure}, "test", nil
				case 2:
					return nil, "", errors.New("temporary authentication failure")
				default:
					return &recoveredHubClient{waiting: waiting}, "test", nil
				}
			}
			h.wg.Add(1)
			go h.run()
			defer h.Close()
			for range 2 {
				waitForTestSignal(t, warnings, "retry warning")
				if h.Status().State != "retrying" {
					t.Fatal(h.Status())
				}
				// Wake the actual backoff without sleeping through it in the test.
				h.mu.Lock()
				h.signalWakeLocked()
				h.mu.Unlock()
			}
			waitForTestSignal(t, waiting, "recovered connection")
			if !h.Status().Connected || tickets.Load() != 3 {
				t.Fatalf("status=%+v tickets=%d", h.Status(), tickets.Load())
			}
		})
	}
}

type recoveredHubClient struct{ waiting chan struct{} }

func (*recoveredHubClient) Close() error                                   { return nil }
func (*recoveredHubClient) addFollow(context.Context, uint32) error        { return nil }
func (*recoveredHubClient) enableLiveNotice(context.Context, uint32) error { return nil }
func (c *recoveredHubClient) waitLiveEvent(ctx context.Context) (liveEvent, error) {
	c.waiting <- struct{}{}
	<-ctx.Done()
	return liveEvent{}, ctx.Err()
}

func TestRemovingLastTargetDoesNotLeaveRetryingStatus(t *testing.T) {
	h := NewHub(context.Background(), testSessionSource, nil, nil, nil)
	defer h.Close()
	h.Configure(true, "cookie", "device")
	_, off, err := h.Subscribe("test", 123)
	if err != nil {
		t.Fatal(err)
	}
	off()
	time.Sleep(hubRestartDebounce + 20*time.Millisecond)
	if h.Status().State != "idle" {
		t.Fatal(h.Status())
	}
}

func TestSilentConnectionExpiresWithoutWaitingForTCPFailure(t *testing.T) {
	c := &linkdClient{lastReceived: time.Now().Add(-linkdIdleTimeout)}
	_, err := c.waitLiveEvent(context.Background())
	if err == nil || !strings.Contains(err.Error(), "heartbeats") {
		t.Fatalf("silent peer was not rejected: %v", err)
	}
}
