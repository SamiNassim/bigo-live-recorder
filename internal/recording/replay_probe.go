package recording

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"bigo-live-recorder/internal/timeline"
)

// inspectReplayMedia uses FFmpeg's packet checksum muxer, so the managed
// FFmpeg installation is sufficient; a separate ffprobe executable is not needed.
// Encoded packets are demuxed without decoding, and their duration is measured
// from actual packet timestamps rather than a potentially stale duration tag.
func inspectReplayMedia(ctx context.Context, ffmpeg, path string) ([]timeline.TrackInfo, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-copyts", "-i", path, "-map", "0:v?", "-map", "0:a?", "-c", "copy", "-f", "framehash", "-hash", "adler32", "pipe:1")
	configureCommand(cmd)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	tracks, readErr := readPacketTimes(out)
	if readErr != nil {
		cmd.Process.Kill()
	}
	err = cmd.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if err != nil {
		return nil, fmt.Errorf("FFmpeg could not inspect saved media: %s: %w", stderr.String(), err)
	}
	if len(tracks) == 0 {
		return nil, errors.New("the saved file contains no readable audio or video packets")
	}
	return tracks, nil
}

func readPacketTimes(r io.Reader) ([]timeline.TrackInfo, error) {
	type stream struct {
		timeline.TrackInfo
		base   float64
		known  bool
		digest hash.Hash
	}
	streams := make(map[int]*stream)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#tb ") {
			var id, num, den int
			if _, err := fmt.Sscanf(line, "#tb %d: %d/%d", &id, &num, &den); err != nil || den <= 0 || num <= 0 {
				return nil, errors.New("invalid media packet time base")
			}
			streams[id] = &stream{base: float64(num) / float64(den), digest: sha256.New()}
			continue
		}
		if strings.HasPrefix(line, "#media_type ") {
			var id int
			var kind string
			if _, err := fmt.Sscanf(line, "#media_type %d: %s", &id, &kind); err != nil {
				return nil, err
			}
			if s := streams[id]; s != nil {
				s.Video = kind == "video"
				s.known = kind == "audio" || kind == "video"
			}
			continue
		}
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			return nil, errors.New("invalid media packet timing record")
		}
		id, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return nil, err
		}
		s := streams[id]
		if s == nil || !s.known {
			return nil, errors.New("media packet has no declared audio or video clock")
		}
		pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			return nil, err
		}
		duration, err := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		if err != nil || duration < 0 {
			return nil, errors.New("invalid saved packet duration")
		}
		start, end := float64(pts)*s.base, float64(pts+duration)*s.base
		if duration > 0 && (s.PacketDuration == 0 || float64(duration)*s.base < s.PacketDuration) {
			s.PacketDuration = float64(duration) * s.base
		}
		if math.IsInf(start, 0) || math.IsNaN(start) {
			return nil, errors.New("invalid saved media timestamp")
		}
		if s.Packets == 0 {
			s.First = start
		}
		s.First = min(s.First, start)
		s.End = max(s.End, end)
		s.Packets++
		s.digest.Write([]byte(line))
		s.digest.Write([]byte{'\n'})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var result []timeline.TrackInfo
	for _, id := range ids {
		if s := streams[id]; s.known && s.Packets > 0 {
			s.Digest = hex.EncodeToString(s.digest.Sum(nil))
			result = append(result, s.TrackInfo)
		}
	}
	return result, nil
}
