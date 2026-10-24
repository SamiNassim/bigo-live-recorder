package bigo

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRateLimitWaitDoesNotBlockConfigurationOrCanceledRequests(t *testing.T) {
	c := NewClient()
	c.next = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := c.wait(ctx); first <- err }()
	time.Sleep(20 * time.Millisecond)
	configured := make(chan struct{})
	go func() { c.Configure("replacement-cookie", time.Second); close(configured) }()
	select {
	case <-configured:
	case <-time.After(time.Second):
		t.Fatal("rate limit wait blocked configuration")
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	done := make(chan error, 1)
	go func() { _, err := c.wait(short); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener bootstrap could not honor its timeout")
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCanceledRequestDoesNotConsumeQueueSlot(t *testing.T) {
	c := NewClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !c.next.IsZero() {
		t.Fatal("canceled request consumed a queue slot")
	}
}
