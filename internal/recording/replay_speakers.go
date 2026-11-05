package recording

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"bigo-live-recorder/internal/timeline"
)

// Each indicator is a short concat of four reusable transparent PNG states.
// Only one bounded render batch is analyzed at a time; no hour-long PCM buffers,
// per-frame images, extra executables or credentials are needed.
func replaySpeakerTrack(ctx context.Context, ff, dir, work string, index, width, height int, clip timeline.Clip, start, end float64) (string, error) {
	from, to := max(start, clip.Start), min(end, clip.End)
	if clip.Video || from >= to {
		return "", nil
	}
	path, err := timeline.LocalPath(dir, clip.File)
	if err != nil {
		return "", err
	}
	speed := (clip.FileEnd - clip.FileStart) / (clip.End - clip.Start)
	if speed < .9 || speed > 1.1 {
		return "", errors.New("a guest's audio clock differs too much to visualize automatically")
	}
	first, last := max(clip.Start, from-.8), min(clip.End, to+.2)
	levels, err := replaySpeech(ctx, ff, path, clip.FileStart+(first-clip.Start)*speed, (last-first)*speed, speed)
	if err != nil {
		return "", err
	}
	frames := int(math.Ceil((end-start)*25 - 1e-8))
	states := make([]uint8, frames)
	anySpeech := false
	for i := range states {
		at := start + float64(i)/25
		if at < from || at >= to {
			continue
		}
		window := int(math.Floor((at-first)*50 + 1e-6))
		for _, w := range []int{window, window + 1} {
			if w >= 0 && w < len(levels) {
				states[i] = max(states[i], levels[w])
			}
		}
		anySpeech = anySpeech || states[i] != 0
	}
	if !anySpeech {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", nil
	}
	var list strings.Builder
	list.WriteString("ffconcat version 1.0\n")
	for i := 0; i < len(states); {
		j := i + 1
		for j < len(states) && states[j] == states[i] {
			j++
		}
		name := fmt.Sprintf("speaker-%dx%d-%d.png", width, height, states[i])
		fmt.Fprintf(&list, "file '%s'\nduration %.9f\n", name, float64(j-i)/25)
		i = j
	}
	// The concat demuxer needs a final frame to preserve the last duration.
	fmt.Fprintf(&list, "file 'speaker-%dx%d-%d.png'\n", width, height, states[len(states)-1])
	name := fmt.Sprintf("speech-%06d.ffconcat", index)
	return name, os.WriteFile(filepath.Join(work, name), []byte(list.String()), 0644)
}

func replaySpeakerImage(path string, width, height int, level uint8) error {
	canvas := image.NewNRGBA(image.Rect(0, 0, width, height))
	if level > 0 {
		// A fine blue perimeter and three restrained meter bars work for
		// both a camera tile and an audio-only guest without hiding faces.
		border := image.NewUniform(color.NRGBA{R: 76, G: 134, B: 255, A: 150 + level*30})
		for _, rect := range []image.Rectangle{
			image.Rect(1, 1, width-1, 3), image.Rect(1, height-3, width-1, height-1),
			image.Rect(1, 3, 3, height-3), image.Rect(width-3, 3, width-1, height-3),
		} {
			draw.Draw(canvas, rect, border, image.Point{}, draw.Src)
		}
		ink := image.NewUniform(color.NRGBA{R: 105, G: 158, B: 255, A: 255})
		for i, scale := range []int{2, 4, 3} {
			h := 4 + int(level)*scale
			x, y := width-36+i*8, height-24
			draw.Draw(canvas, image.Rect(x, y-h/2, x+4, y+(h+1)/2), ink, image.Point{}, draw.Src)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = png.Encode(f, canvas)
	return errors.Join(err, f.Close())
}
