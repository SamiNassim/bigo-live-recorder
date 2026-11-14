package bigo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"bigo-live-recorder/internal/fault"
)

const nativeClockRecoveryWindow = 3 * time.Second

// Audio and video use separate sockets. Hold early audio until video reaches
// its timestamp, so an encoding change cannot move that audio into the old
// file. Late audio remains in arrival order and is handled by the file boundary.
type nativeMediaOrder struct {
	audio                *nativeFrameQueue
	head                 *nativeMediaFrame
	count                int
	base, videoPTS       uint32
	started              bool
	videoDTS, audioPTS   int64
	videoTime, audioTime time.Time
	videoJump, audioJump time.Time
	jumpCause            error
	recovery             StreamRecovery
}

func (o *nativeMediaOrder) close() {
	if o.audio != nil {
		o.audio.close()
	}
}

func (o *nativeMediaOrder) add(f nativeMediaFrame, emit func(nativeMediaFrame) error) error {
	if !o.started {
		o.base, o.videoPTS, o.started = f.pts, f.pts, true
	}
	now := time.Now()
	arrival := now
	if f.received != 0 {
		// Use socket arrival, since queued frames can reach this check in a
		// burst after a split or reconnect.
		arrival = time.Unix(0, f.received)
	}
	if o.clockStillDiverged(now) {
		return o.jumpCause
	}
	stamp := int64(int32(f.pts-o.base)) * 90
	if f.video {
		dts := int64(int32(f.dts-o.base)) * 90
		lastPTS := int64(int32(o.videoPTS-o.base)) * 90
		// Early audio may already be queued many frames ahead. Only the
		// video track's own timestamps can invalidate a video frame.
		if nativeClockJump(dts, o.videoDTS, arrival, o.videoTime) ||
			nativeClockJump(stamp, lastPTS, arrival, o.videoTime) || (!o.videoJump.IsZero() && !f.key) {
			o.clockJump("video", &o.videoJump, now, f, stamp, lastPTS)
			return nil
		}
		o.clockResumed("video", &o.videoJump, now)
		o.videoDTS, o.videoTime = dts, arrival
		if int32(f.pts-o.videoPTS) > 0 {
			o.videoPTS = f.pts
		}
		if err := o.flush(func(a nativeMediaFrame) bool { return int32(a.pts-f.pts) < 0 }, emit); err != nil {
			return err
		}
		if err := emit(f); err != nil {
			return err
		}
		return o.flush(func(a nativeMediaFrame) bool { return int32(a.pts-o.videoPTS) <= 0 }, emit)
	}
	if !o.acceptsAudio(stamp, arrival) {
		o.clockJump("audio", &o.audioJump, now, f, stamp, o.audioPTS)
		return nil
	}
	if !o.audioTime.IsZero() && stamp <= o.audioPTS {
		return nil // Duplicates and late audio would regress the muxed clock.
	}
	o.clockResumed("audio", &o.audioJump, now)
	o.audioPTS, o.audioTime = stamp, arrival
	if o.count == 0 && int32(f.pts-o.videoPTS) <= 0 {
		return emit(f)
	}
	if o.audio == nil {
		o.audio = newNativeFrameQueue(64 << 10)
	}
	if err := o.audio.push(f); err != nil {
		return err
	}
	o.count++
	return o.flush(func(a nativeMediaFrame) bool { return int32(a.pts-o.videoPTS) <= 0 }, emit)
}

func (o *nativeMediaOrder) acceptsAudio(stamp int64, arrival time.Time) bool {
	// Audio that runs ahead of a stalled video track can still be valid. Bound
	// the lead over the most recent video by the elapsed time on the socket.
	return !nativeClockJump(stamp, o.audioPTS, arrival, o.audioTime) && !nativeClockJump(stamp, o.videoDTS, arrival, o.videoTime)
}

var errNativeMediaOrderClock = errors.New("BIGO native audio and video clocks diverged; a new media connection is required")

func (o *nativeMediaOrder) clockStillDiverged(now time.Time) bool {
	return (!o.audioJump.IsZero() && now.Sub(o.audioJump) >= nativeClockRecoveryWindow) ||
		(!o.videoJump.IsZero() && now.Sub(o.videoJump) >= nativeClockRecoveryWindow)
}

func (o *nativeMediaOrder) clockJump(track string, since *time.Time, now time.Time, f nativeMediaFrame, stamp, previous int64) {
	if !since.IsZero() {
		return
	}
	details := fmt.Sprintf("Origin %s PTS %d ms, DTS %d ms. Delta from last accepted %s PTS: %d ms. Last accepted video DTS: %d ms.",
		track, f.pts, f.dts, track, (stamp-previous)/90, o.base+uint32(o.videoDTS/90))
	*since = now
	o.jumpCause = fault.WithDetails(errNativeMediaOrderClock, details)
	o.recovery.Log("WARN", "Origin "+track+" timestamp jumped. Checking for recovery.", details, nil)
}

func (o *nativeMediaOrder) clockResumed(track string, since *time.Time, now time.Time) {
	if !since.IsZero() {
		o.recovery.Log("INFO", "Origin "+track+" timestamps recovered", fmt.Sprintf("Recovered in %s on the existing sockets.", now.Sub(*since).Round(time.Millisecond)), nil)
		*since = time.Time{}
		if o.audioJump.IsZero() && o.videoJump.IsZero() {
			o.jumpCause = nil
		}
	}
}

func (o *nativeMediaOrder) flush(ready func(nativeMediaFrame) bool, emit func(nativeMediaFrame) error) error {
	for o.count > 0 {
		if o.head == nil {
			f, err := o.audio.next(context.Background())
			if err != nil {
				return err
			}
			o.head = &f
		}
		if ready != nil && !ready(*o.head) {
			return nil
		}
		if err := emit(*o.head); err != nil {
			return err
		}
		o.head = nil
		o.count--
	}
	return nil
}
