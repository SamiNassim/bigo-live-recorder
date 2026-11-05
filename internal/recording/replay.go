package recording

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf16"

	"bigo-live-recorder/internal/ffmpeg"
	"bigo-live-recorder/internal/state"
	"bigo-live-recorder/internal/timeline"
)

var errReplayBatchTooLarge = errors.New("the replay batch exceeds the system command-line limit")

// Replay reconciles the full room and creates a derived viewing copy. All
// originals and timing evidence remain untouched, including on cancellation.
// Only one bounded batch of inputs is opened at once, even for long broadcasts.
func Replay(ctx context.Context, broadcast state.Broadcast, cfg state.Config) (string, error) {
	return assembleReplay(ctx, broadcast, cfg, 60*25)
}

func assembleReplay(ctx context.Context, broadcast state.Broadcast, cfg state.Config, batchFrames int64) (string, error) {
	ff, err := ffmpeg.Find(cfg.FFmpegPath)
	if err != nil {
		return "", err
	}
	if broadcast.EndedAt.IsZero() && broadcast.ReplayThrough.IsZero() {
		return "", errors.New("the recording is still open; finalize guest files before assembling its replay")
	}
	plan, err := timeline.ReconcileThrough(ctx, broadcast.Directory, func(ctx context.Context, path string) ([]timeline.TrackInfo, error) {
		return inspectReplayMedia(ctx, ff, path)
	}, broadcast.ReplayThrough)
	if err != nil {
		return "", err
	}
	if plan.Slots > 64 {
		return "", errors.New("the replay contains more than 64 simultaneous guests")
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", err
	}
	key := sha256.Sum256(append([]byte("replay-v3-speakers/"+strconv.FormatInt(batchFrames, 10)+"/"), encoded...))
	work := filepath.Join(broadcast.Directory, ".replay-work", hex.EncodeToString(key[:8]))
	if err = prepareReplayWork(work, key); err != nil {
		return "", err
	}
	// The plan is useful independently of the rendered copy, and records that
	// cross-guest alignment uses receiver timing rather than a shared server clock.
	if err = writeReplayJSON(filepath.Join(broadcast.Directory, "replay.json"), encoded); err != nil {
		return "", err
	}
	var parts []string
	var durations []float64
	totalFrames := int64(math.Ceil(plan.Duration * 25))
	for firstFrame := int64(0); firstFrame < totalFrames; {
		lastFrame := min(firstFrame+batchFrames, totalFrames)
		start, end := float64(firstFrame)/25, float64(lastFrame)/25
		// A rapidly changing room can contain many short files. Split the
		// batch until it fits the same bounded input budget as a quiet room.
		for countReplayInputs(plan, start, end) > 48 && lastFrame-firstFrame > 1 {
			lastFrame = firstFrame + (lastFrame-firstFrame)/2
			end = float64(lastFrame) / 25
		}
		if countReplayInputs(plan, start, end) > 48 {
			return "", errors.New("too many overlapping guest files to assemble safely")
		}
		name := fmt.Sprintf("part-%06d.mkv", len(parts))
		path := filepath.Join(work, name)
		for {
			duration := float64(lastFrame-firstFrame) / 25
			if validReplayPart(ctx, ff, path, duration) {
				break
			}
			err = renderReplayPart(ctx, ff, broadcast.Directory, work, name, plan, start, end)
			if errors.Is(err, errReplayBatchTooLarge) && lastFrame-firstFrame > 1 {
				lastFrame = firstFrame + (lastFrame-firstFrame)/2
				end = float64(lastFrame) / 25
				continue
			}
			if err != nil {
				return "", err
			}
			break
		}
		parts = append(parts, name)
		durations = append(durations, float64(lastFrame-firstFrame)/25)
		firstFrame = lastFrame
	}
	var list strings.Builder
	for i, name := range parts {
		fmt.Fprintf(&list, "file '%s'\nduration %.9f\n", name, durations[i])
	}
	if err = os.WriteFile(filepath.Join(work, "concat.txt"), []byte(list.String()), 0644); err != nil {
		return "", err
	}
	partial := filepath.Join(work, "replay.pending.mkv")
	os.Remove(partial)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-f", "concat", "-safe", "1", "-i", "concat.txt", "-map", "0:v", "-map", "0:a", "-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-threads", "2", "-f", "matroska", "replay.pending.mkv"}
	if err = runReplayCommand(ctx, ff, work, args); err != nil {
		os.Remove(partial)
		return "", err
	}
	// Decode the complete derived output before publishing it. A process
	// succeeding or a duration tag existing is not proof of a playable replay.
	args = []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-xerror", "-threads", "2", "-i", partial, "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "null", "-"}
	if err = runReplayCommand(ctx, ff, work, args); err != nil {
		os.Remove(partial)
		return "", fmt.Errorf("the assembled replay failed playback validation: %w", err)
	}
	actual, err := inspectReplayMedia(ctx, ff, partial)
	if err != nil {
		return "", err
	}
	var end float64
	for _, t := range actual {
		end = max(end, t.End)
	}
	if math.Abs(end-float64(totalFrames)/25) > .1 {
		return "", errors.New("assembled replay duration does not match the reconciled timeline")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.OpenFile(partial, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return "", err
	}
	stamp := broadcast.StartedAt
	if stamp.IsZero() {
		stamp = plan.Origin
	}
	target := filepath.Join(broadcast.Directory, safeName(broadcast.StreamerID)+"_"+stamp.Format("2006-01-02_15-04-05")+"_replay.mkv")
	if err = os.Rename(partial, target); err != nil {
		return "", err
	}
	// This directory is generated entirely by this job; never clean a raw
	// source directory or another interrupted job while reclaiming work space.
	os.RemoveAll(work)
	return target, nil
}

func prepareReplayWork(work string, key [32]byte) error {
	parent := filepath.Dir(work)
	for _, path := range []string{parent, work} {
		if info, err := os.Lstat(path); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("replay work space is not a regular directory")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	created := false
	if err := os.Mkdir(work, 0755); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	marker := filepath.Join(work, "job.sha256")
	want := hex.EncodeToString(key[:]) + "\n"
	if created {
		if err := os.WriteFile(marker, []byte(want), 0644); err != nil {
			os.Remove(marker)
			os.Remove(work)
			return err
		}
		return nil
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != want {
		entries, listErr := os.ReadDir(work)
		if listErr == nil && (len(entries) == 0 || (len(entries) == 1 && entries[0].Name() == "job.sha256" && strings.HasPrefix(want, string(data)))) {
			return os.WriteFile(marker, []byte(want), 0644)
		}
		return errors.New("an existing replay work directory does not belong to this job; original files are preserved")
	}
	return nil
}

func writeReplayJSON(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".replay-plan-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func runReplayCommand(ctx context.Context, ff, dir string, args []string) error {
	cmd := exec.CommandContext(ctx, ff, args...)
	cmd.Dir = dir
	configureCommand(cmd)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("FFmpeg could not assemble the replay: %s: %w", stderr.String(), err)
	}
	return nil
}

func countReplayInputs(p timeline.Plan, start, end float64) int {
	n := 0
	for _, c := range p.Clips {
		if c.End > start && c.Start < end {
			n++
		}
	}
	return n
}

func validReplayPart(ctx context.Context, ff, path string, duration float64) bool {
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		return false
	}
	tracks, err := inspectReplayMedia(ctx, ff, path)
	if err != nil {
		return false
	}
	video, audio := false, false
	for _, t := range tracks {
		if math.Abs(t.End-duration) > .08 {
			return false
		}
		if t.Video {
			video = true
		} else {
			audio = true
		}
	}
	if !video || !audio {
		return false
	}
	return runReplayCommand(ctx, ff, filepath.Dir(path), []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-xerror", "-threads", "1", "-i", path, "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "null", "-"}) == nil
}

func renderReplayPart(ctx context.Context, ff, dir, work, name string, p timeline.Plan, start, end float64) error {
	const width, height = 1920, 1080
	cols := int(math.Ceil(math.Sqrt(float64(max(1, p.Slots)))))
	rows := (max(1, p.Slots) + cols - 1) / cols
	w, h := (width/cols)/2*2, (height/rows)/2*2
	duration := math.Ceil((end-start)*25-1e-8) / 25
	seconds := func(v float64) string { return strconv.FormatFloat(v, 'f', 9, 64) }
	var graph strings.Builder
	fmt.Fprintf(&graph, "color=c=0x101318:s=%dx%d:r=25:d=%s[base0];\nanullsrc=r=48000:cl=stereo,atrim=duration=%s[quiet];\n", width, height, seconds(duration), seconds(duration))
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-filter_complex_threads", "1"}
	base, input := 0, 0
	audios := []string{"[quiet]"}
	for _, c := range p.Clips {
		from, to := max(start, c.Start), min(end, c.End)
		if from >= to {
			continue
		}
		path, err := timeline.LocalPath(dir, c.File)
		if err != nil {
			return err
		}
		speed := (c.FileEnd - c.FileStart) / (c.End - c.Start)
		if speed < .9 || speed > 1.1 {
			return errors.New("a guest's media and timeline clocks differ too much to assemble automatically")
		}
		seek := c.FileStart + (from-c.Start)*speed
		length := (to - from) * speed
		args = append(args, "-threads", "1", "-reinit_filter", "0", "-seek_timestamp", "1", "-ss", seconds(max(0, seek)), "-t", seconds(length), "-i", path)
		offset, stop := from-start, to-start
		x, y := (c.Slot%cols)*w, (c.Slot/cols)*h
		if c.Video {
			fmt.Fprintf(&graph, "[%d:v]trim=duration=%s,setpts=PTS/%s+%s/TB,scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2:eval=frame,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black:eval=frame,setsar=1[v%d];\n", input, seconds(length), seconds(speed), seconds(offset), w, h-48, w, h-48, input)
			fmt.Fprintf(&graph, "[base%d][v%d]overlay=x=%d:y=%d:eof_action=pass:repeatlast=0:enable='between(t,%s,%s)'[base%d];\n", base, input, x, y, seconds(offset), seconds(stop), base+1)
			base++
		} else {
			fmt.Fprintf(&graph, "[%d:a]atrim=duration=%s,aresample=48000:async=1:first_pts=0:min_hard_comp=0.01,atempo=%s,aformat=channel_layouts=stereo,adelay=%d:all=1[a%d];\n", input, seconds(length), seconds(speed), int64(math.Round(offset*1000)), input)
			audios = append(audios, fmt.Sprintf("[a%d]", input))
		}
		input++
	}
	// Labels are rasterized in Go. User-provided names never enter a filter
	// expression, font lookup, filename, or FFmpeg command option.
	for i, c := range p.Clips {
		from, to := max(start, c.Start), min(end, c.End)
		if from >= to {
			continue
		}
		labelFile := fmt.Sprintf("label-%06d.png", i)
		if err := replayLabel(filepath.Join(work, labelFile), c.ID, c.Name, w); err != nil {
			return err
		}
		args = append(args, "-threads", "1", "-loop", "1", "-framerate", "1", "-t", seconds(duration), "-i", labelFile)
		x, y := (c.Slot%cols)*w, (c.Slot/cols)*h+h-48
		fmt.Fprintf(&graph, "[base%d][%d:v]overlay=x=%d:y=%d:eof_action=repeat:repeatlast=1:enable='between(t,%s,%s)'[base%d];\n", base, input, x, y, seconds(from-start), seconds(to-start), base+1)
		base++
		input++
	}
	speakerImagesReady := false
	for i, c := range p.Clips {
		track, err := replaySpeakerTrack(ctx, ff, dir, work, i, w, h, c, start, end)
		if err != nil {
			return err
		}
		if track == "" {
			continue
		}
		if !speakerImagesReady {
			// Regenerate once per batch so an interrupted PNG write can
			// never poison resumed replay processing.
			for level := uint8(0); level <= 3; level++ {
				name := fmt.Sprintf("speaker-%dx%d-%d.png", w, h, level)
				if err := replaySpeakerImage(filepath.Join(work, name), w, h, level); err != nil {
					return err
				}
			}
			speakerImagesReady = true
		}
		args = append(args, "-threads", "1", "-f", "concat", "-safe", "1", "-i", track)
		x, y := (c.Slot%cols)*w, (c.Slot/cols)*h
		fmt.Fprintf(&graph, "[base%d][%d:v]overlay=x=%d:y=%d:eof_action=pass:repeatlast=0[base%d];\n", base, input, x, y, base+1)
		base++
		input++
	}
	fmt.Fprintf(&graph, "[base%d]format=yuv420p[vout];\n%samix=inputs=%d:normalize=0:dropout_transition=0,alimiter=limit=0.95:level=false:latency=true,atrim=duration=%s[aout]\n", base, strings.Join(audios, ""), len(audios), seconds(duration))
	if err := os.WriteFile(filepath.Join(work, "filters.txt"), []byte(graph.String()), 0644); err != nil {
		return err
	}
	partial := "pending-" + name
	os.Remove(filepath.Join(work, partial))
	args = append(args, "-/filter_complex", "filters.txt", "-map", "[vout]", "-map", "[aout]", "-t", seconds(duration), "-r", "25", "-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-threads", "2", "-c:a", "flac", "-f", "matroska", partial)
	if !replayArgsFit(args) {
		return errReplayBatchTooLarge
	}
	err := runReplayCommand(ctx, ff, work, args)
	// Older FFmpeg releases cannot load an option value from a file. Keep
	// supporting those installations while respecting Windows' argument limit.
	if err != nil && strings.Contains(err.Error(), "Unrecognized option '/filter_complex'") {
		for i, arg := range args {
			if arg == "-/filter_complex" {
				args[i] = "-filter_complex"
				args[i+1] = graph.String()
				break
			}
		}
		if !replayArgsFit(args) {
			return errReplayBatchTooLarge
		}
		err = runReplayCommand(ctx, ff, work, args)
	}
	if err != nil {
		os.Remove(filepath.Join(work, partial))
		return err
	}
	return os.Rename(filepath.Join(work, partial), filepath.Join(work, name))
}

func replayArgsFit(args []string) bool {
	if runtime.GOOS != "windows" {
		return true
	}
	length := 0
	for _, arg := range args {
		length += len(utf16.Encode([]rune(arg))) + 3 + strings.Count(arg, `\`) + strings.Count(arg, `"`)
	}
	return length < 28000
}
