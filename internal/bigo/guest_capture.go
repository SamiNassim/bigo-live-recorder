package bigo

import (
	"context"
	"errors"
	"io"
	"time"
)

// Guest captures share room transports, but own their buffers and file clocks.
// A slow muxer or a guest leaving cannot interrupt the other participants.
func newGuestCapture(parent context.Context, recovery StreamRecovery) *nativeCapture {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	close(done)
	return &nativeCapture{ctx: ctx, cancel: cancel, queue: newNativeFrameQueue(1 << 20), done: done, recovery: recovery, guest: true}
}

func (c *nativeCapture) nextGuestPart(ctx context.Context) (*CapturePart, error) {
	waiting := newNativeFrameQueue(1 << 20)
	owned := true
	defer func() {
		if owned {
			waiting.close()
		}
	}()
	var firstVideo nativeMediaFrame
	var config nativeAudioConfig
	var received bool
	var base uint32
	var terminal error
	// Let a camera keyframe catch up with audio without discarding the audio
	// prefix. A participant without a camera then gets an audio-only file.
	setup, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		f, err := c.nextFrame(setup)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !received {
				return nil, err
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				terminal = err
			}
			break
		}
		if f.reset {
			continue
		}
		if f.video {
			var ready bool
			f, ready = c.videoHeaders.prepare(f)
			if !firstVideo.key {
				if !ready {
					continue
				}
				firstVideo = f
			}
		} else if config == (nativeAudioConfig{}) {
			config = audioConfig(f)
		}
		stamp := f.pts
		if f.video && int32(f.dts-stamp) < 0 {
			stamp = f.dts
		}
		if !received || int32(stamp-base) < 0 {
			base = stamp
		}
		received = true
		if err := waiting.push(f); err != nil {
			return nil, err
		}
		if firstVideo.key && config != (nativeAudioConfig{}) {
			break
		}
	}
	waiting.finish(nil)
	c.deferred = append([]*nativeFrameQueue{waiting}, c.deferred...)
	owned = false
	// A terminal transport error follows the already received media, rather
	// than hiding it before the writer can create the final playable file.
	if terminal != nil {
		c.queue.finish(terminal)
	}
	c.clock = &nativeTSClock{}
	c.timing = &CaptureTiming{Epoch: c.epoch}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	c.mu.Lock()
	c.partDone = done
	c.mu.Unlock()
	reason := c.reason
	c.reason = ""
	if firstVideo.key {
		codec, err := nativeVideoType(firstVideo.codec)
		if err != nil {
			reader.Close()
			writer.Close()
			close(done)
			return nil, err
		}
		c.clock = &nativeTSClock{started: true, base: base, codec: codec, lastPTS: -1}
		go c.writePart(writer, done, codec, config, config == (nativeAudioConfig{}))
	} else {
		go c.writeGuestAudio(writer, done, config)
	}
	return &CapturePart{ReadCloser: reader, Reason: reason, Native: true, AudioOnly: !firstVideo.key, Timing: c.timing}, nil
}

func (c *nativeCapture) writeGuestAudio(writer *io.PipeWriter, done chan struct{}, config nativeAudioConfig) {
	defer close(done)
	stop := context.AfterFunc(c.ctx, func() { writer.CloseWithError(c.ctx.Err()) })
	defer stop()
	mux := nativeTSMux{out: writer, clock: c.clock, audioOnly: true, opus: config[0] == 7, timing: c.timing}
	for {
		f, err := c.nextFrame(c.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			writer.CloseWithError(err)
			return
		}
		if f.reset {
			mux = nativeTSMux{out: writer, clock: c.clock, audioOnly: true, opus: config[0] == 7, timing: c.timing}
			continue
		}
		if f.video {
			var ready bool
			f, ready = c.videoHeaders.prepare(f)
			if !ready {
				continue
			}
			_, err := c.videoBoundary(f, &mux, config)
			c.reason = "The guest enabled their camera; continuing with original video and audio in a new file"
			writer.CloseWithError(err)
			return
		}
		if audioConfig(f) != config {
			tail := newNativeFrameQueue(1 << 20)
			if err := tail.push(f); err != nil {
				tail.close()
				writer.CloseWithError(err)
				return
			}
			tail.finish(nil)
			c.deferred = append([]*nativeFrameQueue{tail}, c.deferred...)
			c.reason = "Guest audio encoding changed; continuing in a new audio recording"
			writer.Close()
			return
		}
		if err := mux.writeFrame(f); err != nil {
			var clockErr nativeClockError
			if errors.As(err, &clockErr) {
				err = c.continueGuestClock(f)
			}
			writer.CloseWithError(err)
			return
		}
	}
}

// A guest has no private socket to reconnect: the room owns its transports.
// Retain the changed frame and continue in a fresh file instead of waiting
// forever for a reset marker that only ordinary single-source sessions emit.
func (c *nativeCapture) continueGuestClock(f nativeMediaFrame) error {
	c.clearPreroll()
	tail := newNativeFrameQueue(1 << 20)
	if err := tail.push(f); err != nil {
		tail.close()
		return err
	}
	tail.finish(nil)
	c.deferred = append([]*nativeFrameQueue{tail}, c.deferred...)
	c.reason = "The guest's media clock restarted; continuing in a new original recording"
	return nil
}
