package bigo

import (
	"bytes"
	"iter"
)

// A new guest file needs its own codec headers even when the encoder does not
// repeat them at every keyframe. Keep this cache on the participant capture,
// not on a file or a shared room transport.
type nativeVideoHeaders struct {
	codec byte
	sets  [3][]byte // H.264 SPS/PPS, or HEVC VPS/SPS/PPS.
}

func (h *nativeVideoHeaders) prepare(f nativeMediaFrame) (nativeMediaFrame, bool) {
	codec, err := nativeVideoType(f.codec)
	if err != nil {
		return f, false
	}
	if codec != h.codec {
		*h = nativeVideoHeaders{codec: codec}
	}
	var present [3][]byte
	picture := false
	for unit := range annexBUnits(f.data) {
		kind, index := unit[0]&31, -1
		if codec == 0x1b {
			picture = picture || kind >= 1 && kind <= 5
			if kind == 7 || kind == 8 {
				index = int(kind - 7)
			}
		} else if len(unit) >= 2 {
			kind = unit[0] >> 1 & 63
			picture = picture || kind <= 31
			if kind >= 32 && kind <= 34 {
				index = int(kind - 32)
			}
		}
		if index >= 0 && len(unit) <= 64<<10 && len(present[index])+len(unit)+4 <= 64<<10 {
			present[index] = append(present[index], 0, 0, 0, 1)
			present[index] = append(present[index], unit...)
		}
	}
	for i, set := range present {
		if len(set) > 0 {
			h.sets[i] = set
		}
	}
	ready := len(h.sets[0]) > 0 && len(h.sets[1]) > 0 && (codec == 0x1b || len(h.sets[2]) > 0)
	if !f.key || !picture || !ready {
		return f, false
	}
	missing := false
	for i, set := range h.sets {
		missing = missing || len(set) > 0 && len(present[i]) == 0
	}
	if missing {
		var prefix []byte
		for _, set := range h.sets {
			prefix = append(prefix, set...)
		}
		f.data = append(prefix, f.data...)
	}
	return f, true
}

// Annex B start codes can have three or four bytes. Parameter sets cannot
// contain an unescaped start code, so no bitstream decoding is needed here.
func annexBUnits(data []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for {
			start := bytes.Index(data, []byte{0, 0, 1})
			if start < 0 {
				return
			}
			data = data[start+3:]
			end := bytes.Index(data, []byte{0, 0, 1})
			if end < 0 {
				end = len(data)
			}
			unit := bytes.TrimRight(data[:end], "\x00")
			if len(unit) > 0 && !yield(unit) {
				return
			}
			data = data[end:]
		}
	}
}
