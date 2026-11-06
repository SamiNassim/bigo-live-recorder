package bigo

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGuestVideoClockRolloverKeepsDecodableHeaders(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	for _, codec := range []struct {
		name, encoder, remove string
		native                byte
	}{
		{"h264", "libx264", "7|8", 2},
		{"hevc", "libx265", "32|33|34", 5},
	} {
		t.Run(codec.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			args := []string{"-v", "error", "-f", "lavfi", "-i", "color=c=black:s=64x96:r=25", "-frames:v", "1", "-c:v", codec.encoder, "-preset", "ultrafast", "-threads", "1"}
			if codec.name == "hevc" {
				args = append(args, "-x265-params", "log-level=error:pools=1:frame-threads=1:repeat-headers=1")
			}
			args = append(args, "-f", codec.name, "pipe:1")
			encoded, err := exec.CommandContext(ctx, ffmpeg, args...).Output()
			if err != nil {
				t.Fatal("generate video", err)
			}
			strip := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", codec.name, "-i", "pipe:0", "-c:v", "copy", "-bsf:v", "filter_units=remove_types="+codec.remove, "-f", codec.name, "pipe:1")
			strip.Stdin = bytes.NewReader(encoded)
			headerless, err := strip.Output()
			if err != nil || len(headerless) == 0 || len(headerless) >= len(encoded) {
				t.Fatal("strip repeated headers", err)
			}
			c := nativeCaptureFixture(t, []nativeMediaFrame{
				{video: true, key: true, codec: codec.native, pts: 1000, dts: 1000, data: encoded},
				{video: true, key: true, codec: codec.native, pts: 40000, dts: 40000, data: headerless},
			})
			c.guest = true
			var firstDecoded []byte
			for i := range 2 {
				part, err := c.Next(ctx)
				if err != nil {
					t.Fatal("open guest file", i, err)
				}
				data, err := io.ReadAll(part)
				part.Close()
				if err != nil {
					t.Fatal("read guest file", i, err)
				}
				path := filepath.Join(t.TempDir(), "guest.mkv")
				mux := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-copyts", "-start_at_zero", "-i", "pipe:0", "-map", "0:v:0", "-c:v", "copy", "-f", "matroska", path)
				mux.Stdin = bytes.NewReader(data)
				if out, err := mux.CombinedOutput(); err != nil {
					t.Fatalf("finalize guest file %d: %v: %s", i, err, out)
				}
				decoded, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", path, "-f", "hash", "-hash", "sha256", "pipe:1").Output()
				if err != nil || len(decoded) == 0 {
					t.Fatal("decode guest file", i, err)
				}
				if i == 0 {
					firstDecoded = decoded
				} else if !bytes.Equal(firstDecoded, decoded) {
					t.Fatal("rollover changed the decoded video")
				}
			}
		})
	}
}
