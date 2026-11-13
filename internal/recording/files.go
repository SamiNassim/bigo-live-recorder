package recording

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	"bigo-live-recorder/internal/timeline"
)

// FileChange reports ordinary independent recordings created by a continuous
// capture. A nil Result is a newly started file. Each file keeps the normal
// collision-resistant filename and its own recording-history entry.
type FileChange struct {
	Path        string
	StartedAt   time.Time
	Result      *Result
	BroadcastID string
	Guest       bigo.Profile
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
	h.changes = append(h.changes, file.change(nil))
}

func (h *Handle) webFileAdvanced(file *fileWriter, result Result, next string, started time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, file.change(&result))
	file.path, file.started = next, started
	h.currentPath, h.currentStarted = next, started
	h.changes = append(h.changes, file.change(nil))
}

func (h *Handle) fileFinished(file *fileWriter, result Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, file.change(&result))
}

func (f *fileWriter) change(result *Result) FileChange {
	return FileChange{Path: f.path, StartedAt: f.started, Result: result, BroadcastID: f.part.BroadcastID, Guest: bigo.Profile{UID: f.part.ParticipantUID, ID: f.part.ParticipantID, Name: f.part.ParticipantName, IdentityVersion: f.part.ParticipantIdentityVersion}}
}

type fileWriter struct {
	path       string
	started    time.Time
	part       *bigo.CapturePart
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	copied     chan error
	waited     chan error
	stderr     *tailBuffer
	exitErr    *error
	timing     *timeline.Frames
	journal    *timeline.Journal
	timingOnce sync.Once
	segments   *webSegments
	saved      *Result
}

type captureOptions struct {
	timing       *timeline.Journal
	reportTiming func(error)
}

func startFile(ffmpeg string, s state.Streamer, cfg state.Config, part *bigo.CapturePart, settings ...captureOptions) (*fileWriter, error) {
	stamp := time.Now()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	path := recordingPath(s, cfg, stamp, hex.EncodeToString(suffix[:]))
	if part.ParticipantUID != 0 {
		id, name := part.ParticipantID, part.ParticipantName
		if id == "" {
			id = "unidentified-guest"
		}
		if name == "" {
			name = "unknown"
		}
		filename := safeName(s.ID) + "_" + safeName(id) + "_" + shortName(name, 48) + "_" + stamp.Format("2006-01-02_15-04-05") + "_" + hex.EncodeToString(suffix[:]) + ".mkv"
		path = filepath.Join(filepath.Dir(path), filename)
		if part.Directory != "" {
			path = filepath.Join(part.Directory, filename)
		}
	}
	if part.AudioOnly {
		path = strings.TrimSuffix(path, ".mkv") + ".mka"
	}
	if part.Directory != "" {
		path = filepath.Join(part.Directory, filepath.Base(path))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fault.Action("could not create the recording folder; check disk space and folder permissions", err)
	}
	var segments *webSegments
	var args []string
	if !part.Native && cfg.SplitMinutes > 0 {
		segments = &webSegments{base: strings.TrimSuffix(path, filepath.Ext(path))}
		path = segments.path(0)
		args = segments.args(part, cfg.SplitMinutes)
	} else {
		args = recordingArgs(part, path)
	}
	cmd := exec.Command(ffmpeg, args...)
	if segments != nil {
		cmd.Stdout = segments
	}
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
	f := &fileWriter{path: path, started: stamp, part: part, cmd: cmd, stdin: stdin, copied: make(chan error, 1), waited: make(chan error, 1), stderr: stderr, segments: segments}
	if len(settings) > 0 && settings[0].timing != nil && part.Timing != nil && !part.Timing.Epoch.IsZero() {
		options := settings[0]
		f.journal = options.timing
		entry := timeline.Entry{File: filepath.Base(path), UID: part.ParticipantUID, ID: part.ParticipantID, Name: part.ParticipantName, Epoch: part.Timing.Epoch, At: stamp}
		f.timing, err = options.timing.Create(entry, options.reportTiming)
		if err != nil {
			if options.reportTiming != nil {
				options.reportTiming(err)
			}
		} else {
			part.Timing.Observe(func(sample bigo.MediaTime) { f.timing.Write(timeline.Frame(sample)) })
		}
	}
	go func() { _, err := io.Copy(stdin, part); stdin.Close(); part.Close(); f.copied <- err }()
	go func() { f.waited <- cmd.Wait() }()
	return f, nil
}

func (f *fileWriter) result(copyErr, waitErr error, canceled bool) (Result, bool) {
	var err error
	if waitErr != nil {
		cause := ffmpegCause(waitErr, f.stderr.String())
		if !canceled && copyErr != nil {
			cause = errors.Join(copyErr, cause)
		}
		err = recordingFFmpegFailure(cause, f.stderr.String())
	} else if !canceled {
		err = copyErr
	}
	var size int64
	noMedia := false
	if info, statErr := os.Stat(f.path); statErr == nil {
		size = info.Size()
		noMedia = info.Mode().IsRegular() && size == 0
	} else if waitErr != nil && errors.Is(statErr, os.ErrNotExist) {
		// Confirm the destination is accessible before recording an absent
		// output. A disconnected disk leaves the outcome unknown.
		dir, err := os.Stat(filepath.Dir(f.path))
		noMedia = err == nil && dir.IsDir()
	}
	if size == 0 && err == nil {
		err = fault.Action("FFmpeg produced no saved media", errors.New("recording file is empty or missing"))
	}
	return Result{Path: f.path, Bytes: size, EndedAt: time.Now(), Err: err}, noMedia
}

func (f *fileWriter) finish(copyErr error, canceled bool) Result {
	if f.saved != nil {
		result := *f.saved
		if !canceled && result.Err == nil {
			result.Err = copyErr
		}
		return result
	}
	f.stdin.Close()
	f.part.Close()
	var waitErr error
	if f.exitErr != nil {
		waitErr = *f.exitErr
	} else {
		waitErr = finish(f.cmd, f.waited)
	}
	result, noMedia := f.result(copyErr, waitErr, canceled)
	f.timingOnce.Do(func() {
		if f.timing != nil {
			f.part.Timing.Observe(nil)
			err := f.timing.Close()
			entry := timeline.Entry{Kind: "file_end", File: filepath.Base(f.path), At: result.EndedAt, NoMedia: noMedia}
			if err != nil {
				entry.Error = err.Error()
			}
			f.journal.Append(entry)
		}
	})
	return result
}

func (h *Handle) record(ctx context.Context, capture bigo.Capture, current *fileWriter, ffmpeg string, s state.Streamer, cfg state.Config, report func(string, string), settings ...captureOptions) {
	var finalizers sync.WaitGroup
	defer func() { h.cancel(); capture.Close(); finalizers.Wait(); close(h.done) }()
	// Finalizers run concurrently with the next writer. Bound process overlap
	// if a muxer stalls repeatedly; reception continues into the capture buffer.
	finishing := make(chan struct{}, 4)
	for {
		copyErr := current.waitForInput(ctx, capture, h)
		if current.segments != nil {
			current.finishWeb(h, copyErr, ctx.Err() != nil)
			copyErr = current.saved.Err
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
		next, err := startFile(ffmpeg, s, cfg, part, settings...)
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

func (f *fileWriter) waitForInput(ctx context.Context, capture bigo.Capture, h *Handle) error {
	var ticks <-chan time.Time
	if f.segments != nil {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			f.part.Close()
			f.stdin.Close()
			capture.Close()
			return <-f.copied
		case err := <-f.waited:
			f.exitErr = &err
			f.part.Close()
			f.stdin.Close()
			return <-f.copied
		case err := <-f.copied:
			return err
		case <-ticks:
			f.publishWebSegments(h)
		}
	}
}
