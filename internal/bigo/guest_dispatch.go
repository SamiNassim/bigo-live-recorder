package bigo

import (
	"errors"
	"hash/crc32"
	"io"
	"slices"
	"time"
)

type guestMember struct {
	capture  *nativeCapture
	pending  []nativeMediaFrame
	bytes    int
	latest   uint32
	received time.Time
	replays  guestFrameWindow
}

type guestFrameKey struct {
	pts, dts, checksum uint32
	codec              byte
	video              bool
}

type guestFrameWindow struct {
	keys [512]guestFrameKey
	seen map[guestFrameKey]bool
	next int
}

func (w *guestFrameWindow) accept(frame nativeMediaFrame) bool {
	key := guestFrameKey{pts: frame.pts, dts: frame.dts, checksum: crc32.ChecksumIEEE(frame.data), codec: frame.codec, video: frame.video}
	if w.seen == nil {
		w.seen = make(map[guestFrameKey]bool)
	}
	if w.seen[key] {
		return false
	}
	delete(w.seen, w.keys[w.next])
	w.keys[w.next], w.seen[key] = key, true
	w.next = (w.next + 1) % len(w.keys)
	return true
}

func guestTimestamp(f nativeMediaFrame) uint32 {
	if f.video {
		return f.dts
	}
	return f.pts
}

// Each source has an independent, bounded jitter buffer. Audio keeps flowing
// when a camera is off; no video watermark can hold it indefinitely.
func (m *guestMember) flush(all bool) error {
	count := 0
	for count < len(m.pending) {
		f := m.pending[count]
		if !all && int32(m.latest-guestTimestamp(f)) < 150 && len(m.pending)-count < 256 && m.bytes < 2<<20 {
			break
		}
		if err := m.capture.queue.push(f); err != nil {
			return err
		}
		m.bytes -= len(f.data)
		count++
	}
	copy(m.pending, m.pending[count:])
	clear(m.pending[len(m.pending)-count:])
	m.pending = m.pending[:len(m.pending)-count]
	return nil
}

func (r *guestRoom) dispatch() {
	defer close(r.done)
	defer close(r.guests)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	defer func() {
		// The room's Next reports a transport failure once. Received source
		// packets can still finalize normally; do not repeat that same failure
		// as a corrupt-file warning for every participant.
		for _, member := range r.members {
			if flushErr := member.flush(true); flushErr != nil {
				member.capture.queue.finish(flushErr)
			}
			member.capture.queue.finish(nil)
		}
	}()
	for {
		select {
		case incoming, ok := <-r.frames:
			if !ok {
				return
			}
			member := r.members[incoming.uid]
			if member == nil {
				if len(r.members) >= 64 {
					r.finish(errors.New("BIGO advertised too many simultaneous room participants"))
					continue
				}
				member = &guestMember{capture: newGuestCapture(r.parent, r.recovery)}
				member.capture.epoch = r.epoch
				member.capture.splitInterval = r.splitInterval
				r.members[incoming.uid] = member
				select {
				case r.guests <- Participant{UID: incoming.uid, Capture: member.capture}:
				case <-r.ctx.Done():
					member.capture.queue.finish(nil)
					continue
				}
			}
			if member.capture.ctx.Err() != nil {
				continue
			}
			member.received = time.Now()
			f := incoming.frame
			if !member.replays.accept(f) {
				continue
			}
			ts := guestTimestamp(f)
			if len(member.pending) == 0 || int32(ts-member.latest) > 0 {
				member.latest = ts
			}
			index, _ := slices.BinarySearchFunc(member.pending, ts, func(frame nativeMediaFrame, timestamp uint32) int {
				return int(int32(guestTimestamp(frame) - timestamp))
			})
			member.pending = slices.Insert(member.pending, index, f)
			member.bytes += len(f.data)
			if err := member.flush(false); err != nil {
				r.finish(err)
			}
		case now := <-ticker.C:
			for uid, member := range r.members {
				if now.Sub(member.received) >= 150*time.Millisecond {
					if err := member.flush(true); err != nil && !errors.Is(err, io.ErrClosedPipe) {
						r.finish(err)
					}
				}
				if now.Sub(member.received) >= 30*time.Second || member.capture.ctx.Err() != nil {
					member.capture.queue.finish(nil)
					delete(r.members, uid)
				}
			}
		}
	}
}
