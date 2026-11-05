package recording

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"os/exec"
	"strconv"
)

const (
	speechRate    = 16000
	speechSamples = speechRate / 50 // Twenty milliseconds per analysis window.
)

// speechLevels estimates speech activity, not words or speaker identity. Each
// input is already one guest's isolated audio. Energy above an adaptive noise
// floor, a speech-shaped spectrum and a sustained onset reject quiet background
// noise, isolated clicks and steady tones. Hysteresis bridges short word gaps.
// Music and loud speech-like background sounds can still activate the display.
func speechLevels(ctx context.Context, r io.Reader) ([]uint8, error) {
	var detector speechDetector
	var levels []uint8
	var pcm [speechSamples * 2]byte
	var samples [speechSamples]float64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := io.ReadFull(r, pcm[:])
		if n%2 != 0 {
			return nil, errors.New("incomplete decoded audio sample")
		}
		if n > 0 {
			clear(samples[:])
			for i := 0; i < n/2; i++ {
				samples[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) / 32768
			}
			level, onset := detector.next(samples[:])
			levels = append(levels, level)
			if onset {
				// Analysis is offline: the confirmation delay need not make
				// the visible indicator late at the beginning of a sentence.
				for i := max(0, len(levels)-7); i < len(levels)-1; i++ {
					if detector.history[i%len(detector.history)] {
						levels[i] = max(levels[i], 1)
					}
				}
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return levels, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

type speechDetector struct {
	noise, envelope float64
	history         [7]bool
	index, quiet    int
	active          bool
}

func (d *speechDetector) next(samples []float64) (uint8, bool) {
	energy, shaped := speechFeatures(samples)
	if d.noise == 0 {
		d.noise = -65
	}
	threshold := max(-42, d.noise+9)
	candidate := shaped && energy > threshold
	d.history[d.index%len(d.history)] = candidate
	d.index++
	count := 0
	for _, hit := range d.history {
		if hit {
			count++
		}
	}
	onset := !d.active && candidate && count >= 5
	if onset {
		d.active = true
	}
	if candidate {
		d.quiet = 0
	} else {
		d.quiet++
	}
	if d.quiet >= 12 {
		d.active = false
	}
	if !shaped && !d.active {
		// Track non-speech slowly, but recover quickly when the room
		// becomes quieter. Never learn a sustained voice as background.
		rate := .01
		if energy < d.noise {
			rate = .1
		}
		d.noise += rate * (max(-85, energy) - d.noise)
	}
	target := 0.0
	if d.active {
		if candidate {
			target = min(3, 1+(energy-threshold)/10)
		} else {
			target = 1
		}
	}
	if target > d.envelope {
		d.envelope += .6 * (target - d.envelope)
	} else {
		d.envelope += .18 * (target - d.envelope)
	}
	if !d.active && d.envelope < .5 {
		d.envelope = 0
	}
	return uint8(min(3, math.Round(d.envelope))), onset
}

func speechFeatures(samples []float64) (float64, bool) {
	var spectrum [512]complex128
	var mean, energy float64
	for _, s := range samples {
		mean += s
	}
	mean /= float64(len(samples))
	for i, s := range samples {
		v := s - mean
		energy += v * v
		spectrum[i] = complex(v*(.5-.5*math.Cos(2*math.Pi*float64(i)/float64(len(samples)-1))), 0)
	}
	db := 10 * math.Log10(max(1e-12, energy/float64(len(samples))))
	if db < -55 {
		return db, false
	}
	// A small radix-two FFT keeps memory fixed and needs no DSP dependency.
	for i, j := 1, 0; i < len(spectrum); i++ {
		bit := len(spectrum) >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			spectrum[i], spectrum[j] = spectrum[j], spectrum[i]
		}
	}
	for size := 2; size <= len(spectrum); size *= 2 {
		step := cmplx.Rect(1, -2*math.Pi/float64(size))
		for start := 0; start < len(spectrum); start += size {
			rotation := complex(1, 0)
			for i := 0; i < size/2; i++ {
				a, b := spectrum[start+i], rotation*spectrum[start+i+size/2]
				spectrum[start+i], spectrum[start+i+size/2] = a+b, a-b
				rotation *= step
			}
		}
	}
	var total, voice, logPower, peak float64
	bins, peakBin := 0, 0
	for i := 1; i < len(spectrum)/2; i++ {
		power := real(spectrum[i])*real(spectrum[i]) + imag(spectrum[i])*imag(spectrum[i])
		total += power
		frequency := float64(i*speechRate) / float64(len(spectrum))
		if frequency >= 150 && frequency <= 4000 {
			voice += power
			if power > peak {
				peak, peakBin = power, i
			}
			logPower += math.Log(max(1e-15, power))
			bins++
		}
	}
	var tonal float64
	for i := max(1, peakBin-2); i <= min(len(spectrum)/2-1, peakBin+2); i++ {
		tonal += real(spectrum[i])*real(spectrum[i]) + imag(spectrum[i])*imag(spectrum[i])
	}
	flatness := math.Exp(logPower/float64(bins)) / max(1e-15, voice/float64(bins))
	return db, voice > total*.55 && tonal < voice*.88 && flatness < .32
}

// Use the same seek, silence reconstruction and clock correction as the mix.
// A little context on both sides avoids resetting detection at render batches.
func replaySpeech(ctx context.Context, ff, path string, seek, length, speed float64) ([]uint8, error) {
	seconds := func(v float64) string { return strconv.FormatFloat(v, 'f', 9, 64) }
	filter := "atrim=duration=" + seconds(length) + ",aresample=16000:async=1:first_pts=0:min_hard_comp=0.01,atempo=" + seconds(speed) + ",apad,atrim=duration=" + seconds(length/speed)
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1", "-seek_timestamp", "1", "-ss", seconds(max(0, seek)), "-t", seconds(length), "-i", path, "-map", "0:a:0", "-af", filter, "-ac", "1", "-ar", strconv.Itoa(speechRate), "-f", "s16le", "pipe:1")
	configureCommand(cmd)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	levels, readErr := speechLevels(ctx, out)
	if readErr != nil {
		cmd.Process.Kill()
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if readErr != nil {
		return nil, readErr
	}
	if err != nil {
		return nil, fmt.Errorf("could not analyze guest speech activity: %s: %w", stderr.String(), err)
	}
	return levels, nil
}
