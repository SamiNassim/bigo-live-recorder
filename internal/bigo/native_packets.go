package bigo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const maxNativeFrameBytes = 8 << 20

var errNativeAACFrame = errors.New("invalid BIGO native AAC frame")

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
	reset      bool
	received   int64
}

type nativeVideoPart struct {
	frame, owner     uint32
	index, count     int
	key, split, last bool
	codec            byte
	mode             byte
	data             []byte
	received         int64
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
			p.mode = r.u8()
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

type nativeAudioSample struct {
	data  []byte
	flags byte
}
type nativeAudioPacket struct {
	owner, sequence, pts uint32
	codec                byte
	count                int
	samples              [7]nativeAudioSample
}

func decodeNativeAudio(data []byte) ([]nativeMediaFrame, error) {
	packet, err := parseNativeAudio(data)
	if err != nil {
		return nil, err
	}
	return decodeNativeAAC(packet)
}

func parseNativeAudio(data []byte) (nativeAudioPacket, error) {
	var packet nativeAudioPacket
	r := nativeReader{data: data}
	r.u16()
	f, g := r.u8(), r.u8()
	if g&128 == 0 {
		return nativeAudioPacket{}, errors.New("unsupported BIGO legacy audio header")
	}
	packet.sequence = r.u32()
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
		packet.owner = r.u32()
	}
	if g&8 != 0 {
		r.u32()
	}
	r.u8()
	codec := r.u8()
	r.u32()
	if codec != 6 && codec != 7 {
		return nativeAudioPacket{}, fmt.Errorf("unsupported BIGO native audio codec %d", codec)
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
			owner := r.u64()
			r.take(16)
			if owner <= uint64(^uint32(0)) {
				packet.owner = uint32(owner)
			} else {
				packet.owner = 0
			}
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
	packet.pts, packet.codec = pts, codec
	packet.count = int(f >> 5)
	for i := range packet.count {
		flags := r.u8()
		length := r.u16()
		if length&0xfc00 != 0xfc00 {
			return nativeAudioPacket{}, errors.New("invalid BIGO audio frame length")
		}
		packet.samples[i] = nativeAudioSample{flags: flags, data: r.take(int(length & 1023))}
	}
	return packet, r.err
}

func decodeNativeAAC(packet nativeAudioPacket) ([]nativeMediaFrame, error) {
	var rates [7]int
	rate := 0
	var invalid error
	for i, sample := range packet.samples[:packet.count] {
		sampleRate, err := nativeAACRate(sample.data)
		if err != nil {
			invalid = errors.Join(invalid, fmt.Errorf("%w: codec=%d flags=%#x bytes=%d: %v", errNativeAACFrame, packet.codec, sample.flags, len(sample.data), err))
		} else {
			rate = sampleRate
		}
		rates[i] = sampleRate
	}
	var frames []nativeMediaFrame
	var elapsed uint64
	for i, sample := range packet.samples[:packet.count] {
		if rates[i] != 0 {
			rate = rates[i]
			timestamp := packet.pts + uint32(elapsed/uint64(time.Millisecond))
			frames = append(frames, nativeMediaFrame{data: sample.data, pts: timestamp, dts: timestamp, codec: 6})
		}
		if rate != 0 {
			blocks := uint64(1)
			if rates[i] != 0 {
				blocks += uint64(sample.data[6] & 3)
			}
			elapsed += blocks * 1024 * uint64(time.Second) / uint64(rate)
		}
	}
	return frames, invalid
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
