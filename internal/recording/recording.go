// Package recording copies the original media into short-cluster Matroska files.
// FFmpeg is fed decrypted MPEG-TS on stdin; it never receives account credentials.
package recording

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/ffmpeg"
	"bigo-live-recorder/internal/state"
)

type Result struct {
	Path            string
	Bytes           int64
	Source          string
	StartedAt       time.Time
	EndedAt         time.Time
	Err             error
	FFmpegWarnings  string
	RoomEnded       bool
	RoomModeChanged bool
}

type Handle struct {
	Path           string
	StartedAt      time.Time
	cancel         context.CancelFunc
	done           chan struct{}
	result         Result // published by closing done
	mu             sync.Mutex
	currentPath    string
	currentStarted time.Time
	changes        []FileChange
	roomKind       bigo.RoomKind
	activeFiles    map[string]time.Time // Present only for concurrent guest files.
	broadcast      state.Broadcast
}

func (h *Handle) Stop()                 { h.cancel() }
func (h *Handle) Done() <-chan struct{} { return h.done }
func (h *Handle) Result() Result        { <-h.done; return h.result }

func (h *Handle) File() string            { h.mu.Lock(); defer h.mu.Unlock(); return h.currentPath }
func (h *Handle) Started() time.Time      { h.mu.Lock(); defer h.mu.Unlock(); return h.currentStarted }
func (h *Handle) RoomKind() bigo.RoomKind { return h.roomKind }

// FirstFile is stable even when a buffered input rolls over before the
// monitoring loop has saved the initial history entry.
func (h *Handle) FirstFile() (string, time.Time) { return h.Path, h.StartedAt }

func FindFFmpeg(configured string) (string, error) {
	return ffmpeg.Find(configured)
}

// Start returns only after FFmpeg has started. Context cancellation closes the
// HLS reader and stdin, allowing FFmpeg to finalize the MKV before a bounded kill.
func Start(parent context.Context, s state.Streamer, source bigo.Stream, cfg state.Config, recovery bigo.StreamRecovery, settings ...Options) (*Handle, error) {
	ffmpeg, err := FindFFmpeg(cfg.FFmpegPath)
	if err != nil {
		return nil, fault.Action("FFmpeg was not found; install or select it in Settings", err)
	}
	ctx, cancel := context.WithCancel(parent)
	options := Options{}
	if len(settings) > 0 {
		options = settings[0]
	}
	splitInterval := time.Duration(cfg.SplitMinutes) * time.Minute
	if source.RoomKind == bigo.RoomHybridMultiGuest || source.RoomKind == bigo.RoomAudioMultiGuest {
		if options.Broadcast.ID == "" {
			options.Broadcast = NewBroadcast(s, source, cfg, time.Now())
		}
		room, err := bigo.OpenGuestRoom(ctx, source, recovery, splitInterval)
		if err != nil {
			cancel()
			return nil, err
		}
		h := startGuestRoom(ctx, cancel, ffmpeg, s, cfg, room, recovery, options)
		h.roomKind = source.RoomKind
		return h, nil
	}
	if cfg.SplitMinutes > 0 && options.Broadcast.ID == "" {
		options.Broadcast = NewSegmentedBroadcast(s, source, cfg, time.Now())
	}
	capture, err := bigo.OpenCapture(ctx, &http.Client{Timeout: 25 * time.Second}, source, bigo.Origin+"/"+s.ID, recovery, splitInterval)
	if err != nil {
		cancel()
		return nil, err
	}
	if options.Broadcast.ID != "" {
		capture = participantCapture{Capture: capture, broadcast: options.Broadcast}
	}
	h, err := startCapture(ctx, cancel, ffmpeg, s, cfg, capture, recovery.Report)
	if h != nil {
		h.roomKind = source.RoomKind
		h.broadcast = options.Broadcast
		if h.broadcast.Segmented && h.broadcast.StartedAt.IsZero() {
			h.broadcast.StartedAt = h.StartedAt
		}
	}
	return h, err
}

func startCapture(ctx context.Context, cancel context.CancelFunc, ffmpeg string, s state.Streamer, cfg state.Config, capture bigo.Capture, report func(string, string), settings ...captureOptions) (*Handle, error) {
	part, err := capture.Next(ctx)
	if err != nil {
		cancel()
		capture.Close()
		return nil, err
	}
	current, err := startFile(ffmpeg, s, cfg, part, settings...)
	if err != nil {
		cancel()
		part.Close()
		capture.Close()
		return nil, err
	}
	h := &Handle{Path: current.path, StartedAt: current.started, currentPath: current.path, currentStarted: current.started, cancel: cancel, done: make(chan struct{})}
	go h.record(ctx, capture, current, ffmpeg, s, cfg, report, settings...)
	return h, nil
}

func recordingArgs(stream io.ReadCloser, path string) []string {
	return append(recordingInputArgs(stream), "-f", "matroska", "-cluster_time_limit", "1000", "-cluster_size_limit", "1048576", "-flush_packets", "1", path)
}

func recordingInputArgs(stream io.ReadCloser) []string {
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostats", "-n"}
	clock, ok := stream.(interface{ NativeTimestamps() bool })
	native := ok && clock.NativeTimestamps()
	if native {
		// Native recovery already normalizes both tracks onto one clock. The
		// MPEG-TS discontinuity fixer can otherwise shift them independently.
		// A short native capture can contain too few TS packets for format probing.
		args = append(args, "-copyts", "-start_at_zero", "-f", "mpegts")
	}
	args = append(args, "-i", "pipe:0", "-map", "0:v?", "-map", "0:a?", "-c", "copy")
	if !native {
		// The web feed can have a stray video PTS even when DTS is continuous.
		// Use DTS when PTS leads by over ten seconds. FFmpeg uses MinInt64
		// for a missing timestamp.
		args = append(args, "-bsf:v", `setts=pts='if(gt((PTS-DTS)*TB,10)*gt(DTS,-9223372036854775808),DTS,PTS)'`)
	}
	return args
}

func recordingPath(s state.Streamer, cfg state.Config, stamp time.Time, suffix string) string {
	return filepath.Join(recordingDirectory(s, cfg), safeName(s.ID)+"_"+stamp.Format("2006-01-02_15-04-05")+"_"+suffix+".mkv")
}

func recordingDirectory(s state.Streamer, cfg state.Config) string {
	if cfg.FolderPerStreamer {
		return filepath.Join(cfg.RecordingPath, safeName(s.ID)+"_"+safeName(s.DisplayName()))
	}
	return cfg.RecordingPath
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

func safeName(s string) string {
	return shortName(s, 90)
}

func shortName(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
		if b.Len() >= limit {
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
