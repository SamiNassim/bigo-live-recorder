package bigo

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/fault"
)

func TestBlockedRequestIdentifiesQueueWithoutSendingHTTP(t *testing.T) {
	var calls atomic.Int32
	c := NewClient()
	c.http.Transport = failingTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected request")
	})
	c.next = time.Now().Add(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Presence(ctx, "demo")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request queue") || calls.Load() != 0 {
		t.Fatal("queue timeout was confused with a sent request", err, calls.Load())
	}
}

type timeoutNativeConn struct{ net.Conn }

func (timeoutNativeConn) SetReadDeadline(time.Time) error { return nil }
func (timeoutNativeConn) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234}, Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.20"), Port: 5678}, Err: os.ErrDeadlineExceeded}
}

func TestNativeReceiveTimeoutIdentifiesTrackAndPreservesSocketDetail(t *testing.T) {
	for _, video := range []bool{false, true} {
		conn := nativeMediaConn{Conn: timeoutNativeConn{}, video: video}
		err := conn.receive(context.Background(), make(chan nativeMediaFrame, 1))
		track := "audio"
		if video {
			track = "video"
		}
		if !strings.Contains(err.Error(), "native "+track) || !strings.Contains(err.Error(), "20 seconds") || strings.Contains(err.Error(), "192.0.2") || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal("native timeout was vague or lost its cause", err)
		}
		if !strings.Contains(fault.Details(err), "192.0.2.10:1234") {
			t.Fatal("socket details unavailable for diagnostics")
		}
	}
}

func TestNativeRecoveryDistinguishesConfirmedEndFromExhaustedRetries(t *testing.T) {
	for _, offline := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed end", false: "unconfirmed interruption"}[offline], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failed := make(chan error, 1)
			failed <- io.ErrUnexpectedEOF
			session := &nativeMediaSession{ctx: ctx, cleanup: func() {}, workers: &sync.WaitGroup{}, failed: failed, events: make(chan nativeMediaFrame)}
			var messages, details []string
			attempts := 0
			c := &nativeCapture{ctx: ctx, cancel: cancel, queue: newNativeFrameQueue(1024), done: make(chan struct{}), session: session}
			c.recovery = StreamRecovery{
				Refresh: func(context.Context) (Stream, error) {
					attempts++
					if offline {
						return Stream{}, nil
					}
					return Stream{}, fault.Wrap("native source lookup timed out", context.DeadlineExceeded)
				},
				ReportDetails: func(level, message, detail string, cause error) {
					messages = append(messages, level+" "+message)
					details = append(details, detail+"\n"+fault.Details(cause))
				},
			}
			go c.receive(Stream{Alive: 1, UID: 123}, session)
			defer c.Close()
			select {
			case <-c.done:
			case <-time.After(15 * time.Second):
				t.Fatal("recovery did not finish within its retry bounds")
			}
			_, err := c.queue.next(context.Background())
			joined := strings.Join(messages, "\n")
			if offline {
				if attempts != 1 || !errors.Is(err, io.EOF) || !strings.Contains(joined, "INFO BIGO reports this broadcast ended") {
					t.Fatal("confirmed end was treated as an error", attempts, err, joined)
				}
			} else if attempts != 3 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "after 3 attempts") || strings.Contains(joined, "broadcast ended") || !strings.Contains(joined, "attempt 3 of 3") {
				t.Fatal("recovery failed to explain its attempts without claiming offline", attempts, err, joined)
			} else if !strings.Contains(strings.Join(details, "\n"), "failed during source refresh") || !strings.Contains(fault.Details(err), "Attempt 3 of 3") {
				t.Fatal("recovery did not retain the phase and attempt of its failures", details, fault.Details(err))
			}
		})
	}
}

func TestHTTPBackoffMessageMatchesEnforcedMinimum(t *testing.T) {
	for _, status := range []int{403, 429} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		c := NewClient()
		c.base, c.http = server.URL, server.Client()
		_, err := c.Presence(context.Background(), "demo")
		server.Close()
		var response *HTTPError
		if !errors.As(err, &response) || response.RetryAfter < 29*time.Second || response.RetryAfter > 31*time.Second {
			t.Fatal("reported backoff differs from enforced wait", err)
		}
		if strings.Contains(err.Error(), "after 0s") || !strings.Contains(err.Error(), "at least 30s") {
			t.Fatal("misleading backoff", err)
		}
		if difference := response.RetryAfter - time.Until(c.next); difference < 0 || difference > time.Second {
			t.Fatal("reported backoff does not match the request queue", difference)
		}
	}
	if text := (&HTTPError{Status: 503}).Error(); strings.Contains(text, "paused") || strings.Contains(text, "0s") {
		t.Fatal("invented a retry interval for an unthrottled response", text)
	}
}

func TestHTTPMessageDoesNotClaimAnUnscheduledBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := NewClient()
	c.base, c.http = server.URL, server.Client()
	_, err := c.Presence(context.Background(), "demo")
	if err == nil || strings.Contains(err.Error(), "paused") || !strings.Contains(fault.Details(err), "/OUserCenter/getUserInfoStudio") {
		t.Fatal("message claimed a delay not enforced by the request queue, or lost the endpoint", err)
	}
}

func TestRecoveryDiagnosticCallbackDoesNotDuplicateMainMessage(t *testing.T) {
	var plain, detailed int
	recovery := StreamRecovery{
		Report: func(string, string) { plain++ },
		ReportError: func(level, message string, err error) {
			detailed++
			if level != "WARN" || message != "Reconnecting stream" || !strings.Contains(fault.Details(err), "context deadline") {
				t.Error("lost recovery context", level, message, err)
			}
		},
	}
	recovery.reportError("WARN", "Reconnecting stream", context.DeadlineExceeded)
	if plain != 0 || detailed != 1 {
		t.Fatal("failure was duplicated", plain, detailed)
	}
	recovery.ReportError = nil
	recovery.reportError("WARN", "Reconnecting stream", context.DeadlineExceeded)
	if plain != 1 {
		t.Fatal("legacy reporter missed the warning")
	}
}
