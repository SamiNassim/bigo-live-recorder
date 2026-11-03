package timeline

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const frameSize = 48
const frameMagic = "BGTL0001"

type Frame struct {
	Received   time.Duration
	SourcePTS  uint32
	SourceDTS  uint32
	PTS, DTS   int64
	Duration   time.Duration
	Checksum   uint32
	Video, Key bool
	Codec      byte
}

type Frames struct {
	mu      sync.Mutex
	file    *os.File
	buffer  *bufio.Writer
	err     error
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
	report  func(error)
}

// Create records each muxed frame in 48 bytes without retaining a growing
// in-memory timeline. A periodic flush keeps an interrupted recording usable.
func (j *Journal) Create(entry Entry, report func(error)) (*Frames, error) {
	if filepath.Base(entry.File) != entry.File || entry.File == "." {
		return nil, errors.New("invalid timeline media filename")
	}
	entry.Kind = "file"
	entry.Timing = filepath.ToSlash(filepath.Join(".timeline", entry.File+".bin"))
	f, err := os.OpenFile(filepath.Join(j.dir, entry.Timing), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	if _, err = io.WriteString(f, frameMagic); err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = j.Append(entry)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &Frames{file: f, buffer: bufio.NewWriterSize(f, 16<<10), done: make(chan struct{}), stopped: make(chan struct{}), report: report}
	go w.flushLoop()
	return w, nil
}

func (w *Frames) fail(err error) {
	if err != nil && w.err == nil {
		w.err = err
		if w.report != nil {
			w.report(err)
		}
	}
}

func (w *Frames) Write(f Frame) {
	var b [frameSize]byte
	binary.LittleEndian.PutUint64(b[0:8], uint64(f.Received))
	binary.LittleEndian.PutUint32(b[8:12], f.SourcePTS)
	binary.LittleEndian.PutUint32(b[12:16], f.SourceDTS)
	binary.LittleEndian.PutUint64(b[16:24], uint64(f.PTS))
	binary.LittleEndian.PutUint64(b[24:32], uint64(f.DTS))
	binary.LittleEndian.PutUint64(b[32:40], uint64(f.Duration))
	binary.LittleEndian.PutUint32(b[40:44], f.Checksum)
	if f.Video {
		b[44] |= 1
	}
	if f.Key {
		b[44] |= 2
	}
	b[45] = f.Codec
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil || w.file == nil {
		return
	}
	_, err := w.buffer.Write(b[:])
	w.fail(err)
}

func (w *Frames) flushLoop() {
	defer close(w.stopped)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.mu.Lock()
			if w.err == nil {
				w.fail(w.buffer.Flush())
				if w.err == nil {
					w.fail(w.file.Sync())
				}
			}
			w.mu.Unlock()
		}
	}
}

func (w *Frames) Close() error {
	w.once.Do(func() {
		close(w.done)
		<-w.stopped
		w.mu.Lock()
		defer w.mu.Unlock()
		w.fail(w.buffer.Flush())
		w.fail(w.file.Sync())
		w.fail(w.file.Close())
		w.file = nil
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Walk ignores only a truncated final frame. Earlier evidence remains useful
// after an application or machine crash; no repair touches the original media.
func Walk(path string, visit func(Frame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return err
	}
	if string(magic[:]) != frameMagic {
		return errors.New("unsupported timing sidecar")
	}
	var b [frameSize]byte
	for {
		if _, err := io.ReadFull(r, b[:]); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		} else if err != nil {
			return err
		}
		frame := Frame{Received: time.Duration(binary.LittleEndian.Uint64(b[:8])), SourcePTS: binary.LittleEndian.Uint32(b[8:12]), SourceDTS: binary.LittleEndian.Uint32(b[12:16]), PTS: int64(binary.LittleEndian.Uint64(b[16:24])), DTS: int64(binary.LittleEndian.Uint64(b[24:32])), Duration: time.Duration(binary.LittleEndian.Uint64(b[32:40])), Checksum: binary.LittleEndian.Uint32(b[40:44]), Video: b[44]&1 != 0, Key: b[44]&2 != 0, Codec: b[45]}
		if frame.Received <= 0 || frame.Duration < 0 || frame.Duration > time.Second {
			return errors.New("invalid frame timing evidence")
		}
		if err := visit(frame); err != nil {
			return err
		}
	}
}
