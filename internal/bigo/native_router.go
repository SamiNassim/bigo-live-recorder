package bigo

import (
	"errors"
)

// Packets for another room participant or substream must never share fragment
// state. Substream tags are opaque identities, not resolution ranks. Change the
// active substream only at a newer usable keyframe from the requested owner.
type nativeVideoRouter struct {
	owner    uint32
	sources  map[byte]*nativeVideoAssembler
	mode     byte
	selected bool
	lastDTS  uint32
}

func (r *nativeVideoRouter) add(p nativeVideoPart) ([]nativeMediaFrame, error) {
	if r.owner != 0 && p.owner != 0 && p.owner != r.owner {
		return nil, nil
	}
	if r.sources == nil {
		r.sources = make(map[byte]*nativeVideoAssembler)
	}
	a := r.sources[p.mode]
	if a == nil {
		if len(r.sources) >= 8 {
			return nil, errors.New("too many BIGO native video sources")
		}
		a = &nativeVideoAssembler{}
		r.sources[p.mode] = a
	}
	frames, err := a.add(p)
	if err != nil {
		return nil, err
	}
	bytes := 0
	for _, source := range r.sources {
		bytes += source.bytes
	}
	if bytes > maxNativeFrameBytes {
		return nil, errors.New("BIGO native video source buffers are full")
	}
	var ready []nativeMediaFrame
	for _, f := range frames {
		if r.selected && int32(f.dts-r.lastDTS) < 0 {
			continue
		}
		if !r.selected || p.mode != r.mode {
			if !f.key || (r.selected && int32(f.dts-r.lastDTS) <= 0) {
				continue
			}
			r.mode, r.selected = p.mode, true
		}
		r.lastDTS = f.dts
		ready = append(ready, f)
	}
	return ready, nil
}
