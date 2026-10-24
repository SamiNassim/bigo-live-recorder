package bigo

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenStreamSelectsBestVariantAndRestoresProtectedHeaders(t *testing.T) {
	t.Parallel()
	// Synthetic known-answer vectors cover both protected packet headers.
	// The remaining packet bytes must be preserved unchanged.
	protected := bytes.Repeat([]byte{0xff}, 3*188)
	expected := append([]byte(nil), protected...)
	for _, entry := range []struct {
		offset     int
		ciphertext string
		plaintext  string
	}{
		{0, "f48de44b2088b28ed1351ffc98d34d75", "470020100102030405060708090a0b0c"},
		{188, "dfeed0e85eaaaf31d5a5d659363af98f", "470030100d0e0f101112131415161718"},
	} {
		ciphertext, err := hex.DecodeString(entry.ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := hex.DecodeString(entry.plaintext)
		if err != nil {
			t.Fatal(err)
		}
		copy(protected[entry.offset:], ciphertext)
		copy(expected[entry.offset:], plaintext)
	}
	protected[376], expected[376] = 0x47, 0x47
	var lowRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Referer") != "https://www.bigo.tv/test" {
			t.Errorf("unexpected referer %q", request.Header.Get("Referer"))
		}
		if request.Header.Get("Cookie") != "" {
			t.Error("guest media request contained an account cookie")
		}
		switch request.URL.Path {
		case "/master.m3u8":
			fmt.Fprint(writer, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100,CODECS=\"avc1,mp4a\"\nlow/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=200\nhigh/index.m3u8\n")
		case "/low/index.m3u8":
			lowRequests.Add(1)
			writer.WriteHeader(http.StatusNotFound)
		case "/high/index.m3u8":
			fmt.Fprint(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:101\n#EXT-X-BIGO-WEB-PROTECTION:VERSION=1,SEED=1122334455\n#EXTINF:2,\n../media/a.ts?part=1\n#EXT-X-ENDLIST\n")
		case "/media/a.ts":
			if request.URL.Query().Get("part") != "1" {
				t.Error("relative segment query was lost")
			}
			for start := 0; start < len(protected); start += 7 {
				end := min(start+7, len(protected))
				_, _ = writer.Write(protected[start:end])
			}
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, server.Client(), server.URL+"/master.m3u8", "https://www.bigo.tv/test")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	actual, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("restored stream differs: got %x, want %x", actual[:min(16, len(actual))], expected[:16])
	}
	if lowRequests.Load() != 0 {
		t.Fatal("lower-bandwidth variant was requested")
	}
}

func TestOpenStreamRefreshDoesNotRepeatSegments(t *testing.T) {
	t.Parallel()
	var playlists, first, second atomic.Int32
	firstBytes, secondBytes := hlsTestMedia('a'), hlsTestMedia('b')
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/index.m3u8":
			count := playlists.Add(1)
			fmt.Fprint(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:1,\na.ts\n")
			if count > 1 {
				fmt.Fprint(writer, "#EXTINF:1,\nb.ts\n#EXT-X-ENDLIST\n")
			}
		case "/a.ts":
			first.Add(1)
			_, _ = writer.Write(firstBytes)
		case "/b.ts":
			second.Add(1)
			_, _ = writer.Write(secondBytes)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, server.Client(), server.URL+"/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	actual, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, append(firstBytes, secondBytes...)) || first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("stream bytes=%d first=%d second=%d", len(actual), first.Load(), second.Load())
	}
}

func TestOpenStreamSequenceResetRequiresNewDiscontinuity(t *testing.T) {
	t.Parallel()
	var playlists atomic.Int32
	firstBytes, secondBytes, thirdBytes := hlsTestMedia('a'), hlsTestMedia('b'), hlsTestMedia('c')
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/index.m3u8":
			switch playlists.Add(1) {
			case 1, 3:
				// The third response simulates an out-of-date CDN playlist after
				// we have already accepted the new discontinuity epoch.
				fmt.Fprint(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:100\n#EXT-X-DISCONTINUITY-SEQUENCE:7\n#EXTINF:1,\na.ts\n")
			case 2:
				fmt.Fprint(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-DISCONTINUITY-SEQUENCE:7\n#EXT-X-DISCONTINUITY\n#EXTINF:1,\nb.ts\n")
			default:
				fmt.Fprint(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-DISCONTINUITY-SEQUENCE:8\n#EXTINF:1,\nb.ts\n#EXTINF:1,\nc.ts\n#EXT-X-ENDLIST\n")
			}
		case "/a.ts":
			_, _ = writer.Write(firstBytes)
		case "/b.ts":
			_, _ = writer.Write(secondBytes)
		case "/c.ts":
			_, _ = writer.Write(thirdBytes)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, server.Client(), server.URL+"/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	actual, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, append(append(firstBytes, secondBytes...), thirdBytes...)) {
		t.Fatalf("new discontinuity was lost after media sequence reset: bytes=%d", len(actual))
	}
}

func TestOpenStreamIsolatesCallerAccountCookies(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if value := request.Header.Get("Cookie"); value != "" {
			t.Errorf("account cookie reached media server: %q", value)
		}
		if request.URL.Path == "/index.m3u8" {
			http.SetCookie(writer, &http.Cookie{Name: "new_cookie", Value: "ignored"})
			fmt.Fprint(writer, "#EXTM3U\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n")
			return
		}
		_, _ = writer.Write(hlsTestMedia('a'))
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverURL, _ := url.Parse(server.URL)
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "uniqid", Value: "account-cookie"}})
	client := server.Client()
	client.Jar = jar
	stream, err := OpenStream(context.Background(), client, server.URL+"/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := io.Copy(io.Discard, stream); err != nil {
		t.Fatal(err)
	}
	if client.Jar != jar || len(jar.Cookies(serverURL)) != 1 {
		t.Fatal("OpenStream mutated the caller's account cookie jar")
	}
}

func TestOpenStreamDoesNotExposeSignedURLsInRequestErrors(t *testing.T) {
	t.Parallel()
	transport := hlsTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: request.URL.String(), Err: errors.New("fixture network failure")}
	})
	_, err := OpenStream(context.Background(), &http.Client{Transport: transport}, "https://cdn.example/index.m3u8?auth-token=secret", "")
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "auth-token") {
		t.Fatalf("request error exposed signed URL: %v", err)
	}
	if !strings.Contains(err.Error(), "fixture network failure") {
		t.Fatalf("request failure cause was lost: %v", err)
	}
}

func TestOpenStreamRejectsNonTransportStreamResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/index.m3u8" {
			fmt.Fprint(writer, "#EXTM3U\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n")
			return
		}
		fmt.Fprint(writer, strings.Repeat("<html>upstream login required</html>", 20))
	}))
	defer server.Close()
	stream, err := OpenStream(context.Background(), server.Client(), server.URL+"/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	actual, err := io.ReadAll(stream)
	if err == nil || len(actual) != 0 {
		t.Fatalf("invalid segment reached recording output: bytes=%d error=%v", len(actual), err)
	}
}

type hlsTestRoundTripper func(*http.Request) (*http.Response, error)

func (transport hlsTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func hlsTestMedia(value byte) []byte {
	media := bytes.Repeat([]byte{value}, 2*188)
	media[0], media[188] = 0x47, 0x47
	return media
}

func TestOpenStreamCancellationUnblocksReader(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:1\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := OpenStream(ctx, server.Client(), server.URL+"/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(stream)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read error=%v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader stayed blocked after cancellation")
	}
}

func TestOpenStreamCloseUnblocksBackpressuredWriter(t *testing.T) {
	t.Parallel()
	segmentStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/index.m3u8" {
			fmt.Fprint(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\na.ts\n#EXT-X-ENDLIST\n")
			return
		}
		close(segmentStarted)
		_, _ = writer.Write(bytes.Repeat([]byte{0x47}, 128*1024))
	}))
	defer server.Close()
	stream, err := OpenStream(context.Background(), server.Client(), server.URL+"/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-segmentStarted:
	case <-time.After(time.Second):
		t.Fatal("segment request did not start")
	}
	// Deliberately never read the pipe, as when FFmpeg has exited early.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.(*hlsReadCloser).done:
	case <-time.After(time.Second):
		t.Fatal("segment writer stayed blocked after Close")
	}
}

func TestOpenStreamRejectsUnsupportedOrMalformedProtection(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{
		"#EXT-X-BIGO-WEB-PROTECTION:VERSION=2,SEED=1",
		"#EXT-X-BIGO-WEB-PROTECTION:VERSION=1,SEED=invalid",
		"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"",
		"#EXT-X-BYTERANGE:188@0",
	} {
		t.Run(tag, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				fmt.Fprintf(writer, "#EXTM3U\n%s\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n", tag)
			}))
			defer server.Close()
			stream, err := OpenStream(context.Background(), server.Client(), server.URL+"/index.m3u8", "")
			if err == nil {
				_ = stream.Close()
				t.Fatal("unsupported/malformed playlist was accepted")
			}
			if !strings.Contains(err.Error(), "BIGO") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
