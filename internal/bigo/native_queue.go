package bigo

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"

	"bigo-live-recorder/internal/fault"
)

// A slow file handoff must not stop reading the media sockets. Spill to a
// private temporary file when the bounded memory queue is full. Once spilling,
// subsequent frames stay on disk until it is drained, preserving arrival order.
type nativeFrameQueue struct {
	mu            sync.Mutex
	ready         chan struct{}
	frames        []nativeMediaFrame
	head          int
	bytes         int
	limit         int
	file          *os.File
	read, written int64
	closed        bool
	err           error
}

func newNativeFrameQueue(limit int) *nativeFrameQueue {
	return &nativeFrameQueue{ready: make(chan struct{}, 1), limit: limit}
}

func (q *nativeFrameQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *nativeFrameQueue) push(f nativeMediaFrame) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return io.ErrClosedPipe
	}
	if len(f.data) > maxNativeFrameBytes {
		return errors.New("BIGO native frame exceeds the capture buffer limit")
	}
	if q.file == nil && q.bytes+len(f.data) <= q.limit && len(q.frames)-q.head < 256 {
		if q.head >= 128 {
			n := copy(q.frames, q.frames[q.head:])
			clear(q.frames[n:])
			q.frames, q.head = q.frames[:n], 0
		}
		q.frames = append(q.frames, f)
		q.bytes += len(f.data)
	} else {
		if q.file == nil {
			file, err := os.CreateTemp("", "bigo-media-*")
			if err != nil {
				return fault.Action("could not buffer incoming stream data on disk; check disk space and temporary folder permissions", err)
			}
			q.file = file
		}
		var header [24]byte
		binary.LittleEndian.PutUint32(header[:4], uint32(len(f.data)))
		binary.LittleEndian.PutUint32(header[4:8], f.pts)
		binary.LittleEndian.PutUint32(header[8:12], f.dts)
		header[12] = f.codec
		binary.LittleEndian.PutUint64(header[16:], uint64(f.received))
		if f.video {
			header[13] |= 1
		}
		if f.key {
			header[13] |= 2
		}
		if f.reset {
			header[13] |= 4
		}
		if n, err := q.file.WriteAt(header[:], q.written); err != nil {
			return fault.Action("could not buffer incoming stream data on disk; check disk space and temporary folder permissions", err)
		} else if n != len(header) {
			return fault.Action("could not write all buffered stream data to disk; check available disk space", io.ErrShortWrite)
		}
		if n, err := q.file.WriteAt(f.data, q.written+24); err != nil {
			return fault.Action("could not buffer incoming stream data on disk; check disk space and temporary folder permissions", err)
		} else if n != len(f.data) {
			return fault.Action("could not write all buffered stream data to disk; check available disk space", io.ErrShortWrite)
		}
		q.written += int64(24 + len(f.data))
	}
	q.signal()
	return nil
}

func (q *nativeFrameQueue) next(ctx context.Context) (nativeMediaFrame, error) {
	for {
		q.mu.Lock()
		if q.head < len(q.frames) {
			f := q.frames[q.head]
			q.frames[q.head] = nativeMediaFrame{}
			q.head++
			q.bytes -= len(f.data)
			if q.head == len(q.frames) {
				q.frames = q.frames[:0]
				q.head = 0
			}
			q.mu.Unlock()
			return f, nil
		}
		if q.file != nil && q.read < q.written {
			f, err := q.readFrame()
			q.mu.Unlock()
			return f, err
		}
		if q.closed {
			err := q.err
			q.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return nativeMediaFrame{}, err
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nativeMediaFrame{}, ctx.Err()
		case <-q.ready:
		}
	}
}

func (q *nativeFrameQueue) readFrame() (nativeMediaFrame, error) {
	var header [24]byte
	if _, err := q.file.ReadAt(header[:], q.read); err != nil {
		return nativeMediaFrame{}, fault.Action("could not read buffered stream data from disk", err)
	}
	size := int(binary.LittleEndian.Uint32(header[:4]))
	if size > maxNativeFrameBytes || int64(size) > q.written-q.read-24 {
		return nativeMediaFrame{}, fault.Action("buffered stream data could not be read safely", errors.New("invalid BIGO capture buffer frame"))
	}
	f := nativeMediaFrame{data: make([]byte, size), pts: binary.LittleEndian.Uint32(header[4:8]), dts: binary.LittleEndian.Uint32(header[8:12]), codec: header[12], video: header[13]&1 != 0, key: header[13]&2 != 0, reset: header[13]&4 != 0}
	f.received = int64(binary.LittleEndian.Uint64(header[16:]))
	if _, err := q.file.ReadAt(f.data, q.read+24); err != nil {
		return nativeMediaFrame{}, fault.Action("could not read buffered stream data from disk", err)
	}
	q.read += int64(24 + size)
	if q.read == q.written {
		q.removeFile()
	}
	return f, nil
}

func (q *nativeFrameQueue) finish(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed, q.err = true, err
		q.signal()
	}
}

func (q *nativeFrameQueue) removeFile() {
	if q.file != nil {
		name := q.file.Name()
		q.file.Close()
		os.Remove(name)
		q.file = nil
		q.read, q.written = 0, 0
	}
}

func (q *nativeFrameQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	clear(q.frames)
	q.frames, q.head, q.bytes = nil, 0, 0
	q.removeFile()
	q.signal()
}
