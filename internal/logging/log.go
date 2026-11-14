// Package logging keeps bounded on-disk logs and a searchable UI tail.
package logging

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"bigo-live-recorder/internal/fault"
)

const MaxBytes int64 = 5 << 20
const MaxEntries = 5000

type Entry struct {
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Level    string    `json:"level"`
	Streamer string    `json:"streamer,omitempty"`
	Message  string    `json:"message"`
	Details  string    `json:"details,omitempty"`
}
type Log struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	limit    int64
	sequence uint64
	revision uint64
	entries  []Entry
}

func Open(dir string) (*Log, error) { return OpenSize(dir, MaxBytes) }
func OpenSize(dir string, limit int64) (*Log, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	l := &Log{path: filepath.Join(dir, "bigo-live-recorder.log"), limit: limit, revision: 1}
	for _, suffix := range []string{".2", ".1", ""} {
		f, err := os.Open(l.path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 4096), 1<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				l.keep(e)
				l.sequence = max(l.sequence, e.Sequence)
			}
		}
		_ = f.Close()
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	l.file = f
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	l.size = stat.Size()
	return l, nil
}
func (l *Log) keep(e Entry) {
	l.revision++
	if len(l.entries) == MaxEntries {
		copy(l.entries, l.entries[1:])
		l.entries[len(l.entries)-1] = e
	} else {
		l.entries = append(l.entries, e)
	}
}
func (l *Log) Write(level, id, message string) {
	l.WriteError(level, id, message, nil)
}

// WriteError keeps a readable message and a separate, bounded diagnostic cause.
func (l *Log) WriteError(level, id, message string, cause error) {
	l.WriteDetails(level, id, message, "", cause)
}

// WriteDetails adds context to the log file and the copyable details without
// putting it in the main message.
func (l *Log) WriteDetails(level, id, message, details string, cause error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sequence++
	if len(message) > 16000 {
		message = message[:16000] + " [truncated]"
	}
	if cause != nil {
		if details != "" {
			details += "\n"
		}
		details += fault.Details(cause)
	}
	if len(details) > 16000 {
		details = details[:16000] + " [truncated]"
	}
	e := Entry{Sequence: l.sequence, Time: time.Now(), Level: level, Streamer: id, Message: message, Details: details}
	l.keep(e)
	b, _ := json.Marshal(e)
	b = append(b, '\n')
	if l.file == nil {
		return
	}
	if l.size+int64(len(b)) > l.limit {
		if err := l.rotate(); err != nil {
			fmt.Fprintln(os.Stderr, "rotate log:", err)
			return
		}
	}
	n, err := l.file.Write(b)
	l.size += int64(n)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write log:", err)
	}
}
func (l *Log) rotate() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil
	if err := rotateFiles(l.path); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	l.file = f
	l.size = 0
	return err
}

// Keep the current file and two archives for both event and crash logs.
func rotateFiles(path string) error {
	if err := os.Remove(path + ".2"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path+".1", path+".2"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (l *Log) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Revision changes for new entries and when the displayed tail is cleared.
func (l *Log) Revision() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.revision
}

// EntriesSince copies the retained entries only after their revision changes.
func (l *Log) EntriesSince(revision uint64) ([]Entry, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if revision == l.revision {
		return nil, revision
	}
	return append([]Entry(nil), l.entries...), l.revision
}

func (l *Log) ClearView() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
	l.revision++
}
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := errors.Join(l.file.Sync(), l.file.Close())
	l.file = nil
	return err
}

var _ io.Closer = (*Log)(nil)
