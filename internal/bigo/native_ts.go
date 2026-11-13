package bigo

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"
)

type nativeTSClock struct {
	mu                       sync.Mutex
	base                     uint32
	offset, lastPTS, lastDTS int64
	codec                    byte
	started                  bool
	continuity               map[uint16]byte
}

type nativeClockError string

func (err nativeClockError) Error() string { return string(err) }

func nativeVideoType(codec byte) (byte, error) {
	switch codec {
	case 0, 2, 3:
		return 0x1b, nil
	case 4, 5:
		return 0x24, nil
	default:
		return 0, errors.New("unsupported BIGO native video codec")
	}
}

// nativeTSMux packages Annex B video and AAC or Opus audio without changing either
// elementary stream. This gives FFmpeg one timestamped input on every OS.
type nativeTSMux struct {
	out                          io.Writer
	base                         uint32
	codec                        byte
	continuity                   map[uint16]byte
	started                      bool
	lastTables, lastVideoDTS     int64
	startPTS                     int64
	offset                       int64
	clock                        *nativeTSClock
	lastAudioPTS                 int64
	lastVideoTime, lastAudioTime time.Time
	noAudio                      bool
	audioOnly, opus              bool
	startWithAudio               bool
	videoCodec                   byte
	timing                       *CaptureTiming
}

func (m *nativeTSMux) splitDue(f nativeMediaFrame, interval time.Duration) bool {
	if !m.started || interval <= 0 {
		return false
	}
	stamp := int64(int32(f.pts-m.base))*90 + m.offset
	return stamp >= interval.Milliseconds()*90
}

func (m *nativeTSMux) writeFrame(f nativeMediaFrame) error {
	codec := byte(0)
	if f.video {
		var err error
		codec, err = nativeVideoType(f.codec)
		if err != nil {
			return err
		}
	}
	if !m.started {
		if !m.audioOnly && !m.startWithAudio && (!f.video || !f.key) {
			return nil
		}
		if !f.video && !m.audioOnly {
			codec = m.videoCodec
		}
		m.started = true
		m.base = f.pts
		m.codec = codec
		m.continuity = make(map[uint16]byte)
		m.lastTables = -90000
		if m.clock != nil {
			m.clock.mu.Lock()
			if m.clock.started && m.clock.codec != codec {
				m.clock.mu.Unlock()
				return errors.New("BIGO native video codec changed")
			}
			if !m.clock.started {
				m.clock.started = true
				m.clock.base = f.pts
				m.clock.codec = codec
			} else {
				next := int64(int32(f.pts-m.clock.base))*90 + m.clock.offset
				if next <= m.clock.lastPTS {
					m.clock.offset += m.clock.lastPTS + 90 - next
				}
			}
			m.base, m.offset, m.lastVideoDTS = m.clock.base, m.clock.offset, m.clock.lastDTS
			m.clock.mu.Unlock()
		}
		m.startPTS = int64(int32(f.pts-m.base))*90 + m.offset
		if m.startWithAudio {
			m.startPTS = 0
		}
	}
	pts := int64(int32(f.pts-m.base))*90 + m.offset
	// Audio servers can replay cached samples when a media socket reconnects.
	// Start at this connection's first video keyframe, even when the shared
	// recording clock began on an earlier socket.
	if pts < 0 || (!f.video && pts < m.startPTS) {
		if (f.video && pts < m.startPTS-10*90000) || (!f.video && !m.lastAudioTime.IsZero() && pts < m.lastAudioPTS-10*90000) {
			return nativeClockError("BIGO native media clock moved backward; a new media connection is required")
		}
		return nil
	}
	dts := pts
	now := time.Now()
	if f.video {
		if codec != m.codec {
			return errors.New("BIGO native video codec changed")
		}
		dts = int64(int32(f.dts-m.base))*90 + m.offset
		if nativeClockJump(dts, m.lastVideoDTS, now, m.lastVideoTime) {
			return nativeClockError("BIGO native video timestamp jumped; a new media connection is required")
		}
		if nativeClockJump(dts, m.lastAudioPTS, now, m.lastAudioTime) {
			return nativeClockError("BIGO native video and audio clocks diverged; a new media connection is required")
		}
		if dts > pts {
			dts = pts
		}
		if dts < 0 {
			dts = 0
		}
		if dts < m.lastVideoDTS {
			dts = m.lastVideoDTS
		}
		if pts < dts {
			pts = dts
		}
	} else {
		if nativeClockJump(pts, m.lastAudioPTS, now, m.lastAudioTime) {
			return nativeClockError("BIGO native audio timestamp jumped; a new media connection is required")
		}
		if nativeClockJump(pts, m.lastVideoDTS, now, m.lastVideoTime) {
			return nativeClockError("BIGO native audio and video clocks diverged; a new media connection is required")
		}
	}
	if pts-m.lastTables >= 45000 || f.key {
		if err := m.tables(); err != nil {
			return err
		}
		m.lastTables = pts
	}
	pid, id := uint16(257), byte(0xc0)
	if m.opus {
		id = 0xbd
	}
	if f.video {
		pid, id = 256, 0xe0
	}
	pes := []byte{0, 0, 1, id, 0, 0, 0x80, 0x80, 5}
	if f.video {
		pes[7], pes[8] = 0xc0, 10
		pes = append(pes, tsTimestamp(pts, 3)...)
		pes = append(pes, tsTimestamp(dts, 1)...)
	} else {
		pes = append(pes, tsTimestamp(pts, 2)...)
	}
	data := f.data
	if !f.video && m.opus {
		data = opusAccessUnit(data)
	}
	pes = append(pes, data...)
	if !f.video {
		if len(pes)-6 > 65535 {
			return errors.New("BIGO native audio frame is too large")
		}
		binary.BigEndian.PutUint16(pes[4:], uint16(len(pes)-6))
	}
	if err := m.packets(pid, pes, f.video || m.audioOnly, f.key, dts); err != nil {
		return err
	}
	m.timing.record(f, pts, dts)
	if f.video {
		m.lastVideoDTS, m.lastVideoTime = dts, now
	} else {
		m.lastAudioPTS, m.lastAudioTime = pts, now
	}
	if m.clock != nil {
		m.clock.mu.Lock()
		m.clock.lastPTS = max(m.clock.lastPTS, pts)
		if f.video {
			m.clock.lastDTS = dts
		}
		m.clock.mu.Unlock()
	}
	return nil
}

// Independent media sockets can briefly lead or lag each other. A timestamp
// changing ten seconds more than elapsed time is a discontinuity, not ordinary
// network jitter or video reordering. Reject it before advancing either clock:
// clamping all later video to one bad future timestamp loses synchronization.
func nativeClockJump(current, previous int64, now, received time.Time) bool {
	if received.IsZero() {
		return false
	}
	delta := current - previous
	elapsed := max(int64(0), now.Sub(received).Milliseconds()) * 90
	const tolerance = 10 * 90000
	return delta < -tolerance || delta > elapsed+tolerance
}

func tsTimestamp(v int64, prefix byte) []byte {
	x := uint64(v) & 0x1ffffffff
	return []byte{prefix<<4 | byte(x>>29)&14 | 1, byte(x >> 22), byte(x>>14)&0xfe | 1, byte(x >> 7), byte(x<<1) | 1}
}

func (m *nativeTSMux) tables() error {
	pat := []byte{0, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0}
	if err := m.section(0, pat); err != nil {
		return err
	}
	pcr := byte(0)
	if m.audioOnly {
		pcr = 1
	}
	pmt := []byte{2, 0xb0, 0, 0, 1, 0xc1, 0, 0, 0xe1, pcr, 0xf0, 0}
	if !m.audioOnly {
		pmt = append(pmt, m.codec, 0xe1, 0, 0xf0, 0)
	}
	if !m.noAudio {
		if m.opus {
			pmt = append(pmt, 0x06, 0xe1, 1, 0xf0, 10, 0x05, 4, 'O', 'p', 'u', 's', 0x7f, 2, 0x80, 2)
		} else {
			pmt = append(pmt, 0x0f, 0xe1, 1, 0xf0, 0)
		}
	}
	pmt[2] = byte(len(pmt) + 1) // Section length includes its four-byte CRC.
	return m.section(4096, pmt)
}

func (m *nativeTSMux) section(pid uint16, data []byte) error {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	section := make([]byte, 1, len(data)+5)
	section = append(section, data...)
	section = binary.BigEndian.AppendUint32(section, crc)
	return m.packets(pid, section, false, false, 0)
}

func (m *nativeTSMux) packets(pid uint16, data []byte, pcr, key bool, dts int64) error {
	first := true
	for len(data) > 0 {
		var packet [188]byte
		for i := range packet {
			packet[i] = 255
		}
		packet[0], packet[1], packet[2] = 0x47, byte(pid>>8)&31, byte(pid)
		if first {
			packet[1] |= 0x40
		}
		packet[3] = 0x10 | m.nextContinuity(pid)&15
		adapt := 0
		if first && pcr {
			adapt = 8
		}
		n := min(len(data), 184-adapt)
		adapt = 184 - n
		start := 4
		if adapt > 0 {
			packet[3] |= 0x20
			packet[4] = byte(adapt - 1)
			start += adapt
			if adapt > 1 {
				packet[5] = 0
				if first && pcr {
					packet[5] = 0x10
					if key {
						packet[5] |= 0x40
					}
					x := uint64(dts) & 0x1ffffffff
					copy(packet[6:12], []byte{byte(x >> 25), byte(x >> 17), byte(x >> 9), byte(x >> 1), byte(x<<7) | 0x7e, 0})
				}
			}
		}
		copy(packet[start:], data[:n])
		data = data[n:]
		if n, err := m.out.Write(packet[:]); err != nil {
			return err
		} else if n != len(packet) {
			return io.ErrShortWrite
		}
		first = false
	}
	return nil
}

func (m *nativeTSMux) nextContinuity(pid uint16) byte {
	if m.clock == nil {
		value := m.continuity[pid]
		m.continuity[pid]++
		return value
	}
	// Continue the same transport stream across socket reconnects, including
	// PAT/PMT counters, so FFmpeg does not mistake recovery for packet loss.
	m.clock.mu.Lock()
	defer m.clock.mu.Unlock()
	if m.clock.continuity == nil {
		m.clock.continuity = make(map[uint16]byte)
	}
	value := m.clock.continuity[pid]
	m.clock.continuity[pid]++
	return value
}
