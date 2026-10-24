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

func shortHLSPolicy() hlsPolicy {
	return hlsPolicy{stallAfter: 25 * time.Millisecond, maxStall: 500 * time.Millisecond, retryDelay: 5 * time.Millisecond, refreshEvery: 70 * time.Millisecond, sourceCooldown: 20 * time.Millisecond, pollInterval: 5 * time.Millisecond}
}

func recoveryStream(t *testing.T, server *httptest.Server, source Stream, recovery StreamRecovery) *hlsReadCloser {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	stream, err := openHLSStream(ctx, server.Client(), source, "https://www.bigo.tv/test", recovery, shortHLSPolicy(), true)
	if err != nil {
		t.Fatal(err)
	}
	r := stream.(*hlsReadCloser)
	t.Cleanup(func() { r.Close(); <-r.done })
	return r
}

func readRecoveryMedia(t *testing.T, stream *hlsReadCloser, expected []byte) {
	t.Helper()
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(stream, actual); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("recovery replayed or lost media")
	}
	stream.Close()
	<-stream.done
}

func TestStalledMediaReloadsOriginalMasterInsideSameReader(t *testing.T) {
	var masters atomic.Int32
	a, b := hlsTestMedia('a'), hlsTestMedia('b')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") && r.Header.Get("Cache-Control") != "no-cache" {
			t.Error("playlist cache revalidation missing")
		}
		switch r.URL.Path {
		case "/master.m3u8":
			variant := "old.m3u8"
			if masters.Add(1) > 1 {
				variant = "new.m3u8"
			}
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=200\n%s\n", variant)
		case "/old.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:100\na.ts\n")
		case "/new.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:101\nb.ts\n#EXT-X-ENDLIST\n")
		case "/a.ts":
			w.Write(a)
		case "/b.ts":
			w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/master.m3u8"}, StreamRecovery{})
	actual, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(actual, append(a, b...)) || masters.Load() != 2 {
		t.Fatal(len(actual), masters.Load(), err)
	}
}

func TestStalledCDNFailsOverWithoutAccountRequestsOrDuplicateSegments(t *testing.T) {
	var checks, firstSegments atomic.Int32
	a, b := hlsTestMedia('a'), hlsTestMedia('b')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("account credentials reached media CDN")
		}
		switch r.URL.Path {
		case "/old.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:100\na.ts\n")
		case "/mirror.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:100\na.ts\nb.ts\n")
		case "/a.ts":
			firstSegments.Add(1)
			w.Write(a)
		case "/b.ts":
			w.Write(b)
		}
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/old.m3u8", CDN: []string{server.URL + "/mirror.m3u8"}}, StreamRecovery{Refresh: func(context.Context) (Stream, error) { checks.Add(1); return Stream{}, ErrURLWithheld }})
	readRecoveryMedia(t, r, append(a, b...))
	if checks.Load() != 0 || firstSegments.Load() != 1 {
		t.Fatal("unexpected API check or duplicate segment", checks.Load(), firstSegments.Load())
	}
}

func TestExpiredPlaybackURLRefreshesAndKeepsReaderOpen(t *testing.T) {
	var playlists, checks atomic.Int32
	a, b := hlsTestMedia('a'), hlsTestMedia('b')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old.m3u8":
			if playlists.Add(1) > 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:100\na.ts\n")
		case "/new.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:101\nb.ts\n")
		case "/a.ts":
			w.Write(a)
		case "/b.ts":
			w.Write(b)
		}
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/old.m3u8", RoomID: "same-room"}, StreamRecovery{Refresh: func(context.Context) (Stream, error) {
		checks.Add(1)
		return Stream{Alive: 1, RoomID: "same-room", HLS: server.URL + "/new.m3u8?token=private-fixture"}, nil
	}, Report: func(_, message string) {
		if strings.Contains(message, "private-fixture") {
			t.Error("recovery log exposed signed URL")
		}
	}})
	readRecoveryMedia(t, r, append(a, b...))
	if checks.Load() != 1 {
		t.Fatal("refresh was not event-triggered", checks.Load())
	}
}

func TestTruncatedSegmentIsRetriedWithoutWritingPartialMedia(t *testing.T) {
	media := hlsTestMedia('a')
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.m3u8" {
			fmt.Fprint(w, "#EXTM3U\na.ts\n#EXT-X-ENDLIST\n")
			return
		}
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "1000")
		}
		w.Write(media)
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/live.m3u8"}, StreamRecovery{})
	actual, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(actual, media) || requests.Load() != 2 {
		t.Fatal("partial segment reached output", len(actual), requests.Load(), err)
	}
}

func TestHealthyRecordingNeverRefreshesAccountPlayback(t *testing.T) {
	var playlists, checks atomic.Int32
	media := hlsTestMedia('a')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.m3u8" {
			n := playlists.Add(1)
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:%d\n%d.ts\n", n, n)
			return
		}
		w.Write(media)
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/live.m3u8"}, StreamRecovery{Refresh: func(context.Context) (Stream, error) { checks.Add(1); return Stream{}, nil }})
	readRecoveryMedia(t, r, bytes.Repeat(media, 4))
	if checks.Load() != 0 {
		t.Fatal("healthy recording made account API requests", checks.Load())
	}
}

func TestRecoveryHasBoundedDurationAndAccountRefreshBudget(t *testing.T) {
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "#EXTM3U\n") }))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/live.m3u8"}, StreamRecovery{Refresh: func(context.Context) (Stream, error) { checks.Add(1); return Stream{}, ErrURLWithheld }})
	_, err := io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "recovery exhausted") {
		t.Fatal(err)
	}
	if checks.Load() != 3 {
		t.Fatal("incorrect account refresh budget", checks.Load())
	}
}

func TestOfflineRefreshAloneDoesNotEndRecoverableMedia(t *testing.T) {
	var checks atomic.Int32
	a, b := hlsTestMedia('a'), hlsTestMedia('b')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:100\na.ts\n")
		case "/new.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:101\nb.ts\n")
		case "/a.ts":
			w.Write(a)
		case "/b.ts":
			w.Write(b)
		}
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/old.m3u8"}, StreamRecovery{Refresh: func(context.Context) (Stream, error) {
		if checks.Add(1) == 1 {
			return Stream{}, nil
		}
		return Stream{Alive: 1, HLS: server.URL + "/new.m3u8"}, nil
	}})
	readRecoveryMedia(t, r, append(a, b...))
	if checks.Load() != 2 {
		t.Fatal("a single offline result prematurely ended recovery", checks.Load())
	}
}

func TestNewBroadcastFinalizesRatherThanAppendingDifferentRoom(t *testing.T) {
	var newRequests atomic.Int32
	media := hlsTestMedia('a')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old.m3u8":
			fmt.Fprint(w, "#EXTM3U\na.ts\n")
		case "/new.m3u8":
			newRequests.Add(1)
			fmt.Fprint(w, "#EXTM3U\nb.ts\n")
		default:
			w.Write(media)
		}
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/old.m3u8", RoomID: "old-room"}, StreamRecovery{Refresh: func(context.Context) (Stream, error) {
		return Stream{Alive: 1, RoomID: "new-room", HLS: server.URL + "/new.m3u8"}, nil
	}})
	actual, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(actual, media) || newRequests.Load() != 0 {
		t.Fatal(len(actual), newRequests.Load(), err)
	}
}

func TestEndMarkerAndOfflineResponseFinishGracefully(t *testing.T) {
	var checks atomic.Int32
	media := hlsTestMedia('a')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.m3u8" {
			fmt.Fprint(w, "#EXTM3U\na.ts\n#EXT-X-ENDLIST\n")
			return
		}
		w.Write(media)
	}))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/live.m3u8"}, StreamRecovery{Refresh: func(context.Context) (Stream, error) { checks.Add(1); return Stream{}, nil }})
	actual, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(actual, media) || checks.Load() != 1 {
		t.Fatal(len(actual), checks.Load(), err)
	}
}

func TestCancelDuringPlaybackRefreshStopsPromptly(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "#EXTM3U\n") }))
	defer server.Close()
	r := recoveryStream(t, server, Stream{HLS: server.URL + "/live.m3u8"}, StreamRecovery{Refresh: func(ctx context.Context) (Stream, error) { close(entered); <-ctx.Done(); return Stream{}, ctx.Err() }})
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, r); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	r.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled recovery stayed blocked")
	}
}
