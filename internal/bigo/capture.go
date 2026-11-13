package bigo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"bigo-live-recorder/internal/fault"
)

// Capture separates reception from file lifetimes. Next is called after the
// preceding part reaches EOF. Closing a part never closes the capture.
type Capture interface {
	Next(context.Context) (*CapturePart, error)
	Close() error
}

type CapturePart struct {
	io.ReadCloser
	Reason                     string
	Native                     bool
	AudioOnly                  bool
	ParticipantUID             uint32
	ParticipantID              string
	ParticipantName            string
	ParticipantIdentityVersion int
	BroadcastID                string
	Directory                  string
	Timing                     *CaptureTiming
}

func (p *CapturePart) NativeTimestamps() bool { return p.Native }

var errMultiGuestCapture = errors.New("multi-guest rooms require separate participant captures")

func OpenCapture(ctx context.Context, client *http.Client, source Stream, referer string, recovery StreamRecovery, splitInterval time.Duration) (Capture, error) {
	if source.RoomKind == RoomAudioMultiGuest || source.RoomKind == RoomHybridMultiGuest {
		return nil, errMultiGuestCapture
	}
	if source.native != nil {
		capture, err := openNativeCapture(ctx, client, source, referer, recovery, splitInterval)
		if err == nil {
			return capture, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if source.URL() == "" {
			return nil, err
		}
		recovery.reportError("WARN", "Native media could not start: "+fault.Reason(err)+". Trying the web stream instead.", err)
	}
	return openWebCapture(ctx, client, source, referer, recovery, splitInterval)
}

func openNativeCapture(ctx context.Context, client *http.Client, source Stream, referer string, recovery StreamRecovery, splitInterval time.Duration) (Capture, error) {
	capture, err := newNativeCaptureWithFallback(ctx, source, recovery, func(ctx context.Context, fresh Stream) (Capture, error) {
		return openWebCapture(ctx, client, fresh, referer, recovery, splitInterval)
	})
	if err != nil {
		return nil, err
	}
	capture.splitInterval = splitInterval
	return capture, nil
}

func openWebCapture(ctx context.Context, client *http.Client, source Stream, referer string, recovery StreamRecovery, splitInterval time.Duration) (Capture, error) {
	if recovery.Origin != nil {
		return newFallbackCapture(ctx, client, source, referer, recovery, defaultFallbackPolicy(), func(ctx context.Context, fresh Stream) (Capture, error) {
			return openNativeCapture(ctx, client, fresh, referer, recovery, splitInterval)
		})
	}
	stream, err := openHLSStream(ctx, client, source, referer, recovery, defaultHLSPolicy(), true)
	if err != nil {
		return nil, err
	}
	return &singleCapture{stream: stream}, nil
}

type singleCapture struct {
	stream io.ReadCloser
	taken  bool
}

func (c *singleCapture) Next(context.Context) (*CapturePart, error) {
	if c.taken {
		return nil, io.EOF
	}
	c.taken = true
	return &CapturePart{ReadCloser: c.stream}, nil
}
func (c *singleCapture) Close() error { return c.stream.Close() }
