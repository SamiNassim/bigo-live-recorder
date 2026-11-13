package recording

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FFmpeg sends a CSV row to stdout after closing each file. Keep the same
// input open so cutting an output never reconnects the web stream.
type webSegments struct {
	base      string
	current   int
	mu        sync.Mutex
	pending   []byte
	completed int
}

func (s *webSegments) path(index int) string {
	return fmt.Sprintf("%s-part-%03d.mkv", s.base, index+1)
}

func (s *webSegments) args(stream io.ReadCloser, minutes int) []string {
	pattern := strings.ReplaceAll(s.base, "%", "%%") + "-part-%03d.mkv"
	return append(recordingInputArgs(stream), "-f", "segment", "-segment_format", "matroska",
		"-segment_format_options", "cluster_time_limit=1000:cluster_size_limit=1048576:flush_packets=1",
		"-segment_start_number", "1", "-segment_time", strconv.Itoa(minutes*60),
		"-reset_timestamps", "1", "-segment_list_type", "csv", "-segment_list", "pipe:1", pattern)
}

func (s *webSegments) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, p...)
	for {
		line, rest, ok := bytes.Cut(s.pending, []byte{'\n'})
		if !ok {
			return len(p), nil
		}
		s.pending = rest
		row, err := csv.NewReader(bytes.NewReader(line)).Read()
		if err == nil && len(row) == 3 && row[0] == filepath.Base(s.path(s.completed)) {
			s.completed++
		}
	}
}

func (f *fileWriter) publishWebSegments(h *Handle) {
	s := f.segments
	s.mu.Lock()
	completed := s.completed
	s.mu.Unlock()
	for s.current < completed {
		next := s.path(s.current + 1)
		// FFmpeg reports the old file just before opening the new one. The
		// last report at EOF has no successor.
		if _, err := os.Stat(next); err != nil {
			return
		}
		result, _ := f.result(nil, nil, false)
		s.current++
		h.webFileAdvanced(f, result, next, result.EndedAt)
	}
}

func (f *fileWriter) finishWeb(h *Handle, copyErr error, canceled bool) {
	f.stdin.Close()
	f.part.Close()
	if f.exitErr == nil {
		err := finish(f.cmd, f.waited)
		f.exitErr = &err
	}
	f.publishWebSegments(h)
	result, _ := f.result(copyErr, *f.exitErr, canceled)
	f.saved = &result
}
