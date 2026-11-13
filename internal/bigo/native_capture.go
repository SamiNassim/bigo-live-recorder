package bigo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"bigo-live-recorder/internal/fault"
)

const nativeRecoveryAttempts = 3
const nativeRecoveryDelay = 5 * time.Second

type nativeCapture struct {
	ctx               context.Context
	cancel            context.CancelFunc
	queue             *nativeFrameQueue
	done              chan struct{}
	recovery          StreamRecovery
	mu                sync.Mutex
	nextMu            sync.Mutex
	session           *nativeMediaSession
	reconnectErr      error
	partDone          <-chan struct{}
	deferred          []*nativeFrameQueue
	reason            string
	clock             *nativeTSClock
	gops              []nativeVideoGOP
	openFallback      func(context.Context, Stream) (Capture, error)
	replacement       Capture
	replacementReason bool
	guest             bool
	epoch             time.Time
	timing            *CaptureTiming
	videoHeaders      nativeVideoHeaders
	splitInterval     time.Duration
}

type nativeVideoGOP struct {
	pts    uint32
	frames *nativeFrameQueue
}

func newNativeCapture(parent context.Context, source Stream, recovery StreamRecovery) (*nativeCapture, error) {
	return newNativeCaptureWithFallback(parent, source, recovery, nil)
}

func newNativeCaptureWithFallback(parent context.Context, source Stream, recovery StreamRecovery, openFallback func(context.Context, Stream) (Capture, error)) (*nativeCapture, error) {
	ctx, cancel := context.WithCancel(parent)
	session, err := openNativeMediaSession(ctx, source, recovery.Report)
	if err != nil {
		cancel()
		return nil, err
	}
	c := &nativeCapture{ctx: ctx, cancel: cancel, queue: newNativeFrameQueue(8 << 20), done: make(chan struct{}), recovery: recovery, session: session, openFallback: openFallback}
	if recovery.Report != nil {
		recovery.Report("INFO", "Native video and audio connected (Origin requested)")
	}
	go c.receive(source, session)
	return c, nil
}

func (c *nativeCapture) Close() error {
	c.cancel()
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session != nil {
		session.Close()
	}
	<-c.done
	c.nextMu.Lock()
	defer c.nextMu.Unlock()
	c.mu.Lock()
	partDone := c.partDone
	c.mu.Unlock()
	if partDone != nil {
		<-partDone
	}
	c.queue.close()
	for _, queue := range c.deferred {
		queue.close()
	}
	c.deferred = nil
	c.clearPreroll()
	c.mu.Lock()
	replacement := c.replacement
	c.mu.Unlock()
	if replacement != nil {
		replacement.Close()
	}
	return nil
}

func (c *nativeCapture) receive(source Stream, session *nativeMediaSession) {
	defer close(c.done)
	defer func() { session.Close() }()
	order := &nativeMediaOrder{}
	defer func() { order.close() }()
	for {
		frame, err := session.next(c.ctx)
		if err == nil {
			if err = order.add(frame, c.queue.push); err == nil {
				continue
			}
			if !errors.Is(err, errNativeMediaOrderClock) {
				c.queue.finish(err)
				return
			}
			session.Close()
		}
		if c.ctx.Err() != nil {
			c.queue.finish(c.ctx.Err())
			return
		}
		if flushErr := order.flush(nil, c.queue.push); flushErr != nil {
			c.queue.finish(flushErr)
			return
		}
		order.close()
		order = &nativeMediaOrder{}
		c.mu.Lock()
		if c.reconnectErr != nil {
			err = c.reconnectErr
		}
		c.reconnectErr = nil
		c.mu.Unlock()
		if c.recovery.Refresh == nil {
			c.queue.finish(err)
			return
		}
		c.recovery.reportError("WARN", "Stream data stopped arriving: "+fault.Reason(err)+". Attempting to reconnect while keeping the recording open.", err)
		session.Close()
		recovered := false
		for attempt := 0; attempt < nativeRecoveryAttempts; attempt++ {
			if c.ctx.Err() != nil {
				break
			}
			if attempt > 0 {
				c.recovery.reportError("WARN", fmt.Sprintf("Could not reconnect to the stream: %s. Retrying in %s (attempt %d of %d).", fault.Reason(err), nativeRecoveryDelay, attempt+1, nativeRecoveryAttempts), err)
			}
			if attempt > 0 && waitHLS(c.ctx, nativeRecoveryDelay) != nil {
				break
			}
			ctx, cancel := context.WithTimeout(c.ctx, 25*time.Second)
			fresh, refreshErr := c.recovery.Refresh(ctx)
			cancel()
			if refreshErr != nil {
				err = refreshErr
				continue
			}
			if fresh.RoomKind == RoomAudioMultiGuest || fresh.RoomKind == RoomHybridMultiGuest {
				if c.recovery.Report != nil {
					kind := "multi-guest"
					if fresh.RoomKind == RoomAudioMultiGuest {
						kind = "audio-only and multi-guest"
					}
					c.recovery.Report("INFO", "The room is now "+kind+"; finalizing this video before checking for individual guest recordings.")
				}
				c.queue.finish(nil)
				return
			}
			if fresh.Alive != 1 || (fresh.UID != 0 && source.UID != 0 && fresh.UID != source.UID) || (fresh.RoomID != "" && source.RoomID != "" && fresh.RoomID != source.RoomID) {
				if c.recovery.Report != nil {
					message := "BIGO reports a different broadcast; finalizing the previous recording"
					if fresh.Alive != 1 {
						message = "BIGO reports this broadcast ended; finalizing the recording"
					}
					c.recovery.Report("INFO", message)
				}
				c.queue.finish(nil)
				return
			}
			if fresh.native == nil {
				if fresh.URL() != "" && c.openFallback != nil {
					fallback, openErr := c.openFallback(c.ctx, fresh)
					if openErr == nil {
						c.mu.Lock()
						c.replacement = fallback
						c.mu.Unlock()
						c.queue.finish(nil)
						return
					}
					err = openErr
					continue
				}
				err = errors.New("BIGO native source is unavailable")
				continue
			}
			next, openErr := openNativeMediaSession(c.ctx, fresh, c.recovery.Report)
			if openErr != nil {
				err = openErr
				continue
			}
			session, source = next, fresh
			c.mu.Lock()
			c.session = session
			c.mu.Unlock()
			if err = c.queue.push(nativeMediaFrame{reset: true}); err != nil {
				c.queue.finish(err)
				return
			}
			recovered = true
			break
		}
		if !recovered {
			if c.ctx.Err() != nil {
				c.queue.finish(c.ctx.Err())
			} else {
				c.queue.finish(fault.Wrap(fmt.Sprintf("could not restore the native stream after %d attempts: %s", nativeRecoveryAttempts, fault.Reason(err)), err))
			}
			return
		}
		if c.recovery.Report != nil {
			c.recovery.Report("INFO", "BIGO media resumed; continuing capture")
		}
	}
}

func (c *nativeCapture) reconnect(err error) {
	c.mu.Lock()
	c.reconnectErr = err
	session := c.session
	c.mu.Unlock()
	if session != nil {
		session.Close()
	}
}

func (c *nativeCapture) nextFrame(ctx context.Context) (nativeMediaFrame, error) {
	for len(c.deferred) > 0 {
		f, err := c.deferred[0].next(ctx)
		if errors.Is(err, io.EOF) {
			c.deferred[0].close()
			c.deferred = c.deferred[1:]
			continue
		}
		return f, err
	}
	return c.queue.next(ctx)
}

type nativeAudioConfig [3]byte

func audioConfig(f nativeMediaFrame) nativeAudioConfig {
	if f.codec == 7 {
		return nativeAudioConfig{7, 2, 0}
	}
	if len(f.data) < 7 {
		return nativeAudioConfig{}
	}
	return nativeAudioConfig{f.data[1] & 8, f.data[2] & 0xfc, (f.data[2]&1)<<2 | f.data[3]>>6}
}

func (c *nativeCapture) Next(ctx context.Context) (*CapturePart, error) {
	c.nextMu.Lock()
	defer c.nextMu.Unlock()
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	if c.partDone != nil {
		select {
		case <-c.partDone:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.clearPreroll()
	if c.guest {
		return c.nextGuestPart(ctx)
	}
	waiting := newNativeFrameQueue(1 << 20)
	defer func() {
		if waiting != nil {
			waiting.close()
		}
	}()
	var first nativeMediaFrame
	var config nativeAudioConfig
	videoOnly := false
	var boundary *uint32
	var audioPTS uint32
	for !first.key || config == (nativeAudioConfig{}) {
		f, err := c.nextFrame(ctx)
		if err != nil {
			// Even if the media connection fails before the next AAC sample,
			// flush received video into a final video-only file.
			if first.key && ctx.Err() == nil && c.ctx.Err() == nil {
				waiting.finish(err)
				videoOnly = true
				break
			}
			if errors.Is(err, io.EOF) {
				c.mu.Lock()
				replacement := c.replacement
				c.mu.Unlock()
				if replacement != nil {
					part, err := replacement.Next(ctx)
					if err == nil && !c.replacementReason {
						part.Reason = "Native media became unavailable; continuing with web playback"
						c.replacementReason = true
					}
					return part, err
				}
			}
			return nil, err
		}
		if f.reset {
			if first.key {
				if err := waiting.push(f); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !first.key {
			if !f.video || !f.key {
				continue
			}
			first = f
		}
		if !f.video {
			config = audioConfig(f)
			audioPTS = f.pts
		} else if kind, _ := nativeVideoType(f.codec); f.key && boundary == nil {
			initial, _ := nativeVideoType(first.codec)
			if kind != initial {
				pts := f.pts
				boundary = &pts
			}
		}
		if err := waiting.push(f); err != nil {
			return nil, err
		}
	}
	codec, err := nativeVideoType(first.codec)
	if err != nil {
		return nil, err
	}
	// A new file gets its own zero-based clock and transport continuity. Socket
	// recovery within this file reuses that clock instead.
	c.clock = &nativeTSClock{}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	c.mu.Lock()
	c.partDone = done
	c.mu.Unlock()
	reason := c.reason
	c.reason = ""
	noAudio := videoOnly
	if boundary != nil && !videoOnly {
		noAudio = int32(audioPTS-*boundary) >= 0
	}
	waiting.finish(nil)
	c.deferred = append([]*nativeFrameQueue{waiting}, c.deferred...)
	waiting = nil
	go c.writePart(writer, done, codec, config, noAudio)
	return &CapturePart{ReadCloser: reader, Reason: reason, Native: true}, nil
}

func (c *nativeCapture) rememberVideo(f nativeMediaFrame) error {
	if !f.video {
		return nil
	}
	if f.key {
		c.gops = append(c.gops, nativeVideoGOP{pts: f.pts, frames: newNativeFrameQueue(256 << 10)})
	}
	if len(c.gops) == 0 {
		return nil
	}
	return c.gops[len(c.gops)-1].frames.push(f)
}

func (c *nativeCapture) advancePreroll(pts uint32) {
	// Video and audio arrive independently. Keep the keyframe preceding the
	// audio watermark even when newer video keyframes have already arrived.
	for len(c.gops) > 1 && int32(pts-c.gops[1].pts) >= 0 {
		c.gops[0].frames.close()
		c.gops[0] = nativeVideoGOP{}
		c.gops = c.gops[1:]
	}
}

func (c *nativeCapture) clearPreroll() {
	for _, gop := range c.gops {
		gop.frames.close()
	}
	c.gops = nil
}

func (c *nativeCapture) writePart(writer *io.PipeWriter, done chan struct{}, codec byte, config nativeAudioConfig, noAudio bool) {
	defer close(done)
	stop := context.AfterFunc(c.ctx, func() { writer.CloseWithError(c.ctx.Err()) })
	defer stop()
	mux := nativeTSMux{out: writer, clock: c.clock, noAudio: noAudio, opus: config[0] == 7, startWithAudio: c.guest, videoCodec: codec, timing: c.timing}
	draining := false
	warnedPrefix := false
	for {
		f, err := c.nextFrame(c.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			writer.CloseWithError(err)
			return
		}
		if f.cut {
			c.guestStartupCut()
			writer.Close()
			return
		}
		if f.reset {
			c.clearPreroll()
			if !c.guest {
				c.videoHeaders = nativeVideoHeaders{}
			}
			mux = nativeTSMux{out: writer, clock: c.clock, noAudio: noAudio, opus: config[0] == 7, timing: c.timing}
			draining = false
			continue
		}
		if draining {
			continue
		}
		if f.video {
			var boundary nativeMediaFrame
			var headersReady bool
			if c.guest || c.splitInterval > 0 {
				boundary, headersReady = c.videoHeaders.prepare(f)
			}
			if c.guest {
				f = boundary
			}
			fresh, err := nativeVideoType(f.codec)
			if err != nil {
				writer.CloseWithError(err)
				return
			}
			timed := fresh == codec && headersReady && mux.splitDue(f, c.splitInterval)
			if fresh != codec || timed {
				if !f.key {
					if !warnedPrefix && c.recovery.Report != nil {
						c.recovery.Report("WARN", "Video encoding changed without a keyframe; waiting for decodable video")
						warnedPrefix = true
					}
					continue
				}
				reason := "Video encoding changed; continuing in a new recording"
				if timed {
					f = boundary
					reason = "Recording split interval reached; continuing in a new file"
				}
				// Audio travels on an independent socket. Keep this file open
				// until audio reaches the new keyframe, otherwise late samples
				// from before the cut would be discarded by the next file.
				reason, err = c.videoBoundary(f, &mux, config, reason)
				if err != nil {
					writer.CloseWithError(err)
					return
				}
				c.reason = reason
				writer.Close()
				return
			}
		} else if audioConfig(f) != config {
			if len(c.gops) == 0 {
				writer.CloseWithError(errors.New("BIGO audio encoding changed without a retained video keyframe"))
				return
			}
			// Retain a short video pre-roll rather than losing audio while
			// waiting for another keyframe. Only video overlaps the old file.
			tail := newNativeFrameQueue(1 << 20)
			if err := tail.push(f); err != nil {
				tail.close()
				writer.CloseWithError(err)
				return
			}
			tail.finish(nil)
			c.prependPreroll(tail)
			c.reason = "Audio encoding changed; continuing in a new recording with video pre-roll"
			writer.Close()
			return
		}
		if err = mux.writeFrame(f); err != nil {
			if c.ctx.Err() != nil || errors.Is(err, io.ErrClosedPipe) {
				writer.CloseWithError(err)
				return
			}
			var clockErr nativeClockError
			if c.guest && errors.As(err, &clockErr) {
				writer.CloseWithError(c.continueGuestClock(f))
				return
			}
			if c.recovery.Refresh == nil {
				writer.CloseWithError(err)
				return
			}
			draining = true
			c.reconnect(err)
			continue
		}
		if err := c.rememberVideo(f); err != nil {
			writer.CloseWithError(err)
			return
		}
		if !f.video {
			c.advancePreroll(f.pts)
		}
	}
}

func (c *nativeCapture) prependPreroll(tail *nativeFrameQueue) {
	queues := make([]*nativeFrameQueue, 0, len(c.gops)+1+len(c.deferred))
	for _, gop := range c.gops {
		gop.frames.finish(nil)
		queues = append(queues, gop.frames)
	}
	queues = append(queues, tail)
	c.deferred = append(queues, c.deferred...)
	c.gops = nil
}

func (c *nativeCapture) videoBoundary(first nativeMediaFrame, mux *nativeTSMux, config nativeAudioConfig, reason string) (string, error) {
	queue := newNativeFrameQueue(1 << 20)
	if err := queue.push(first); err != nil {
		queue.close()
		return "", err
	}
	for !mux.noAudio {
		f, err := c.nextFrame(c.ctx)
		if err != nil {
			if c.ctx.Err() != nil {
				queue.close()
				return "", err
			}
			if errors.Is(err, io.EOF) {
				err = nil
			}
			queue.finish(err)
			break
		}
		if !f.video && !f.reset && !f.cut && int32(f.pts-first.pts) < 0 {
			// A late audio configuration change can precede this video cut.
			// Roll that change first, retaining the already-buffered video.
			if audioConfig(f) != config {
				if err = queue.push(f); err != nil {
					queue.close()
					return "", err
				}
				queue.finish(nil)
				c.prependPreroll(queue)
				return "Audio encoding changed; continuing in a new recording with video pre-roll", nil
			}
			if err = mux.writeFrame(f); err != nil {
				queue.close()
				return "", err
			}
			c.advancePreroll(f.pts)
			continue
		}
		if err = queue.push(f); err != nil {
			queue.close()
			return "", err
		}
		if f.reset || f.cut || (!f.video && int32(f.pts-first.pts) >= 0) {
			break
		}
	}
	queue.finish(nil)
	c.deferred = append([]*nativeFrameQueue{queue}, c.deferred...)
	return reason, nil
}
