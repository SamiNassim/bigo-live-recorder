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
	order   []uint32
	bytes   int
	last    uint32
	started bool
}

func (a *nativeVideoAssembler) add(p nativeVideoPart) ([]nativeMediaFrame, error) {
	if p.index < 0 || p.index >= 4096 || p.count < 0 || p.count > 4096 || (!p.split && p.index != 0) {
		return nil, errors.New("invalid BIGO video fragment index")
	}
	if !a.started && len(a.frames) == 0 && !p.key {
		return nil, nil // Wait for a keyframe before buffering its dependents.
	}
	if a.started && int32(p.frame-a.last) <= 0 {
		return nil, nil // Ignore retransmissions of frames already emitted.
	}
	if a.frames == nil {
		a.frames = make(map[uint32]*nativeVideoAssembly)
	}
	f := a.frames[p.frame]
	if f == nil {
		if len(a.frames) >= maxNativePendingFrames {
			if a.started {
				return nil, errors.New("BIGO video reassembly stalled: earlier frame is incomplete and the frame buffer is full")
			}
			// Joining midway through a keyframe is normal. No video has
			// started yet, so discard this undecodable startup GOP and wait
			// for a complete keyframe within the existing startup timeout.
			a.resetStartup()
			if !p.key {
				return nil, nil
			}
		}
		f = &nativeVideoAssembly{parts: make(map[int][]byte), key: p.key, codec: p.codec}
		a.frames[p.frame] = f
		i := len(a.order)
		a.order = append(a.order, p.frame)
		for i > 0 && int32(p.frame-a.order[i-1]) < 0 {
			a.order[i] = a.order[i-1]
			i--
		}
		a.order[i] = p.frame
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
			if a.started || len(p.data) > maxNativeFrameBytes {
				return nil, errors.New("BIGO video fragment buffer exhausted")
			}
			a.resetStartup()
			return a.add(p)
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
	if !a.started {
		first := -1
		for i, id := range a.order {
			if f := a.frames[id]; f.key && f.data != nil {
				first = i
				break
			}
		}
		if first < 0 {
			return nil
		}
		// A partial keyframe from before the connection must not block the
		// first usable keyframe. Once started, incomplete frames are retained.
		for _, id := range a.order[:first] {
			a.bytes -= a.frames[id].bytes
			delete(a.frames, id)
		}
		a.trimOrder(first)
		a.started = true
	}
	var ready []nativeMediaFrame
	// Provider frame numbers can skip. Only frames whose fragments have
	// actually arrived may hold up later frames; unseen IDs are not holes.
	for _, id := range a.order {
		f := a.frames[id]
		if f.data == nil {
			break
		}
		ready = append(ready, nativeMediaFrame{data: f.data[9:], pts: binary.LittleEndian.Uint32(f.data), dts: binary.LittleEndian.Uint32(f.data[4:]), video: true, key: f.key, codec: f.codec})
		a.last = id
		a.bytes -= f.bytes
		delete(a.frames, id)
	}
	if n := len(ready); n > 0 {
		a.trimOrder(n)
	}
	return ready
}

func (a *nativeVideoAssembler) trimOrder(n int) {
	copy(a.order, a.order[n:])
	clear(a.order[len(a.order)-n:])
	a.order = a.order[:len(a.order)-n]
}

func (a *nativeVideoAssembler) resetStartup() {
	clear(a.frames)
	clear(a.order)
	a.order = a.order[:0]
	a.bytes = 0
}
