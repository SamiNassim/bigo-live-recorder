// Package timeline preserves portable timing evidence alongside original media.
package timeline

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
)

const JournalName = "timeline.jsonl"

type Entry struct {
	Version   int       `json:"version,omitempty"`
	Kind      string    `json:"kind"`
	Broadcast string    `json:"broadcast,omitempty"`
	File      string    `json:"file,omitempty"`
	Timing    string    `json:"timing,omitempty"`
	UID       uint32    `json:"uid,omitempty"`
	ID        string    `json:"id,omitempty"`
	Name      string    `json:"name,omitempty"`
	Epoch     time.Time `json:"epoch,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Journal struct {
	mu   sync.Mutex
	file *os.File
	dir  string
	err  error
}

// Open repairs only an interrupted final append. Completed entries and raw
// media are never rewritten. The recorder owns one Journal per active room.
func Open(dir, broadcast string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Join(dir, ".timeline"), 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, JournalName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	entries, end, err := readJournal(f)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 && (entries[0].Kind != "broadcast" || entries[0].Version != 1 || entries[0].Broadcast != broadcast) {
		return nil, errors.New("the broadcast folder contains an unrelated or unsupported timeline")
	}
	if err := f.Truncate(end); err != nil {
		return nil, err
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		return nil, err
	}
	j := &Journal{file: f, dir: dir}
	f = nil
	if len(entries) == 0 {
		if err := j.Append(Entry{Version: 1, Kind: "broadcast", Broadcast: broadcast, At: time.Now()}); err != nil {
			j.Close()
			return nil, err
		}
	}
	return j, nil
}

func readJournal(r io.Reader) ([]Entry, int64, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	var entries []Entry
	var end int64
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, end, errors.New("timeline entry is too large")
		}
		if errors.Is(err, io.EOF) {
			return entries, end, nil
		}
		if err != nil {
			return nil, end, err
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, end, fmt.Errorf("read broadcast timeline: %w", err)
		}
		entries = append(entries, entry)
		end += int64(len(line))
	}
}

func Read(dir string) ([]Entry, error) {
	f, err := os.Open(filepath.Join(dir, JournalName))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, _, err := readJournal(f)
	return entries, err
}

// MarkEnded persists the room's confirmed end for a portable broadcast folder.
// Pauses, guest departures and application shutdown never call this function.
func MarkEnded(dir, broadcast string, at time.Time) error {
	entries, err := Read(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Kind == "broadcast_end" {
			return nil
		}
	}
	j, err := Open(dir, broadcast)
	if err != nil {
		return err
	}
	err = j.Append(Entry{Kind: "broadcast_end", At: at})
	return errors.Join(err, j.Close())
}

func (j *Journal) Append(entry Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	if j.file == nil {
		return os.ErrClosed
	}
	b, err := json.Marshal(entry)
	if err == nil {
		_, err = j.file.Write(append(b, '\n'))
	}
	if err == nil {
		err = j.file.Sync()
	}
	j.err = err
	return err
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return j.err
	}
	err := j.file.Close()
	j.file = nil
	return errors.Join(j.err, err)
}

// LocalPath permits portable, relative references only. In particular a
// transferred timeline cannot make FFmpeg open a URL or another directory.
func LocalPath(dir, name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", errors.New("timeline media path is not relative to its broadcast folder")
	}
	path := filepath.Join(dir, name)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realDir, realPath)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("timeline media resolves outside its broadcast folder")
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("timeline media is not a regular file")
	}
	return realPath, nil
}
