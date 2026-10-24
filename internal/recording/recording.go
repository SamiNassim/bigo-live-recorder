// Package recording copies the original media into short-cluster Matroska files.
// FFmpeg is fed decrypted MPEG-TS on stdin; it never receives account credentials.
package recording

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
)

type Result struct {
	Path    string
	Bytes   int64
	EndedAt time.Time
	Err     error
}

type Handle struct {
	Path      string
	StartedAt time.Time
	cancel    context.CancelFunc
	done      chan struct{}
	result    Result // published by closing done
}

func (h *Handle) Stop()                 { h.cancel() }
func (h *Handle) Done() <-chan struct{} { return h.done }
func (h *Handle) Result() Result        { <-h.done; return h.result }

func (h *Handle) File() string       { return h.Path }
func (h *Handle) Started() time.Time { return h.StartedAt }

func FindFFmpeg(configured string) (string, error) {
	candidates := []string{configured}
	if configured == "" {
		candidates = []string{"ffmpeg"}
		if runtime.GOOS == "darwin" {
			candidates = append(candidates, "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg")
		}
		if executable, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(executable), "ffmpeg"), filepath.Join(filepath.Dir(executable), "ffmpeg.exe"))
		}
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("FFmpeg was not found; install it or choose its executable in Settings")
}

// Start returns only after FFmpeg has started. Context cancellation closes the
// HLS reader and stdin, allowing FFmpeg to finalize the MKV before a bounded kill.
func Start(parent context.Context, s state.Streamer, source string, cfg state.Config) (*Handle, error) {
	ffmpeg, err := FindFFmpeg(cfg.FFmpegPath)
	if err != nil {
		return nil, err
	}
	dir := cfg.RecordingPath
	if cfg.FolderPerStreamer {
		dir = filepath.Join(dir, safeName(s.DisplayName())+"_"+safeName(s.ID))
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create recording folder: %w", err)
	}
	stamp := time.Now()
	var suffix [4]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	name := safeName(s.DisplayName()) + "_" + stamp.Format("2006-01-02_15-04-05") + "_" + hex.EncodeToString(suffix[:]) + ".mkv"
	path := filepath.Join(dir, name)
	ctx, cancel := context.WithCancel(parent)
	stream, err := bigo.OpenStream(ctx, &http.Client{Timeout: 25 * time.Second}, source, bigo.Origin+"/"+s.ID)
	if err != nil {
		cancel()
		return nil, err
	}
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "warning", "-nostats", "-n", "-i", "pipe:0", "-map", "0:v?", "-map", "0:a?", "-c", "copy", "-f", "matroska", "-cluster_time_limit", "1000", "-cluster_size_limit", "1048576", "-flush_packets", "1", path)
	configureCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		stream.Close()
		return nil, err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		cancel()
		stream.Close()
		stdin.Close()
		return nil, fmt.Errorf("start FFmpeg: %w", err)
	}
	h := &Handle{Path: path, StartedAt: stamp, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		defer cancel()
		defer stream.Close()
		copyDone := make(chan error, 1)
		go func() { _, e := io.Copy(stdin, stream); stdin.Close(); copyDone <- e }()
		wait := make(chan error, 1)
		go func() { wait <- cmd.Wait() }()
		var waitErr, copyErr error
		select {
		case waitErr = <-wait:
			stream.Close()
			stdin.Close()
			copyErr = <-copyDone
		case copyErr = <-copyDone:
			waitErr = finish(cmd, wait)
		case <-ctx.Done():
			stream.Close()
			stdin.Close()
			waitErr = finish(cmd, wait)
			copyErr = <-copyDone
		}
		// Stop is a normal graceful end, but an encoder/muxer failure is not.
		if waitErr != nil {
			err = fmt.Errorf("FFmpeg: %w: %s", waitErr, stderr.String())
		} else if ctx.Err() == nil && copyErr != nil {
			err = copyErr
		} else {
			err = nil
		}
		info, statErr := os.Stat(path)
		var size int64
		if statErr == nil {
			size = info.Size()
		}
		if size == 0 && err == nil {
			err = errors.New("FFmpeg produced no media")
		}
		if cfg.ConvertToMP4 && size > 0 && waitErr == nil && parent.Err() == nil {
			if converted, e := convert(parent, ffmpeg, path); e != nil {
				err = errors.Join(err, e)
			} else if cfg.DeleteAfterConversion {
				if e = os.Remove(path); e != nil {
					err = errors.Join(err, e)
				} else {
					path = converted
					if info, e = os.Stat(path); e == nil {
						size = info.Size()
					}
				}
			}
		}
		h.result = Result{Path: path, Bytes: size, EndedAt: time.Now(), Err: err}
	}()
	return h, nil
}

func finish(cmd *exec.Cmd, wait <-chan error) error {
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case err := <-wait:
		return err
	case <-t.C:
		_ = cmd.Process.Kill()
		return <-wait
	}
}

func convert(parent context.Context, ffmpeg, source string) (string, error) {
	target := strings.TrimSuffix(source, filepath.Ext(source)) + ".mp4"
	// A failed or interrupted conversion never removes the original MKV.
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-i", source, "-map", "0", "-c", "copy", "-movflags", "+faststart", target)
	configureCommand(cmd)
	var tail tailBuffer
	cmd.Stderr = &tail
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("MP4 conversion failed (MKV kept): %w: %s", err, tail.String())
	}
	if info, err := os.Stat(target); err != nil || info.Size() == 0 {
		return "", errors.New("MP4 conversion produced no output; MKV kept")
	}
	return target, nil
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
		if b.Len() >= 90 {
			break
		}
	}
	v := strings.Trim(b.String(), " .")
	if v == "" {
		v = "stream"
	}
	base := strings.ToUpper(strings.SplitN(v, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		v = "_" + v
	}
	return v
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = append([]byte(nil), b.data[len(b.data)-8192:]...)
	}
	return len(p), nil
}
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}
