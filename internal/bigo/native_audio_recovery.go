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
		return false // A replacement dying immediately needs fresh room details.
	}
	elapsed := s.video.lastPlayable.Load()
	return elapsed > 0 && failed.at.Sub(s.video.setupStarted.Add(time.Duration(elapsed-1))) <= nativeAudioRecoveryVideoAge
}

// Try the existing room's audio route once. The caller continues consuming
// video and falls back to full, source-refreshing recovery if this fails.
func (c *nativeCapture) recoverAudio(source Stream, session *nativeMediaSession, order *nativeMediaOrder, cause error) error {
	session.audioRetried = true
	started := time.Now()
	c.recovery.Log("WARN", "Origin audio interrupted. Keeping video open.", "Trying the current room's audio route while continuing to record video.", cause)
	ctx, cancel := context.WithTimeout(session.ctx, nativeAudioRecoveryTimeout)
	defer cancel()
	type result struct {
		conn *nativeMediaConn
		err  error
	}
	ready := make(chan result)
	channel := source.native
	go func() {
		conn, err := dialNativeMedia(ctx, channel.media, channel.source, channel.sid, false)
		if err == nil {
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			err = conn.login(channel, source.UID)
			stop()
		}
		select {
		case ready <- result{conn, err}:
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
		}
	}()
	var connected time.Time
	for {
		select {
		case <-ctx.Done():
			return fault.Wrap("audio-only recovery did not restore synchronized audio", ctx.Err())
		case err := <-session.failed:
			return err
		case opened := <-ready:
			if opened.err != nil {
				if opened.conn != nil {
					opened.conn.Close()
				}
				return fault.Wrap("could not reopen the Origin audio socket", opened.err)
			}
			// The old receiver finished sending before it reported failure.
			// Drain its queued audio before accepting samples from its replacement.
			if err := c.drainNativeMedia(session, order); err != nil {
				opened.conn.Close()
				return err
			}
			connected = time.Now()
			opened.conn.setupStarted = connected
			if !session.startTrack(opened.conn, channel) {
				return context.Canceled
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
					return err
				}
				c.recovery.Log("INFO", "Origin audio resumed",
					fmt.Sprintf("Audio-only recovery took %s. Audio socket setup took %s. First aligned audio PTS minus video PTS: %d ms.",
						time.Since(started).Round(time.Millisecond), connected.Sub(started).Round(time.Millisecond), delta), nil)
				return nil
			}
			if err := order.add(frame, c.queue.push); err != nil {
				return err
			}
		}
	}
}
