package bigo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

type fallbackPolicy struct{ initial, maximum, timeout time.Duration }

func defaultFallbackPolicy() fallbackPolicy {
	return fallbackPolicy{30 * time.Second, 2 * time.Minute, 25 * time.Second}
}

type fallbackCapture struct {
	ctx        context.Context
	cancel     context.CancelFunc
	stream     io.ReadCloser
	taken      bool
	mu         sync.Mutex
	candidate  Capture
	done       chan struct{}
	openNative func(context.Context, Stream) (Capture, error)
}

func newFallbackCapture(parent context.Context, client *http.Client, source Stream, referer string, recovery StreamRecovery, policy fallbackPolicy, openNative func(context.Context, Stream) (Capture, error)) (Capture, error) {
	ctx, cancel := context.WithCancel(parent)
	c := &fallbackCapture{ctx: ctx, cancel: cancel, done: make(chan struct{}), openNative: openNative}
	recovery.stopHLS = func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.candidate != nil }
	stream, err := openHLSStream(ctx, client, source, referer, recovery, defaultHLSPolicy(), true)
	if err != nil {
		cancel()
		return nil, err
	}
	c.stream = stream
	go c.findOrigin(source, recovery, policy)
	return c, nil
}

func (c *fallbackCapture) findOrigin(source Stream, recovery StreamRecovery, policy fallbackPolicy) {
	defer close(c.done)
	delay := policy.initial
	for {
		if waitHLS(c.ctx, delay) != nil {
			return
		}
		ctx, cancel := context.WithTimeout(c.ctx, policy.timeout)
		fresh, err := recovery.Origin(ctx, source)
		if err == nil && (fresh.UID != source.UID || fresh.RoomID != source.RoomID) {
			err = errors.New("BIGO native upgrade returned another broadcast")
		}
		var candidate Capture
		candidateCtx, closeCandidate := context.WithCancel(c.ctx)
		stop := context.AfterFunc(ctx, closeCandidate)
		if err == nil && fresh.native != nil {
			candidate, err = c.openNative(candidateCtx, fresh)
		} else if err == nil {
			err = ErrOriginUnavailable
		}
		stop()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		cancel()
		if candidate != nil && err == nil {
			c.mu.Lock()
			c.candidate = &promotionCapture{Capture: candidate, cancel: closeCandidate}
			c.mu.Unlock()
			return
		}
		closeCandidate()
		if candidate != nil {
			candidate.Close()
		}
		if c.ctx.Err() != nil {
			return
		}
		delay = min(policy.maximum, delay*2)
	}
}

type promotionCapture struct {
	Capture
	cancel context.CancelFunc
}

func (c *promotionCapture) Close() error { c.cancel(); return c.Capture.Close() }

func (c *fallbackCapture) Next(ctx context.Context) (*CapturePart, error) {
	if !c.taken {
		c.taken = true
		return &CapturePart{ReadCloser: c.stream}, nil
	}
	c.mu.Lock()
	candidate := c.candidate
	c.mu.Unlock()
	if candidate == nil {
		return nil, io.EOF
	}
	part, err := candidate.Next(ctx)
	if err == nil && part.Reason == "" {
		part.Reason = "Native playback became available; recording the requested Origin stream"
	}
	return part, err
}

func (c *fallbackCapture) Close() error {
	c.cancel()
	c.stream.Close()
	<-c.done
	c.mu.Lock()
	candidate := c.candidate
	c.mu.Unlock()
	if candidate != nil {
		return candidate.Close()
	}
	return nil
}
