package bigo

import (
	"encoding/binary"
	"errors"
	"io"
)

const maxNativePendingFrames = 32

type nativeVideoAssembly struct {
	parts        map[int][]byte
	data         []byte
	count, bytes int
	key          bool
	codec        byte
}

// Complete frames wait behind earlier fragments so a delayed keyframe reaches
// the decoder before the frames that depend on it. Both completed and partial
// frames count toward the limits; exhausting either limit triggers recovery.
type nativeVideoAssembler struct {
	frames  map[uint32]*nativeVideoAssembly
	bytes   int
	last    uint32
	started bool
}

func (a *nativeVideoAssembler) add(p nativeVideoPart) ([]nativeMediaFrame, error) {
	if p.index < 0 || p.index >= 4096 || p.count < 0 || p.count > 4096 || (!p.split && p.index != 0) {
		return nil, errors.New("invalid BIGO video fragment index")
	}
	if !a.started {
		// A connection can join in the middle of a GOP. Frames preceding its
		// first keyframe cannot be decoded, and must not block that keyframe.
		if !p.key {
			return nil, nil
		}
		a.started, a.last = true, p.frame-1
	}
	distance := int32(p.frame - a.last)
	if distance <= 0 {
		return nil, nil // Ignore retransmissions of frames already emitted.
	}
	if distance > maxNativePendingFrames {
		return nil, errors.New("BIGO video reassembly window exhausted: an earlier frame is missing or incomplete")
	}
	if a.frames == nil {
		a.frames = make(map[uint32]*nativeVideoAssembly)
	}
	f := a.frames[p.frame]
	if f == nil {
		if len(a.frames) >= maxNativePendingFrames {
			return nil, errors.New("BIGO video reassembly stalled: earlier frame is incomplete and the frame buffer is full")
		}
		f = &nativeVideoAssembly{parts: make(map[int][]byte), key: p.key, codec: p.codec}
		a.frames[p.frame] = f
	}
	if f.codec != p.codec || f.key != p.key {
		return nil, errors.New("inconsistent BIGO video fragments")
	}
	if f.data != nil {
		return nil, nil // A complete frame is already waiting for its turn.
	}
	if f.count != 0 && (p.index >= f.count || (p.last && p.index+1 != f.count)) {
		return nil, errors.New("inconsistent BIGO video fragment count")
	}
	if !p.split || p.last {
		f.count = p.index + 1
		for index := range f.parts {
			if index >= f.count {
				return nil, errors.New("BIGO video fragment exceeds the final index")
			}
		}
	}
	if _, exists := f.parts[p.index]; !exists {
		if len(p.data) > maxNativeFrameBytes-a.bytes {
			return nil, errors.New("BIGO video fragment buffer exhausted")
		}
		f.parts[p.index] = p.data
		f.bytes += len(p.data)
		a.bytes += len(p.data)
	}
	if f.count == 0 || len(f.parts) != f.count {
		return nil, nil
	}
	data := make([]byte, 0, f.bytes)
	for i := range f.count {
		part, ok := f.parts[i]
		if !ok {
			return nil, nil
		}
		data = append(data, part...)
	}
	if len(data) < 9 {
		return nil, io.ErrUnexpectedEOF
	}
	f.data, f.parts = data, nil
	return a.drain(), nil
}

func (a *nativeVideoAssembler) drain() []nativeMediaFrame {
	var ready []nativeMediaFrame
	for {
		id := a.last + 1
		f := a.frames[id]
		if f == nil || f.data == nil {
			break
		}
		ready = append(ready, nativeMediaFrame{data: f.data[9:], pts: binary.LittleEndian.Uint32(f.data), dts: binary.LittleEndian.Uint32(f.data[4:]), video: true, key: f.key, codec: f.codec})
		a.last = id
		a.bytes -= f.bytes
		delete(a.frames, id)
	}
	return ready
}
