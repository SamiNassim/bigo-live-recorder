package bigo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"bigo-live-recorder/internal/fault"
)

func openNativeRecoveringStream(parent context.Context, client *http.Client, source Stream, referer string, recovery StreamRecovery) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(parent)
	clock := &nativeTSClock{}
	stream, setup, err := openNativeStreamClock(ctx, source, clock, recovery.Report)
	if err != nil {
		cancel()
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if source.URL() == "" {
			return nil, err
		}
		recovery.reportError("WARN", "Native media could not start: "+fault.Reason(err)+". Trying the web stream instead.", err)
		source.native = nil
		source.Quality = "web"
		return openHLSStream(parent, client, source, referer, recovery, defaultHLSPolicy(), true)
	}
	recovery.Log("INFO", "Native video and audio connected (Origin requested)", setup, nil)
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { writer.CloseWithError(ctx.Err()) })
		defer stop()
		current := source
		for {
			err = func() error {
				defer stream.Close()
				closeStream := context.AfterFunc(ctx, func() { stream.Close() })
				defer closeStream()
				_, err := io.Copy(writer, stream)
				return err
			}()
			if ctx.Err() != nil {
				writer.CloseWithError(ctx.Err())
				return
			}
			if err == nil {
				writer.Close()
				return
			}
			if recovery.Refresh == nil {
				writer.CloseWithError(err)
				return
			}
			reconnect := newNativeRecoveryLog(err)
			recovery.Log("WARN", "Origin stream interrupted. Reconnecting…", "Reopening both Origin media sockets while keeping the recording open.", err)
			recovered := false
			for attempt := 0; attempt < nativeRecoveryAttempts; attempt++ {
				if attempt > 0 {
					if waitHLS(ctx, nativeRecoveryDelay) != nil {
						break
					}
				}
				reconnect.beginAttempt(attempt + 1)
				refreshCtx, done := context.WithTimeout(ctx, 25*time.Second)
				fresh, refreshErr := recovery.Refresh(refreshCtx)
				reconnect.refresh = time.Since(reconnect.attemptStarted)
				done()
				if refreshErr != nil {
					err = reconnect.failure("source refresh", refreshErr)
					continue
				}
				if fresh.Alive != 1 || (fresh.RoomID != "" && current.RoomID != "" && fresh.RoomID != current.RoomID) {
					writer.Close()
					return
				}
				if fresh.native != nil {
					stream, setup, err = openNativeStreamClock(ctx, fresh, clock, recovery.Report)
				} else {
					// An HLS fallback can have a different codec. Finalize this
					// MKV before the monitor starts a compatible new recording.
					writer.CloseWithError(errors.New("BIGO native source is unavailable; a new recording is required for web fallback"))
					return
				}
				if err == nil {
					current = fresh
					recovered = true
					break
				}
				err = reconnect.failure("media setup", err)
			}
			if !recovered {
				writer.CloseWithError(fault.Wrap("could not restore the native stream after 3 attempts: "+fault.Reason(err), err))
				return
			}
			recovery.Log("INFO", "BIGO media resumed; continuing the same recording file", reconnect.details("restored media")+"\n"+setup, nil)
		}
	}()
	return &nativeReadCloser{PipeReader: reader, cancel: cancel, done: done}, nil
}
