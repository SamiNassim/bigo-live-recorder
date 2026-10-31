package bigo

import (
	"context"
	"errors"
	"time"
)

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
	stamp := int64(int32(f.pts-o.base)) * 90
	if f.video {
		stamp = int64(int32(f.dts-o.base)) * 90
	}
	if nativeClockJump(stamp, o.videoDTS, now, o.videoTime) || nativeClockJump(stamp, o.audioPTS, now, o.audioTime) {
		return errNativeMediaOrderClock
	}
	if f.video {
		o.videoDTS, o.videoTime = stamp, now
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
	o.audioPTS, o.audioTime = stamp, now
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

var errNativeMediaOrderClock = errors.New("BIGO native audio and video clocks diverged; a new media connection is required")

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
