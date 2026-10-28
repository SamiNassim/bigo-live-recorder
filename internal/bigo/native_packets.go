package bigo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maxNativeFrameBytes = 8 << 20

type nativePacket struct {
	uri    uint32
	status uint16
	data   []byte
}

// Small media packets pack their length and message type into the first word.
// The upper bytes remain part of the media header, unlike control packets.
func readNativePacket(r io.Reader) (nativePacket, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nativePacket{}, err
	}
	word := binary.LittleEndian.Uint32(head[:])
	size, uri, status := word, uint32(0), uint16(200)
	packed := word&0x80000000 != 0
	if packed {
		size, uri = word>>4&0xfff, word&15
	}
	if size < 4 || size > maxNativeFrameBytes || (!packed && size < 10) {
		return nativePacket{}, errors.New("invalid BIGO media packet length")
	}
	data := make([]byte, int(size))
	copy(data, head[:])
	if _, err := io.ReadFull(r, data[4:]); err != nil {
		return nativePacket{}, err
	}
	if !packed {
		uri, status = binary.LittleEndian.Uint32(data[4:]), binary.LittleEndian.Uint16(data[8:])
		data = data[10:]
	}
	return nativePacket{uri: uri, status: status, data: data}, nil
}

func (r *nativeReader) u8() byte {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}
func (r *nativeReader) u64() uint64 {
	if b := r.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

type nativeMediaFrame struct {
	data       []byte
	pts, dts   uint32
	video, key bool
	codec      byte
}

type nativeVideoPart struct {
	frame, owner     uint32
	index, count     int
	key, split, last bool
	codec            byte
	data             []byte
}

func decodeNativeVideo(data []byte) (nativeVideoPart, error) {
	r := nativeReader{data: data}
	r.u16()
	r.u8()
	kind := r.u8()
	r.u16()
	p := nativeVideoPart{frame: r.u32(), key: kind&7 == 1}
	f, g := r.u8(), r.u8()
	p.codec, p.split, p.last = f&7, g&16 != 0, g&32 != 0
	if g&1 != 0 {
		p.owner = r.u32()
	}
	if g&4 != 0 {
		r.u32()
	}
	if g&8 != 0 {
		r.u8()
	}
	if p.split {
		p.index = int(r.u8())
	}
	if g&128 != 0 {
		r.u32()
	}
	if f&64 != 0 {
		r.take(5)
	}
	if f&16 != 0 {
		r.u16()
	}
	skip := 0
	if f&32 != 0 {
		e := r.u8()
		if e&2 != 0 {
			r.u8()
		}
		if e&4 != 0 {
			skip = int(r.u8())
		}
		if e&8 != 0 {
			r.u32()
		}
		if e&16 != 0 {
			p.count = int(r.u8() & 127)
		}
		if e&64 != 0 {
			p.index |= int(r.u8()) << 8
		}
		if e&128 != 0 {
			e2 := r.u8()
			if e2&8 != 0 {
				r.u8()
			}
			if e2&16 != 0 {
				r.take(33)
			}
			if e2&64 != 0 {
				r.u16()
			}
			if e2&128 != 0 {
				e3 := r.u8()
				if e3&1 != 0 {
					p.owner = uint32(r.u64())
					r.take(16)
				}
				if e3&2 != 0 {
					r.u8()
				}
				if e3&4 != 0 {
					r.u16()
				}
				if e3&32 != 0 {
					r.u32()
				}
				if e3&64 != 0 {
					r.u16()
				}
				if e3&128 != 0 {
					e4 := r.u8()
					if e4&1 != 0 {
						r.u8()
					}
					if e4&4 != 0 {
						r.u8()
					}
					if e4&8 != 0 {
						r.take(4)
					}
					if e4&16 != 0 {
						r.u16()
					}
					if e4&64 != 0 {
						skip = int(r.u16())
					}
					if e4&128 != 0 && r.u8()&1 != 0 {
						return p, errors.New("BIGO combined video sources are unsupported")
					}
				}
			}
		}
	}
	// Extension lengths include the media header except its initial two bytes.
	if skip > r.off-2 {
		r.take(skip - r.off + 2)
	}
	p.data = r.take(r.remaining())
	return p, r.err
}

type nativeVideoAssembly struct {
	parts        map[int][]byte
	count, bytes int
	key          bool
	codec        byte
}

type nativeVideoAssembler struct {
	frames map[uint32]*nativeVideoAssembly
	bytes  int
}

func (a *nativeVideoAssembler) add(p nativeVideoPart) (*nativeMediaFrame, error) {
	if p.index >= 4096 || p.count > 4096 {
		return nil, errors.New("invalid BIGO video fragment index")
	}
	if a.frames == nil {
		a.frames = make(map[uint32]*nativeVideoAssembly)
	}
	f := a.frames[p.frame]
	if f == nil {
		if len(a.frames) >= 32 {
			return nil, errors.New("BIGO video fragment window exhausted")
		}
		f = &nativeVideoAssembly{parts: make(map[int][]byte), key: p.key, codec: p.codec}
		a.frames[p.frame] = f
	}
	if f.codec != p.codec || f.key != p.key {
		return nil, errors.New("inconsistent BIGO video fragments")
	}
	if _, exists := f.parts[p.index]; !exists {
		f.parts[p.index] = p.data
		f.bytes += len(p.data)
		a.bytes += len(p.data)
	}
	if a.bytes > maxNativeFrameBytes {
		return nil, errors.New("BIGO video fragment buffer exhausted")
	}
	if !p.split {
		f.count = 1
	} else if p.last {
		f.count = p.index + 1
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
	delete(a.frames, p.frame)
	a.bytes -= f.bytes
	// Joining mid-frame is normal. Retire preceding incomplete frames once a
	// newer complete frame arrives, keeping the cache bounded during startup.
	for id, old := range a.frames {
		if int32(p.frame-id) > 0 {
			delete(a.frames, id)
			a.bytes -= old.bytes
		}
	}
	if len(data) < 9 {
		return nil, io.ErrUnexpectedEOF
	}
	return &nativeMediaFrame{data: data[9:], pts: binary.LittleEndian.Uint32(data), dts: binary.LittleEndian.Uint32(data[4:]), video: true, key: f.key, codec: f.codec}, nil
}

func decodeNativeAudio(data []byte) ([]nativeMediaFrame, error) {
	r := nativeReader{data: data}
	r.u16()
	f, g := r.u8(), r.u8()
	if g&128 == 0 {
		return nil, errors.New("unsupported BIGO legacy audio header")
	}
	r.u32() // packet sequence
	if g&2 != 0 {
		r.u32()
	}
	if g&64 != 0 {
		r.u32()
	}
	pts := uint32(0)
	if g&32 != 0 {
		pts = r.u32()
	}
	if g&16 != 0 {
		r.u32()
	}
	if g&8 != 0 {
		r.u32()
	}
	r.u8()
	codec := r.u8()
	r.u32()
	if codec != 6 && codec != 7 {
		return nil, fmt.Errorf("unsupported BIGO native audio codec %d", codec)
	}
	if f&1 != 0 {
		ext := r.u8()
		if ext&1 != 0 {
			r.u32()
		}
		if ext&2 != 0 {
			r.u8()
		}
	}
	if g&1 != 0 {
		e := r.u8()
		if e&2 != 0 {
			r.u8()
		}
		if e&4 != 0 {
			r.u32()
		}
		skip := 0
		if e&16 != 0 {
			skip = int(r.u8())
		}
		if e&32 != 0 {
			r.u32()
		}
		if e&64 != 0 {
			r.take(24)
		}
		if e&128 != 0 {
			e2 := r.u8()
			if e2&4 != 0 {
				r.take(int(r.u16()))
			}
			if e2&8 != 0 {
				r.u32()
			}
			if e2&16 != 0 {
				r.u32()
			}
			if e2&32 != 0 {
				r.take(int(r.u8()) * 8)
			}
			if e2&128 != 0 {
				e3 := r.u8()
				if e3&2 != 0 {
					n := int(r.u8())
					r.take(n * 13)
				}
				if e3&4 != 0 {
					r.take(int(r.u8()) * 8)
				}
			}
		}
		if skip > r.off {
			r.take(skip - r.off)
		}
	}
	var frames []nativeMediaFrame
	var samples uint64
	for range int(f >> 5) {
		r.u8() // per-frame flags
		length := r.u16()
		if length&0xfc00 != 0xfc00 {
			return nil, errors.New("invalid BIGO AAC frame length")
		}
		frame := r.take(int(length & 1023))
		if r.err != nil {
			return nil, r.err
		}
		rate, err := nativeAACRate(frame)
		if err != nil {
			return nil, err
		}
		timestamp := pts + uint32(samples*1000/uint64(rate))
		frames = append(frames, nativeMediaFrame{data: frame, pts: timestamp, dts: timestamp})
		samples += 1024
	}
	return frames, r.err
}

func nativeAACRate(data []byte) (int, error) {
	if len(data) < 7 || data[0] != 255 || data[1]&0xf6 != 0xf0 {
		return 0, errors.New("BIGO native audio is not AAC ADTS")
	}
	rates := [13]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	index := data[2] >> 2 & 15
	length := int(data[3]&3)<<11 | int(data[4])<<3 | int(data[5])>>5
	if int(index) >= len(rates) || length != len(data) {
		return 0, errors.New("invalid BIGO AAC ADTS header")
	}
	return rates[index], nil
}
