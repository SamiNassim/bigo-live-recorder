package bigo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bigo-live-recorder/internal/fault"
)

const (
	maxPlaylistBytes = 1 << 20
	maxMasterDepth   = 4
	maxStreamStall   = 2 * time.Minute
	maxSegmentBytes  = 32 << 20
)

type hlsSegment struct {
	url           string
	sequence      uint64
	discontinuity uint64
	seed          *uint32
}

type hlsPlaylist struct {
	segments              []hlsSegment
	variant               string
	refresh               time.Duration
	end                   bool
	explicitDiscontinuity bool
}

type hlsReadCloser struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *hlsReadCloser) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}

// OpenStream opens an ordinary or BIGO-protected HLS stream without a browser.
// The returned reader emits the original media bytes, restoring only the two
// packet headers described by EXT-X-BIGO-WEB-PROTECTION. Closing the reader also
// cancels outstanding playlist/segment requests.
func OpenStream(ctx context.Context, client *http.Client, playlistURL, referer string) (io.ReadCloser, error) {
	return openHLSStream(ctx, client, Stream{HLS: playlistURL}, referer, StreamRecovery{}, defaultHLSPolicy(), false)
}

// OpenRecoveringStream keeps one reader open while recovering playback. API
// refreshes happen only after a stall/error/end marker, never during healthy
// recording. Account authentication stays in Refresh, isolated from media HTTP.
func OpenRecoveringStream(ctx context.Context, client *http.Client, source Stream, referer string, recovery StreamRecovery) (io.ReadCloser, error) {
	if source.native != nil {
		return openNativeRecoveringStream(ctx, client, source, referer, recovery)
	}
	return openHLSStream(ctx, client, source, referer, recovery, defaultHLSPolicy(), true)
}

func openHLSStream(ctx context.Context, client *http.Client, source Stream, referer string, recovery StreamRecovery, policy hlsPolicy, recoverPlayback bool) (io.ReadCloser, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	// A recording must remain a guest even if a caller supplies an app-owned
	// client that also has account cookies. Leave the caller's client untouched.
	guest := *client
	guest.Jar = nil
	client = &guest
	sources := source.URLs()
	if len(sources) == 0 {
		return nil, errors.New("BIGO HLS requires an HTTP(S) playback URL without embedded credentials")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	initialCtx, cancelInitial := context.WithTimeout(streamCtx, 30*time.Second)
	started := time.Now()
	var playlist hlsPlaylist
	var mediaURL, rootURL string
	var err error
	var tried int
	for _, rootURL = range sources {
		tried++
		playlist, mediaURL, err = loadHLSPlaylist(initialCtx, client, rootURL, referer)
		if err == nil {
			break
		}
	}
	cancelInitial()
	details := fmt.Sprintf("Web HLS playlist setup took %s. Playback sources tried: %d of %d.", time.Since(started).Round(time.Millisecond), tried, len(sources))
	if err != nil {
		cancel()
		return nil, fault.WithDetails(err, details)
	}
	if recoverPlayback {
		recovery.Log("INFO", "Web playback connected", details, nil)
	}
	reader, writer := io.Pipe()
	stream := &hlsReadCloser{PipeReader: reader, cancel: cancel, done: make(chan struct{})}
	stopCancellation := context.AfterFunc(streamCtx, func() {
		_ = writer.CloseWithError(streamCtx.Err())
	})
	go func() {
		defer close(stream.done)
		r := &hlsReader{client: client, output: writer, source: source, rootURL: rootURL, mediaURL: mediaURL, referer: referer, playlist: playlist, recovery: recovery, policy: policy, enabled: recoverPlayback, lastProgress: time.Now()}
		err := r.read(streamCtx)
		_ = writer.CloseWithError(err)
		stopCancellation()
		cancel()
	}()
	return stream, nil
}

func loadHLSPlaylist(ctx context.Context, client *http.Client, playlistURL, referer string) (hlsPlaylist, string, error) {
	for depth := 0; depth <= maxMasterDepth; depth++ {
		response, err := requestHLS(ctx, client, playlistURL, referer, true)
		if err != nil {
			return hlsPlaylist{}, "", err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxPlaylistBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			return hlsPlaylist{}, "", fmt.Errorf("read BIGO HLS playlist: %w", readErr)
		}
		if len(body) > maxPlaylistBytes {
			return hlsPlaylist{}, "", errors.New("BIGO HLS playlist exceeds 1 MiB")
		}
		base, _ := url.Parse(playlistURL)
		if response.Request != nil && response.Request.URL != nil {
			base = response.Request.URL
		}
		playlist, err := parseHLSPlaylist(body, base)
		if err != nil {
			return hlsPlaylist{}, "", err
		}
		if playlist.variant == "" {
			return playlist, base.String(), nil
		}
		playlistURL = playlist.variant
	}
	return hlsPlaylist{}, "", errors.New("BIGO HLS master playlist nesting exceeds four levels")
}

func parseHLSPlaylist(body []byte, base *url.URL) (hlsPlaylist, error) {
	playlist := hlsPlaylist{refresh: time.Second}
	scanner := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(string(body), "\ufeff")))
	scanner.Buffer(make([]byte, 4096), maxPlaylistBytes)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "#EXTM3U" {
		return playlist, errors.New("BIGO playback response is not an HLS playlist")
	}
	var (
		sequence      uint64
		discontinuity uint64
		seed          *uint32
		isVariant     bool
		bandwidth     uint64
		bestBandwidth uint64
		masterSeen    bool
	)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			var err error
			sequence, err = strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64)
			if err != nil {
				return playlist, errors.New("BIGO HLS has an invalid media sequence")
			}
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			var err error
			discontinuity, err = strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:")), 10, 64)
			if err != nil || len(playlist.segments) > 0 {
				return playlist, errors.New("BIGO HLS has an invalid discontinuity sequence")
			}
			playlist.explicitDiscontinuity = true
		case line == "#EXT-X-DISCONTINUITY":
			if discontinuity == ^uint64(0) {
				return playlist, errors.New("BIGO HLS discontinuity sequence overflows")
			}
			discontinuity++
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			seconds, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")), 10, 32)
			if err != nil || seconds == 0 {
				return playlist, errors.New("BIGO HLS has an invalid target duration")
			}
			playlist.refresh = time.Duration(seconds) * time.Second / 2
			if playlist.refresh > 5*time.Second {
				playlist.refresh = 5 * time.Second
			}
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if len(playlist.segments) > 0 {
				return playlist, errors.New("BIGO HLS mixes master variants and media segments")
			}
			attributes := hlsAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			var err error
			bandwidth, err = strconv.ParseUint(attributes["BANDWIDTH"], 10, 64)
			if err != nil {
				return playlist, errors.New("BIGO HLS master has an invalid bandwidth")
			}
			isVariant, masterSeen = true, true
		case strings.HasPrefix(line, "#EXT-X-BIGO-WEB-PROTECTION:"):
			attributes := hlsAttributes(strings.TrimPrefix(line, "#EXT-X-BIGO-WEB-PROTECTION:"))
			if attributes["VERSION"] != "1" {
				return playlist, errors.New("unsupported BIGO HLS web-protection version")
			}
			value, err := strconv.ParseUint(attributes["SEED"], 10, 32)
			if err != nil {
				return playlist, errors.New("BIGO HLS web-protection has an invalid seed")
			}
			currentSeed := uint32(value)
			seed = &currentSeed
		case strings.HasPrefix(line, "#EXT-X-KEY:") || strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			attributes := hlsAttributes(line[strings.IndexByte(line, ':')+1:])
			if attributes["METHOD"] != "NONE" {
				return playlist, errors.New("encrypted BIGO HLS keys are not supported by the native guest recorder")
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			return playlist, errors.New("BIGO HLS byte-range segments are not supported by the native guest recorder")
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			return playlist, errors.New("BIGO HLS initialization maps are not supported by the native guest recorder")
		case line == "#EXT-X-ENDLIST":
			playlist.end = true
		case strings.HasPrefix(line, "#"):
			continue
		default:
			resolved, err := hlsURL(base, line)
			if err != nil {
				return playlist, err
			}
			if isVariant {
				if playlist.variant == "" || bandwidth > bestBandwidth {
					playlist.variant, bestBandwidth = resolved, bandwidth
				}
				isVariant = false
				continue
			}
			if masterSeen {
				return playlist, errors.New("BIGO HLS mixes master variants and media segments")
			}
			playlist.segments = append(playlist.segments, hlsSegment{url: resolved, sequence: sequence, discontinuity: discontinuity, seed: seed})
			if sequence == ^uint64(0) {
				return playlist, errors.New("BIGO HLS media sequence overflows")
			}
			sequence++
		}
	}
	if err := scanner.Err(); err != nil {
		return playlist, fmt.Errorf("read BIGO HLS playlist lines: %w", err)
	}
	if isVariant {
		return playlist, errors.New("BIGO HLS master variant has no URL")
	}
	return playlist, nil
}

func hlsAttributes(raw string) map[string]string {
	attributes := make(map[string]string)
	start, quoted := 0, false
	for index := 0; index <= len(raw); index++ {
		if index < len(raw) && raw[index] == '"' {
			quoted = !quoted
		}
		if index == len(raw) || (raw[index] == ',' && !quoted) {
			key, value, ok := strings.Cut(raw[start:index], "=")
			if ok {
				attributes[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"")
			}
			start = index + 1
		}
	}
	return attributes
}

func hlsURL(base *url.URL, raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("BIGO HLS contains a malformed URL")
	}
	if base != nil {
		parsed = base.ResolveReference(parsed)
	}
	if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("BIGO HLS requires an HTTP(S) URL without embedded credentials")
	}
	return parsed.String(), nil
}

func requestHLS(ctx context.Context, client *http.Client, resourceURL, referer string, playlist bool) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := waitHLS(ctx, time.Duration(attempt)*250*time.Millisecond); err != nil {
				return nil, err
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, nil)
		if err != nil {
			var requestError *url.Error
			if errors.As(err, &requestError) {
				err = requestError.Err
			}
			return nil, fault.Wrap("could not create the BIGO HLS request", err)
		}
		setRequestHeaders(request, referer)
		if playlist {
			request.Header.Set("Cache-Control", "no-cache")
			request.Header.Set("Pragma", "no-cache")
		}
		response, err := client.Do(request)
		if err == nil {
			if response.StatusCode == http.StatusOK {
				return response, nil
			}
			_ = response.Body.Close()
			lastErr = fmt.Errorf("BIGO HLS resource returned HTTP %d", response.StatusCode)
			if response.StatusCode < 500 && response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusTooManyRequests {
				return nil, lastErr
			}
		} else {
			// net/http wraps errors with the complete signed resource URL. Keep
			// the underlying cause, but never return its token-bearing URL.
			var requestError *url.Error
			for errors.As(err, &requestError) {
				err = requestError.Err
			}
			lastErr = fmt.Errorf("request BIGO HLS resource: %w", err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func readHLSSegment(ctx context.Context, client *http.Client, segment hlsSegment, referer string) ([]byte, error) {
	response, err := requestHLS(ctx, client, segment.url, referer, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	// Commit only complete segments. Retrying after a partial network read must
	// not append the beginning of the same segment twice to FFmpeg's input.
	media, err := io.ReadAll(io.LimitReader(response.Body, maxSegmentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read BIGO HLS segment: %w", err)
	}
	if len(media) > maxSegmentBytes {
		return nil, errors.New("BIGO HLS segment exceeds 32 MiB")
	}
	if len(media) < 2*188 {
		return nil, errors.New("BIGO MPEG-TS segment is shorter than two packet headers")
	}
	if segment.seed != nil {
		restoreBigoHeaders(media[:2*188], *segment.seed)
	}
	if media[0] != 0x47 || media[188] != 0x47 {
		return nil, errors.New("BIGO HLS segment does not contain valid MPEG-TS headers")
	}
	return media, nil
}

// BIGO's VERSION=1 marker describes an XOR mask on the first 16 bytes of each
// of the first two 188-byte transport packets, with a separate xorshift state
// per packet. The remainder of every segment is already ordinary MPEG-TS.
func restoreBigoHeaders(prefix []byte, seed uint32) {
	for packet := uint32(0); packet < 2; packet++ {
		state := seed ^ ((packet + 1) * uint32(2654435769))
		if state == 0 {
			state = 1831565813
		}
		for offset := 0; offset < 16; offset++ {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			mask := byte(state)
			if mask == 0 {
				mask = 165
			}
			prefix[int(packet)*188+offset] ^= mask
		}
	}
}

func waitHLS(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
