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
	stream, err := openNativeStreamClock(ctx, source, clock, recovery.Report)
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
	if recovery.Report != nil {
		recovery.Report("INFO", "Native video and audio connected (Origin requested)")
	}
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
			recovery.reportError("WARN", "Stream data stopped arriving: "+fault.Reason(err)+". Attempting to reconnect while keeping the recording open.", err)
			recovered := false
			for attempt := 0; attempt < 3; attempt++ {
				if attempt > 0 {
					if waitHLS(ctx, 5*time.Second) != nil {
						break
					}
				}
				refreshCtx, done := context.WithTimeout(ctx, 25*time.Second)
				fresh, refreshErr := recovery.Refresh(refreshCtx)
				done()
				if refreshErr != nil {
					err = refreshErr
					continue
				}
				if fresh.Alive != 1 || (fresh.RoomID != "" && current.RoomID != "" && fresh.RoomID != current.RoomID) {
					writer.Close()
					return
				}
				if fresh.native != nil {
					stream, err = openNativeStreamClock(ctx, fresh, clock, recovery.Report)
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
			}
			if !recovered {
				writer.CloseWithError(fault.Wrap("could not restore the native stream after 3 attempts: "+fault.Reason(err), err))
				return
			}
			if recovery.Report != nil {
				recovery.Report("INFO", "BIGO media resumed; continuing the same recording file")
			}
		}
	}()
	return &nativeReadCloser{PipeReader: reader, cancel: cancel, done: done}, nil
}
