package bigo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"bigo-live-recorder/internal/fault"
)

const nativeAudioRecoveryTimeout = 6 * time.Second
const nativeAudioRecoveryVideoAge = 2 * time.Second
const nativeAudioRecoveryMaxLagMS = 1000
const nativeAudioRecoveryMaxLeadMS = 5000

func (s *nativeMediaSession) canRecoverAudio(err error) bool {
	var failed *nativeTrackFailure
	if s.video == nil || !errors.As(err, &failed) || failed.conn == nil || failed.conn.video {
		return false
	}
	if s.audioRetried && failed.at.Sub(failed.conn.setupStarted) < 2*nativeAudioRecoveryTimeout {
		return false // Avoid repeatedly extending the same interruption.
	}
	return s.videoRecentlyPlayable(failed.at)
}

func (s *nativeMediaSession) videoRecentlyPlayable(at time.Time) bool {
	elapsed := s.video.lastPlayable.Load()
	return elapsed > 0 && at.Sub(s.video.setupStarted.Add(time.Duration(elapsed-1))) <= nativeAudioRecoveryVideoAge
}

// Keep consuming video through one retry with the existing route and, if
// needed, one with fresh room details.
func (c *nativeCapture) recoverAudio(source *Stream, session *nativeMediaSession, order *nativeMediaOrder, cause error) error {
	session.audioRetried = true
	started := time.Now()
	c.recovery.Log("WARN", "Origin audio interrupted. Keeping video open.", "Trying the current room's audio route while continuing to record video.", cause)
	fresh, err := c.tryAudio(*source, session, order, started, false)
	if err != nil && c.recovery.Room != nil && source.RoomID != "" && source.RoomID != "0" &&
		session.ctx.Err() == nil && session.videoRecentlyPlayable(time.Now()) &&
		!fault.RequiresAction(err) && !errors.Is(err, errNativeMediaOrderClock) {
		var failed *nativeTrackFailure
		if !errors.As(err, &failed) || failed.conn == nil || !failed.conn.video {
			c.recovery.Log("WARN", "Origin audio retry failed. Refreshing the room.", "Video remains open while obtaining a fresh native audio route.", err)
			fresh, err = c.tryAudio(*source, session, order, started, true)
		}
	}
	if err == nil {
		*source = fresh
	}
	return err
}

func (c *nativeCapture) tryAudio(source Stream, session *nativeMediaSession, order *nativeMediaOrder, started time.Time, refresh bool) (Stream, error) {
	attemptStarted := time.Now()
	ctx, cancel := context.WithTimeout(session.ctx, nativeAudioRecoveryTimeout)
	defer cancel()
	type result struct {
		conn    *nativeMediaConn
		source  Stream
		refresh time.Duration
		err     error
	}
	ready := make(chan result)
	go func() {
		opened := result{source: source}
		if refresh {
			roomCtx, stop := context.WithTimeout(ctx, nativeRoomRefreshTimeout)
			opened.source, opened.err = c.recovery.Room(roomCtx, source)
			stop()
			opened.refresh = time.Since(attemptStarted)
			if opened.err == nil {
				fresh := opened.source
				if fresh.Alive != 1 || fresh.RoomID != source.RoomID || fresh.RoomKind != source.RoomKind ||
					(fresh.UID != 0 && source.UID != 0 && fresh.UID != source.UID) {
					opened.err = errors.New("native audio refresh reported a different broadcast or room mode")
				} else if fresh.native == nil || len(fresh.native.media) == 0 || len(fresh.native.cookie) == 0 {
					opened.err = ErrOriginUnavailable
				}
			}
			if opened.err != nil {
				opened.err = fault.Wrap("could not refresh the Origin audio route", opened.err)
			}
		}
		if opened.err == nil {
			channel := opened.source.native
			opened.conn, opened.err = dialNativeMedia(ctx, channel.media, channel.source, channel.sid, false)
			if opened.err == nil {
				stop := context.AfterFunc(ctx, func() { opened.conn.Close() })
				opened.err = opened.conn.login(channel, opened.source.UID)
				stop()
			}
			if opened.err != nil {
				opened.err = fault.Wrap("could not reopen the Origin audio socket", opened.err)
			}
		}
		select {
		case ready <- opened:
		case <-ctx.Done():
			if opened.conn != nil {
				opened.conn.Close()
			}
		}
	}()
	var connected time.Time
	var refreshTime time.Duration
	var replacement *nativeMediaConn
	var stopTrack func()
	defer func() {
		if stopTrack != nil {
			stopTrack()
		} else if replacement != nil {
			replacement.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return Stream{}, fault.Wrap("audio-only recovery did not restore synchronized audio", ctx.Err())
		case err := <-session.failed:
			var failed *nativeTrackFailure
			if refresh && errors.As(err, &failed) && failed.conn != nil && !failed.conn.video && failed.conn != replacement {
				continue // This receiver was stopped before the room refresh.
			}
			return Stream{}, err
		case opened := <-ready:
			replacement = opened.conn
			if opened.err != nil {
				return Stream{}, opened.err
			}
			if refresh && !session.videoRecentlyPlayable(time.Now()) {
				return Stream{}, errors.New("video stopped arriving during the native audio refresh")
			}
			// Only the original audio is already aligned. A failed replacement
			// may have queued stale samples just before reporting its failure.
			if err := c.drainNativeMedia(session, order, !refresh); err != nil {
				return Stream{}, err
			}
			source, refreshTime = opened.source, opened.refresh
			connected = time.Now()
			replacement.setupStarted = connected
			if stopTrack = session.startTrack(replacement, source.native); stopTrack == nil {
				return Stream{}, context.Canceled
			}
		case frame := <-session.events:
			if !connected.IsZero() && !frame.video {
				delta := int32(frame.pts - order.videoPTS)
				stamp := int64(int32(frame.pts-order.base)) * 90
				if delta < -nativeAudioRecoveryMaxLagMS || delta > nativeAudioRecoveryMaxLeadMS ||
					stamp <= order.audioPTS || !order.acceptsAudio(stamp, time.Unix(0, frame.received)) {
					continue // Cached or out-of-epoch audio.
				}
				if err := order.add(frame, c.queue.push); err != nil {
					return Stream{}, err
				}
				details := fmt.Sprintf("Audio-only recovery took %s. Audio socket setup took %s. First aligned audio PTS minus video PTS: %d ms.",
					time.Since(started).Round(time.Millisecond), (connected.Sub(attemptStarted) - refreshTime).Round(time.Millisecond), delta)
				if refresh {
					details += fmt.Sprintf(" Native room refresh took %s.", refreshTime.Round(time.Millisecond))
				}
				c.recovery.Log("INFO", "Origin audio resumed",
					details, nil)
				replacement, stopTrack = nil, nil
				return source, nil
			}
			if !frame.video && refresh {
				continue
			}
			if err := order.add(frame, c.queue.push); err != nil {
				return Stream{}, err
			}
		}
	}
}
