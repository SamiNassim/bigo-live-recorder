package bigo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGuestStartupClockSplitFinalizesEveryReceivedTrack(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	for _, codec := range []struct {
		name, encoder string
		native        byte
	}{{"h264", "libx264", 2}, {"hevc", "libx265", 5}} {
		t.Run(codec.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
			video := func(pts uint32) nativeMediaFrame {
				return nativeMediaFrame{video: true, key: true, codec: codec.native, pts: pts, dts: pts, data: encoded}
			}
			for _, audioFirst := range []bool{true, false} {
				t.Run(fmt.Sprint("audio_first=", audioFirst), func(t *testing.T) {
					frames := []nativeMediaFrame{video(1000), guestAudio(30000), video(30040), guestAudio(30040)}
					if audioFirst {
						frames = []nativeMediaFrame{guestAudio(30000), guestAudio(30020), video(1000), guestAudio(30040), video(30080), guestAudio(30080)}
					}
					for i := range frames {
						frames[i].received = int64(time.Second + time.Duration(i)*20*time.Millisecond)
					}
					c := nativeCaptureFixture(t, frames)
					c.guest = true
					c.epoch = time.Unix(1900000000, 0)
					var videos, audios int
					for i := 0; ; i++ {
						part, err := c.Next(ctx)
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil || i > len(frames) {
							t.Fatal("startup did not reach a stable capture", err)
						}
						var samples []MediaTime
						part.Timing.Observe(func(sample MediaTime) { samples = append(samples, sample) })
						data, err := io.ReadAll(part)
						part.Close()
						<-c.partDone
						if err != nil {
							t.Fatal("read startup part", err)
						}
						path := filepath.Join(t.TempDir(), "guest.mkv")
						mux := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-copyts", "-start_at_zero", "-i", "pipe:0", "-map", "0:v?", "-map", "0:a?", "-c", "copy", "-f", "matroska", path)
						mux.Stdin = bytes.NewReader(data)
						if out, err := mux.CombinedOutput(); err != nil {
							t.Fatalf("finalize startup part %d: %v: %s", i, err, out)
						}
						got := nativeCapturePayloads(t, data)
						if part.AudioOnly != (len(got[256]) == 0) {
							t.Fatal("file type did not match its actual tracks")
						}
						if len(samples) != len(got[256])+len(got[257]) {
							t.Fatal("startup lost or duplicated replay timing samples")
						}
						for _, payload := range got[256] {
							if !bytes.Equal(payload, encoded) {
								t.Fatal("startup changed the original encoded video")
							}
						}
						for _, payload := range got[257] {
							if !bytes.Equal(payload, opusAccessUnit(guestAudio(0).data)) {
								t.Fatal("startup changed the original encoded audio")
							}
						}
						videos += len(got[256])
						audios += len(got[257])
					}
					wantAudio := len(frames) - 2
					if videos != 2 || audios != wantAudio {
						t.Fatal("startup clock split discarded received media", videos, audios)
					}
				})
			}
		})
	}
}

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
