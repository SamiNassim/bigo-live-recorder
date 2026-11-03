package bigo

import (
	"hash/adler32"
	"sync"
	"time"
)

// MediaTime relates an original encoded frame to its received and muxed clocks.
// Received is measured against Epoch with a monotonic clock, before buffering.
// PTS and DTS use the MPEG-TS 90 kHz time base; source timestamps use milliseconds.
type MediaTime struct {
	Received   time.Duration
	SourcePTS  uint32
	SourceDTS  uint32
	PTS, DTS   int64
	Duration   time.Duration
	Checksum   uint32
	Video, Key bool
	Codec      byte
}

// CaptureTiming is deliberately independent of disk storage. Losing a timing
// sidecar must never stop reception or change the original media bytes.
type CaptureTiming struct {
	Epoch time.Time
	mu    sync.Mutex
	sink  func(MediaTime)
}

// Observe must be called before reading a CapturePart. Samples arrive in mux
// order, including retained pre-roll, with their original receipt times intact.
func (t *CaptureTiming) Observe(sink func(MediaTime)) {
	if t != nil {
		t.mu.Lock()
		t.sink = sink
		t.mu.Unlock()
	}
}

func (t *CaptureTiming) record(f nativeMediaFrame, pts, dts int64) {
	if t == nil || f.received <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sink == nil {
		return
	}
	var duration time.Duration
	if !f.video {
		if f.codec == 7 {
			if samples, err := opusSamples(f.data); err == nil {
				duration = time.Duration(samples) * time.Second / 48000
			}
		} else if rate, err := nativeAACRate(f.data); err == nil {
			duration = time.Duration(1024*(1+int(f.data[6]&3))) * time.Second / time.Duration(rate)
		}
	}
	var checksum uint32
	// Tiny silence/DTX payloads repeat by design. They cannot prove that a
	// frame after a clock reset is a retransmission of an older recording.
	if f.video || len(f.data) >= 32 {
		checksum = adler32.Checksum(f.data)
	}
	t.sink(MediaTime{Received: time.Duration(f.received), SourcePTS: f.pts, SourceDTS: f.dts, PTS: pts, DTS: dts,
		Duration: duration, Checksum: checksum, Video: f.video, Key: f.key, Codec: f.codec})
}
