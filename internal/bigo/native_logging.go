package bigo

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"bigo-live-recorder/internal/fault"
)

type nativeKeepaliveFailure struct{ err error }

type nativeTrackFailure struct {
	error
	at   time.Time
	conn *nativeMediaConn
}

func (e *nativeTrackFailure) Unwrap() error { return e.error }
func (e *nativeTrackFailure) DiagnosticDetails() string {
	return fault.Details(e.error)
}

func (c *nativeMediaConn) track() string {
	if c.video {
		return "video"
	}
	return "audio"
}

func (c *nativeMediaConn) playableAge(now time.Time) string {
	if elapsed := c.lastPlayable.Load(); elapsed > 0 {
		return max(0, now.Sub(c.setupStarted.Add(time.Duration(elapsed-1)))).Round(time.Millisecond).String() + " ago"
	}
	return "none received"
}

func (c *nativeMediaConn) trackFailure(peer *nativeMediaConn, err error) error {
	now := time.Now()
	details := fmt.Sprintf("Origin %s receiver stopped. Last playable media at detection: %s %s, %s %s.",
		c.track(), c.track(), c.playableAge(now), peer.track(), peer.playableAge(now))
	if write := c.keepaliveFailure.Load(); write != nil {
		details += "\nOrigin " + c.track() + " keepalive write failed: " + fault.Details(write.err)
	}
	if errors.Is(err, io.EOF) {
		// A transport EOF must trigger recovery, while keeping the original
		// receiver context available to the diagnostic log.
		details += "\n" + fault.Details(err)
		err = io.ErrUnexpectedEOF
	}
	return &nativeTrackFailure{error: fault.WithDetails(err, details), at: now, conn: c}
}

type nativeSetupLog struct {
	started       time.Time
	steps         []string
	video         *nativeMediaConn
	videoCodec    byte
	keyframe      time.Time
	audio         time.Time
	audioOffsetMS int32
}

func newNativeSetupLog() nativeSetupLog {
	return nativeSetupLog{started: time.Now()}
}

func (l *nativeSetupLog) mark(step string) {
	l.steps = append(l.steps, fmt.Sprintf("%s at %s", step, time.Since(l.started).Round(time.Millisecond)))
}

func (l *nativeSetupLog) details() string {
	steps := l.steps
	if l.video != nil {
		if confirmed := l.video.confirmedAfter.Load(); confirmed > 0 {
			steps = append(steps, fmt.Sprintf("video subscription confirmed at %s", time.Duration(confirmed-1).Round(time.Millisecond)))
		}
	}
	if !l.keyframe.IsZero() {
		codec := fmt.Sprintf("native codec ID %d", l.videoCodec)
		if streamType, _ := nativeVideoType(l.videoCodec); streamType == 0x1b {
			codec = "H.264"
		} else if streamType == 0x24 {
			codec = "HEVC"
		}
		steps = append(steps, fmt.Sprintf("first video keyframe at %s (%s)", l.keyframe.Sub(l.started).Round(time.Millisecond), codec))
	}
	if !l.audio.IsZero() {
		steps = append(steps, fmt.Sprintf("first aligned audio at %s (audio PTS minus keyframe PTS: %d ms)", l.audio.Sub(l.started).Round(time.Millisecond), l.audioOffsetMS))
	}
	return fmt.Sprintf("Origin media setup took %s. Milestones from setup start: %s.",
		time.Since(l.started).Round(time.Millisecond), strings.Join(steps, ", "))
}

type nativeRecoveryLog struct {
	started, attemptStarted time.Time
	attempt                 int
	refresh                 time.Duration
}

func newNativeRecoveryLog(err error) nativeRecoveryLog {
	var stopped *nativeTrackFailure
	if errors.As(err, &stopped) {
		return nativeRecoveryLog{started: stopped.at}
	}
	return nativeRecoveryLog{started: time.Now()}
}

func (l *nativeRecoveryLog) beginAttempt(attempt int) {
	l.attempt = attempt
	l.attemptStarted = time.Now()
}

func (l nativeRecoveryLog) details(outcome string) string {
	return fmt.Sprintf("Origin recovery %s after %s from interruption detection. Attempt %d of %d took %s, including source refresh in %s.",
		outcome, time.Since(l.started).Round(time.Millisecond), l.attempt, nativeRecoveryAttempts,
		time.Since(l.attemptStarted).Round(time.Millisecond), l.refresh.Round(time.Millisecond))
}

func (l nativeRecoveryLog) failure(stage string, err error) error {
	return fault.WithDetails(err, l.details("failed during "+stage))
}
