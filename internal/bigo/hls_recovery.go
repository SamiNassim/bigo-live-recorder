package bigo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"
)

// StreamRecovery supplies authenticated playback refresh and URL-free logging.
// Refresh is bounded to three calls per interrupted-media episode. It must honor
// cancellation and should share the app's normal request-rate limiter.
type StreamRecovery struct {
	Refresh func(context.Context) (Stream, error)
	Report  func(level, message string)
	// Origin retries native playback only while a web fallback is in use. It
	// must use the existing room and shared messaging login, without polling
	// web presence or playback APIs.
	Origin  func(context.Context, Stream) (Stream, error)
	stopHLS func() bool
}

type hlsPolicy struct {
	stallAfter, maxStall, retryDelay, refreshEvery, sourceCooldown time.Duration
	pollInterval                                                   time.Duration // Nonzero only in short, deterministic fixture tests.
}

func defaultHLSPolicy() hlsPolicy {
	return hlsPolicy{stallAfter: 20 * time.Second, maxStall: maxStreamStall, retryDelay: 2 * time.Second, refreshEvery: 45 * time.Second, sourceCooldown: 10 * time.Second}
}

type hlsReader struct {
	client                     *http.Client
	output                     io.Writer
	source                     Stream
	rootURL, mediaURL, referer string
	playlist                   hlsPlaylist
	ready                      []hlsSegment
	cursor                     hlsCursor
	recovery                   StreamRecovery
	policy                     hlsPolicy
	enabled, recovering        bool
	lastProgress, nextRefresh  time.Time
	refreshAttempts            int
	failedSources              map[string]time.Time
}

func (r *hlsReader) report(level, message string) {
	if r.recovery.Report != nil {
		r.recovery.Report(level, message)
	}
}

func (r *hlsReader) beginRecovery(reason string) {
	if !r.recovering {
		r.recovering = true
		r.report("WARN", reason+"; recovering sources inside the current recording")
	}
}

func (r *hlsReader) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, r.lastProgress.Add(r.policy.maxStall))
}

func (r *hlsReader) promoted() bool {
	return r.recovery.stopHLS != nil && r.recovery.stopHLS()
}

func (r *hlsReader) read(ctx context.Context) error {
	for {
		if r.promoted() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		segments := r.ready
		r.ready = nil
		if segments == nil {
			segments = r.cursor.fresh(r.playlist, r.mediaURL)
		}
		for _, segment := range segments {
			if r.promoted() {
				return nil
			}
			requestCtx, cancel := r.requestContext(ctx)
			media, err := readHLSSegment(requestCtx, r.client, segment, r.referer)
			cancel()
			if err != nil {
				if !r.enabled {
					return err
				}
				r.failSource()
				r.beginRecovery("HLS segment download failed")
				break
			}
			if n, err := r.output.Write(media); err != nil {
				return err
			} else if n != len(media) {
				return io.ErrShortWrite
			}
			r.cursor.accept(segment, r.playlist)
			r.lastProgress = time.Now()
			if r.recovering {
				r.report("INFO", "HLS playback resumed; continuing the same recording file")
				r.recovering = false
				r.refreshAttempts = 0
				r.nextRefresh = time.Time{}
				clear(r.failedSources)
			}
		}
		if r.promoted() {
			return nil
		}
		if r.playlist.end && !r.recovering {
			if !r.enabled || r.recovery.Refresh == nil {
				return nil
			}
			r.recovering = true
			r.report("INFO", "HLS playlist end marker received; verifying whether the broadcast ended")
		}
		if time.Since(r.lastProgress) >= r.policy.maxStall {
			return errors.New("BIGO playback recovery exhausted after two minutes without new media")
		}
		if time.Since(r.lastProgress) >= r.policy.stallAfter {
			r.beginRecovery("HLS playlist stopped advancing")
		}
		if r.recovering {
			loaded, ended := r.recover(ctx)
			if ended {
				return nil
			}
			if loaded {
				continue
			}
			if err := waitHLS(ctx, r.policy.retryDelay); err != nil {
				return err
			}
			// Do not re-consume a failed/stale playlist between recovery attempts.
			r.playlist.segments = nil
			continue
		}
		interval := r.playlist.refresh
		if r.policy.pollInterval > 0 {
			interval = r.policy.pollInterval
		}
		if err := waitHLS(ctx, interval); err != nil {
			return err
		}
		requestCtx, cancel := r.requestContext(ctx)
		playlistCtx, cancelPlaylist := context.WithTimeout(requestCtx, 5*time.Second)
		playlist, mediaURL, err := loadHLSPlaylist(playlistCtx, r.client, r.mediaURL, r.referer)
		cancelPlaylist()
		cancel()
		if err != nil {
			if !r.enabled {
				return err
			}
			r.failSource()
			r.beginRecovery("HLS playlist request failed")
			r.playlist.segments = nil
		} else {
			r.playlist, r.mediaURL = playlist, mediaURL
		}
	}
}

func (r *hlsReader) failSource() {
	if r.failedSources == nil {
		r.failedSources = make(map[string]time.Time)
	}
	r.failedSources[r.rootURL] = time.Now()
}

func (r *hlsReader) trySources(ctx context.Context) bool {
	urls := r.source.URLs()
	// Prefer another advertised CDN before revisiting the stalled source.
	if i := slices.Index(urls, r.rootURL); i >= 0 {
		urls = append(slices.Delete(urls, i, i+1), r.rootURL)
	}
	if r.mediaURL != "" && !slices.Contains(urls, r.mediaURL) {
		urls = append(urls, r.mediaURL)
	}
	for _, source := range urls {
		if ctx.Err() != nil || r.promoted() || time.Since(r.lastProgress) >= r.policy.maxStall {
			return false
		}
		if failed := r.failedSources[source]; !failed.IsZero() && time.Since(failed) < r.policy.sourceCooldown {
			continue
		}
		deadlineCtx, cancelDeadline := r.requestContext(ctx)
		requestCtx, cancel := context.WithTimeout(deadlineCtx, 5*time.Second)
		playlist, mediaURL, err := loadHLSPlaylist(requestCtx, r.client, source, r.referer)
		cancel()
		cancelDeadline()
		if err != nil {
			continue
		}
		segments := r.cursor.fresh(playlist, mediaURL)
		if len(segments) == 0 {
			continue
		}
		r.playlist, r.mediaURL, r.rootURL, r.ready = playlist, mediaURL, source, segments
		return true
	}
	return false
}

func (r *hlsReader) recover(ctx context.Context) (loaded, ended bool) {
	if r.promoted() {
		return false, true
	}
	if r.trySources(ctx) {
		return true, false
	}
	if ctx.Err() != nil || r.promoted() || time.Since(r.lastProgress) >= r.policy.maxStall || r.recovery.Refresh == nil || r.refreshAttempts >= 3 || time.Now().Before(r.nextRefresh) {
		return false, false
	}
	r.refreshAttempts++
	r.nextRefresh = time.Now().Add(r.policy.refreshEvery)
	requestCtx, cancel := r.requestContext(ctx)
	refreshCtx, cancelRefresh := context.WithTimeout(requestCtx, 30*time.Second)
	current, err := r.recovery.Refresh(refreshCtx)
	cancelRefresh()
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, false
		}
		r.report("WARN", "BIGO playback refresh failed; retrying media sources without closing the recording")
		return false, false
	}
	if current.Alive == 1 && current.RoomID != "" && r.source.RoomID != "" && current.RoomID != r.source.RoomID {
		r.report("INFO", "BIGO broadcast changed; finalizing the previous recording")
		return false, true
	}
	if current.Alive != 1 && r.playlist.end {
		return false, true
	}
	if current.Alive != 1 || len(current.URLs()) == 0 {
		return false, false
	}
	// A stale or withheld refresh must not discard working fallback candidates.
	current.CDN = append(current.CDN, r.source.URLs()...)
	if current.RoomID == "" {
		current.RoomID = r.source.RoomID
	}
	r.source = current
	return r.trySources(ctx), false
}
