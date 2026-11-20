package recording

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bigo-live-recorder/internal/bigo"
)

// Native captures already expose safe file boundaries. Web captures need a
// continuous remuxer: FFmpeg emits independently decodable transport segments
// to a private loopback endpoint. Feeding those segments to the current writer
// keeps reception open while a requested size cut advances to the next file.
type sizeCapture struct {
	ctx    context.Context
	cancel context.CancelFunc
	ffmpeg string
	source bigo.Capture
	web    *webSizeCapture
	mu     sync.Mutex
}

func (c *sizeCapture) Next(ctx context.Context) (*bigo.CapturePart, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer cancel()
	defer stop()
	for {
		if err := c.ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		web := c.web
		c.mu.Unlock()
		if web != nil {
			part, err := web.Next(ctx)
			if !errors.Is(err, io.EOF) {
				return part, err
			}
			web.Close()
			c.mu.Lock()
			c.web = nil
			c.mu.Unlock()
		}
		part, err := c.source.Next(ctx)
		if err != nil || part.RequestSplit != nil {
			return part, err
		}
		web, err = openWebSizeCapture(c.ctx, c.ffmpeg, part)
		if err != nil {
			part.Close()
			return nil, err
		}
		c.mu.Lock()
		c.web = web
		c.mu.Unlock()
	}
}

func (c *sizeCapture) Close() error {
	c.cancel()
	c.mu.Lock()
	web := c.web
	c.mu.Unlock()
	if web != nil {
		web.Close()
	}
	return c.source.Close()
}

type webSizeCapture struct {
	ctx    context.Context
	cancel context.CancelFunc
	source *bigo.CapturePart
	server *http.Server
	parts  chan *bigo.CapturePart
	done   chan struct{}
	err    error // Published by closing done.

	requests sync.Mutex
	next     int64
	progress chan struct{}
	mu       sync.Mutex
	writer   *io.PipeWriter
	request  *atomic.Bool
	started  bool
}

func openWebSizeCapture(parent context.Context, ffmpeg string, source *bigo.CapturePart) (*webSizeCapture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start recording segment transport: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	c := &webSizeCapture{ctx: ctx, cancel: cancel, source: source, parts: make(chan *bigo.CapturePart), done: make(chan struct{}), progress: make(chan struct{})}
	prefix := "/" + rand.Text() + "/"
	c.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		index, err := strconv.ParseInt(strings.TrimSuffix(name, ".ts"), 10, 64)
		if err != nil || index < 0 || !strings.HasSuffix(name, ".ts") {
			http.NotFound(w, r)
			return
		}
		c.segment(w, r, index)
	})}
	args := recordingInputArgs(source)
	// Every independently consumed segment needs codec headers, even when
	// the upstream encoder sends them only at the start of the broadcast.
	filtered := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-bsf:v" {
			args[i+1] += ",dump_extra=freq=keyframe"
			filtered = true
		}
	}
	if !filtered {
		args = append(args, "-bsf:v", "dump_extra=freq=keyframe")
	}
	args = append(args, "-f", "segment", "-segment_format", "mpegts",
		"-segment_format_options", "mpegts_flags=+resend_headers+initial_discontinuity:mpegts_copyts=1",
		"-segment_time", "1", "-segment_list_type", "flat", "-segment_list", "pipe:1",
		"http://"+listener.Addr().String()+prefix+"%d.ts")
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	configureCommand(cmd)
	// This is private process transport, independent of network proxy settings.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToLower(name) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		listener.Close()
		return nil, err
	}
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	completed := &segmentCount{}
	cmd.Stdout = completed
	if err := cmd.Start(); err != nil {
		stdin.Close()
		cancel()
		listener.Close()
		return nil, fmt.Errorf("start recording segment remuxer: %w", err)
	}
	go c.server.Serve(listener)
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, source)
		stdin.Close()
		source.Close()
		copied <- err
	}()
	go func() {
		err := cmd.Wait()
		if err == nil && ctx.Err() == nil {
			// Closing an HTTP output does not wait for its server handler.
			// Drain every segment reported by FFmpeg before publishing EOF.
			c.drain(completed.Load())
		}
		// If FFmpeg exits before consuming its source, unblock the copier.
		source.Close()
		copyErr := <-copied
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if err != nil {
			err = recordingFFmpegFailure(ffmpegCause(err, stderr.String()), stderr.String())
		} else {
			err = copyErr
		}
		c.mu.Lock()
		if c.writer != nil {
			c.writer.CloseWithError(err)
		}
		c.err = err
		c.mu.Unlock()
		close(c.done)
		c.server.Close()
		cancel()
	}()
	return c, nil
}

type segmentCount struct{ atomic.Int64 }

func (c *segmentCount) Write(p []byte) (int, error) {
	var count int64
	for _, b := range p {
		if b == '\n' {
			count++
		}
	}
	c.Add(count)
	return len(p), nil
}

func (c *webSizeCapture) drain(count int64) {
	for {
		c.requests.Lock()
		if c.next >= count {
			c.requests.Unlock()
			return
		}
		progress := c.progress
		c.requests.Unlock()
		select {
		case <-progress:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *webSizeCapture) segment(w http.ResponseWriter, r *http.Request, index int64) {
	defer r.Body.Close()
	// Separate HTTP connections can reach their handlers out of order.
	// Serialize by the muxer's segment index, rather than lock acquisition.
	for {
		c.requests.Lock()
		if index == c.next {
			break
		}
		progress := c.progress
		duplicate := index < c.next
		c.requests.Unlock()
		if duplicate {
			http.Error(w, "segment already received", http.StatusConflict)
			return
		}
		select {
		case <-progress:
		case <-c.ctx.Done():
			return
		}
	}
	defer func() {
		c.next++
		close(c.progress)
		c.progress = make(chan struct{})
		c.requests.Unlock()
	}()
	c.mu.Lock()
	if c.writer == nil || c.request.Load() {
		if c.writer != nil {
			c.writer.Close()
		}
		reader, writer := io.Pipe()
		request := &atomic.Bool{}
		part := *c.source
		part.ReadCloser = reader
		part.RequestSplit = func() { request.Store(true) }
		if c.started {
			part.Reason = "Recording file size reached; continuing in a new file"
		}
		c.writer, c.request, c.started = writer, request, true
		c.mu.Unlock()
		select {
		case c.parts <- &part:
		case <-c.ctx.Done():
			reader.Close()
			writer.CloseWithError(c.ctx.Err())
			return
		}
	} else {
		c.mu.Unlock()
	}
	c.mu.Lock()
	writer := c.writer
	c.mu.Unlock()
	if _, err := io.Copy(writer, r.Body); err != nil {
		writer.CloseWithError(err)
		http.Error(w, "segment transfer failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (c *webSizeCapture) Next(ctx context.Context) (*bigo.CapturePart, error) {
	select {
	case part := <-c.parts:
		return part, nil
	case <-c.done:
		if c.err != nil {
			return nil, c.err
		}
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *webSizeCapture) Close() error {
	c.cancel()
	c.source.Close()
	c.mu.Lock()
	if c.writer != nil {
		c.writer.CloseWithError(c.ctx.Err())
	}
	c.mu.Unlock()
	c.server.Close()
	<-c.done
	return nil
}
