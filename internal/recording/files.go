package recording

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/state"
)

// FileChange reports ordinary independent recordings created by a continuous
// capture. A nil Result is a newly started file. Each file keeps the normal
// collision-resistant filename and its own recording-history entry.
type FileChange struct {
	Path      string
	StartedAt time.Time
	Result    *Result
}

func (h *Handle) DrainFileChanges() []FileChange {
	h.mu.Lock()
	defer h.mu.Unlock()
	changes := h.changes
	h.changes = nil
	return changes
}

func (h *Handle) fileStarted(file *fileWriter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.currentPath, h.currentStarted = file.path, file.started
	h.changes = append(h.changes, FileChange{Path: file.path, StartedAt: file.started})
}

func (h *Handle) fileFinished(file *fileWriter, result Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, FileChange{Path: file.path, StartedAt: file.started, Result: &result})
}

type fileWriter struct {
	path    string
	started time.Time
	part    *bigo.CapturePart
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	copied  chan error
	waited  chan error
	stderr  *tailBuffer
	exitErr *error
}

func startFile(ffmpeg string, s state.Streamer, cfg state.Config, part *bigo.CapturePart) (*fileWriter, error) {
	stamp := time.Now()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	path := recordingPath(s, cfg, stamp, hex.EncodeToString(suffix[:]))
	if part.ParticipantUID != 0 {
		path = recordingPath(s, cfg, stamp, fmt.Sprintf("guest-%d_%s", part.ParticipantUID, hex.EncodeToString(suffix[:])))
	}
	if part.AudioOnly {
		path = strings.TrimSuffix(path, ".mkv") + ".mka"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fault.Action("could not create the recording folder; check disk space and folder permissions", err)
	}
	cmd := exec.Command(ffmpeg, recordingArgs(part, path)...)
	configureCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		stdin.Close()
		return nil, fault.Action("FFmpeg could not start; check its installation in Settings", err)
	}
	f := &fileWriter{path: path, started: stamp, part: part, cmd: cmd, stdin: stdin, copied: make(chan error, 1), waited: make(chan error, 1), stderr: stderr}
	go func() { _, err := io.Copy(stdin, part); stdin.Close(); part.Close(); f.copied <- err }()
	go func() { f.waited <- cmd.Wait() }()
	return f, nil
}

func (f *fileWriter) result(copyErr, waitErr error, canceled bool) Result {
	var err error
	if waitErr != nil {
		cause := ffmpegCause(waitErr, f.stderr.String())
		if !canceled && copyErr != nil {
			cause = errors.Join(copyErr, cause)
		}
		err = fault.Action(ffmpegMessage("finalize the recording", f.stderr.String()), cause)
	} else if !canceled {
		err = copyErr
	}
	var size int64
	if info, statErr := os.Stat(f.path); statErr == nil {
		size = info.Size()
	}
	if size == 0 && err == nil {
		err = fault.Action("FFmpeg produced no saved media", errors.New("recording file is empty or missing"))
	}
	return Result{Path: f.path, Bytes: size, EndedAt: time.Now(), Err: err}
}

func (f *fileWriter) finish(copyErr error, canceled bool) Result {
	f.stdin.Close()
	f.part.Close()
	if f.exitErr != nil {
		return f.result(copyErr, *f.exitErr, canceled)
	}
	return f.result(copyErr, finish(f.cmd, f.waited), canceled)
}

func (h *Handle) record(ctx context.Context, capture bigo.Capture, current *fileWriter, ffmpeg string, s state.Streamer, cfg state.Config, report func(string, string)) {
	var finalizers sync.WaitGroup
	defer func() { h.cancel(); capture.Close(); finalizers.Wait(); close(h.done) }()
	// Finalizers run concurrently with the next writer. Bound process overlap
	// if a muxer stalls repeatedly; reception continues into the capture buffer.
	finishing := make(chan struct{}, 4)
	for {
		var copyErr error
		select {
		case <-ctx.Done():
			current.part.Close()
			current.stdin.Close()
			capture.Close()
			copyErr = <-current.copied
			h.result = current.finish(copyErr, true)
			return
		case waitErr := <-current.waited:
			current.exitErr = &waitErr
			current.part.Close()
			current.stdin.Close()
			copyErr = <-current.copied
		case copyErr = <-current.copied:
		}
		if copyErr != nil || ctx.Err() != nil || (current.exitErr != nil && *current.exitErr != nil) {
			h.result = current.finish(copyErr, ctx.Err() != nil)
			return
		}
		part, nextErr := capture.Next(ctx)
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				nextErr = nil
			}
			h.result = current.finish(nextErr, ctx.Err() != nil)
			return
		}
		select {
		case finishing <- struct{}{}:
		case <-ctx.Done():
			part.Close()
			h.result = current.finish(nil, true)
			return
		}
		next, err := startFile(ffmpeg, s, cfg, part)
		if err != nil {
			<-finishing
			part.Close()
			h.result = current.finish(err, ctx.Err() != nil)
			return
		}
		previous := current
		current = next
		h.fileStarted(current)
		if report != nil {
			message := part.Reason
			if message == "" {
				message = "Media format changed; continuing in a new recording"
			}
			report("INFO", message)
		}
		finalizers.Go(func() { defer func() { <-finishing }(); h.fileFinished(previous, previous.finish(nil, false)) })
	}
}
