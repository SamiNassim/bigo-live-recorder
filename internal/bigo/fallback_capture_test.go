package bigo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type nativeUpgradeFixture struct {
	ctx    context.Context
	taken  bool
	closed atomic.Bool
}

func (c *nativeUpgradeFixture) Next(context.Context) (*CapturePart, error) {
	if c.taken {
		return nil, io.EOF
	}
	c.taken = true
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	return &CapturePart{ReadCloser: io.NopCloser(strings.NewReader("native fixture")), Native: true}, nil
}
func (c *nativeUpgradeFixture) Close() error { c.closed.Store(true); return nil }

func TestFailedNativeOpenReturnsNoCapture(t *testing.T) {
	// An empty native route list fails before any network connection. A
	// failed constructor must not wrap a nil pointer in the Capture interface.
	capture, err := openNativeCapture(context.Background(), nil, Stream{native: &pJoinChannelRes{}}, "", StreamRecovery{})
	if err == nil || capture != nil {
		t.Fatalf("failed native startup returned capture=%T, error=%v", capture, err)
	}
}

func TestWebFallbackContinuesAfterRepeatedNativeStartupFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var playlists, attempts atomic.Int32
	a, b := hlsTestMedia('a'), hlsTestMedia('b')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live.m3u8":
			n := playlists.Add(1)
			name := "a"
			if n > 1 {
				name = "b"
			}
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n%s.ts\n", n, name)
		case "/a.ts":
			w.Write(a)
		case "/b.ts":
			w.Write(b)
		}
	}))
	defer server.Close()
	source := Stream{Alive: 1, UID: 900000001, RoomID: "8589934634", HLS: server.URL + "/live.m3u8"}
	recovery := StreamRecovery{Origin: func(context.Context, Stream) (Stream, error) {
		fresh := source
		fresh.native = &pJoinChannelRes{}
		return fresh, nil
	}}
	capture, err := newFallbackCapture(ctx, server.Client(), source, "https://www.bigo.tv/fixture", recovery,
		fallbackPolicy{time.Millisecond, 5 * time.Millisecond, 50 * time.Millisecond},
		func(ctx context.Context, fresh Stream) (Capture, error) {
			attempts.Add(1)
			return openNativeCapture(ctx, server.Client(), fresh, "", StreamRecovery{})
		})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	part, err := capture.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()
	got := make([]byte, len(a))
	if _, err := io.ReadFull(part, got); err != nil || !bytes.Equal(got, a) {
		t.Fatal("initial web media was lost", err)
	}
	for attempts.Load() < 3 {
		if err := waitHLS(ctx, time.Millisecond); err != nil {
			t.Fatal("native retries did not continue", err)
		}
	}
	if _, err := io.ReadFull(part, got); err != nil || !bytes.Equal(got, b) {
		t.Fatal("native startup failure interrupted web recording", err)
	}
}

func TestWebFallbackKeepsRecordingUntilNativeCandidateIsReady(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var playlistRequests, webChecks, nativeChecks atomic.Int32
	a, b := hlsTestMedia('a'), hlsTestMedia('b')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("account cookie reached a media host")
		}
		switch r.URL.Path {
		case "/live.m3u8":
			n := playlistRequests.Add(1)
			name := "a"
			if n > 1 {
				name = "b"
			}
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n%s.ts\n", n, name)
		case "/a.ts":
			w.Write(a)
		case "/b.ts":
			w.Write(b)
		}
	}))
	defer server.Close()
	source := Stream{Alive: 1, UID: 1234, RoomID: "8589934634", HLS: server.URL + "/live.m3u8"}
	entered, release := make(chan struct{}), make(chan struct{})
	var candidate *nativeUpgradeFixture
	capture, err := newFallbackCapture(ctx, server.Client(), source, "https://www.bigo.tv/demo", StreamRecovery{
		Refresh: func(context.Context) (Stream, error) { webChecks.Add(1); return source, nil },
		Origin: func(context.Context, Stream) (Stream, error) {
			nativeChecks.Add(1)
			fresh := source
			fresh.native = &pJoinChannelRes{}
			return fresh, nil
		},
	}, fallbackPolicy{time.Millisecond, 10 * time.Millisecond, 2 * time.Second}, func(ctx context.Context, _ Stream) (Capture, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		candidate = &nativeUpgradeFixture{ctx: ctx}
		return candidate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	part, err := capture.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(a))
	if _, err = io.ReadFull(part, got); err != nil || !bytes.Equal(got, a) {
		t.Fatal("initial fallback missing", err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("native candidate did not start")
	}
	if _, err = io.ReadFull(part, got); err != nil || !bytes.Equal(got, b) {
		t.Fatal("fallback stopped while native setup was pending", err)
	}
	close(release)
	if _, err = io.Copy(io.Discard, part); err != nil {
		t.Fatal("fallback cut a segment during promotion", err)
	}
	part.Close()
	part, err = capture.Next(ctx)
	if err != nil || !part.Native || part.Reason == "" {
		t.Fatal("native promotion did not create a ready part", err)
	}
	got, err = io.ReadAll(part)
	part.Close()
	if err != nil || string(got) != "native fixture" {
		t.Fatal(err)
	}
	if webChecks.Load() != 0 || nativeChecks.Load() != 1 {
		t.Fatal("upgrade polled the web API", webChecks.Load(), nativeChecks.Load())
	}
	// Successful candidate lifetime is independent of its setup deadline.
	if wait := time.Until(started.Add(2100 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if candidate.ctx.Err() != nil {
		t.Fatal("native candidate retained its expired setup context")
	}
	capture.Close()
	if !candidate.closed.Load() {
		t.Fatal("native candidate leaked after shutdown")
	}
}

func TestWebFallbackRejectsNativeUpgradeForAnotherRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "m3u8") {
			fmt.Fprint(w, "#EXTM3U\na.ts\n")
		} else {
			w.Write(hlsTestMedia('a'))
		}
	}))
	defer server.Close()
	var probes atomic.Int32
	capture, err := newFallbackCapture(ctx, server.Client(), Stream{Alive: 1, UID: 1234, RoomID: "42", HLS: server.URL + "/live.m3u8"}, "https://www.bigo.tv/demo", StreamRecovery{Origin: func(context.Context, Stream) (Stream, error) {
		probes.Add(1)
		return Stream{Alive: 1, UID: 1234, RoomID: "43", native: &pJoinChannelRes{}}, nil
	}}, fallbackPolicy{time.Millisecond, 5 * time.Millisecond, 50 * time.Millisecond}, func(context.Context, Stream) (Capture, error) {
		t.Error("another room was opened")
		return nil, errors.New("wrong room")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	deadline := time.Now().Add(100 * time.Millisecond)
	for probes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if probes.Load() < 2 {
		t.Fatal("fallback did not retry native-only probes")
	}
}
