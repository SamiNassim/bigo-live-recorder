package recording

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"bigo-live-recorder/internal/state"
)

func TestOutputOptionsCannotReplaceInputOrCreateExtraOutput(t *testing.T) {
	for _, raw := range []string{`-i other.mkv -c copy`, `-c copy alternate.mp4`, `ffmpeg -c copy`, `-c copy {output}`, `-filter_complex_script script.txt`, `-vf "movie=other.mp4"`, `-y`, `-c "unfinished`} {
		if _, err := OutputArgs(raw); err == nil {
			t.Fatal("accepted unsafe output options", raw)
		}
	}
	args, err := OutputArgs(`-map 0:v? -map 0:a? -c copy -metadata title="Processed copy" -movflags +faststart`)
	if err != nil || !slices.Contains(args, "title=Processed copy") {
		t.Fatal(args, err)
	}
}

func TestProcessedFormatsKeepOriginalMKVUnchanged(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source := filepath.Join(t.TempDir(), "original recording.mkv")
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "0.5", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", source)
	configureCommand(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic MKV: %v: %s", err, out)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath = ffmpeg
	for _, format := range []string{"mkv", "mp4", "ts"} {
		cfg.FFmpegArgs = "-map 0:v? -map 0:a? -c copy"
		cfg.OutputExtension = format
		if format == "mp4" {
			cfg.FFmpegArgs += " -movflags +faststart"
		}
		processed, err := Process(ctx, source, cfg)
		if err != nil {
			t.Fatal(format, err)
		}
		if processed == source || filepath.Ext(processed) != "."+format {
			t.Fatal("wrong processed output", processed)
		}
		if info, err := os.Stat(processed); err != nil || info.Size() == 0 {
			t.Fatal("processed copy missing", format, err)
		}
		after, err := os.ReadFile(source)
		if err != nil || sha256.Sum256(after) != sha256.Sum256(original) {
			t.Fatal("processing changed the original MKV", err)
		}
	}
}

func TestFailedOrCanceledProcessingNeverRemovesOriginalMKV(t *testing.T) {
	source := filepath.Join(t.TempDir(), "original.mkv")
	original := []byte("original MKV bytes")
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath = filepath.Join(t.TempDir(), "missing")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i, parent := range []context.Context{context.Background(), ctx} {
		if i == 1 {
			path, err := FindFFmpeg("")
			if err != nil {
				continue
			}
			cfg.FFmpegPath = path
		}
		if _, err := Process(parent, source, cfg); err == nil {
			t.Fatal("processing unexpectedly succeeded")
		}
		after, err := os.ReadFile(source)
		if err != nil || sha256.Sum256(after) != sha256.Sum256(original) {
			t.Fatal("processing failure changed the original MKV", err)
		}
	}
}
