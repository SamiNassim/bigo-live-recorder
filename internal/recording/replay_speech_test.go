package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// Synthetic harmonic syllables exercise timing and noise rejection without
// embedding a person's voice or depending on a speech synthesis installation.
func speechFixture(at float64) float64 {
	var value float64
	for i, amplitude := range []float64{.2, .5, 1, .7, .4, .2, .3, .1, .06} {
		value += amplitude * math.Sin(2*math.Pi*173*float64(i+1)*at)
	}
	return value * (.65 + .35*math.Sin(2*math.Pi*4*at)) * .05
}

func speechPCM(seconds float64, sample func(float64) float64) []byte {
	data := make([]byte, int(seconds*speechRate)*2)
	for i := 0; i < len(data)/2; i++ {
		v := min(.999, max(-1, sample(float64(i)/speechRate)))
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(v*32767)))
	}
	return data
}

func TestSpeechActivityRejectsBackgroundNoiseAndShortClicks(t *testing.T) {
	random := rand.New(rand.NewPCG(41, 83))
	for name, sample := range map[string]func(float64) float64{
		"silence":                     func(float64) float64 { return 0 },
		"quiet voice-like background": func(at float64) float64 { return speechFixture(at) * .015 },
		"white noise":                 func(float64) float64 { return (random.Float64() - .5) * .25 },
		"steady tone":                 func(at float64) float64 { return .2 * math.Sin(2*math.Pi*440*at) },
		"low rumble":                  func(at float64) float64 { return .2 * math.Sin(2*math.Pi*65*at) },
		"brief clicks": func(at float64) float64 {
			if math.Mod(at, .6) < .04 {
				return speechFixture(at) * 3
			}
			return 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			levels, err := speechLevels(context.Background(), bytes.NewReader(speechPCM(3, sample)))
			if err != nil || slices.ContainsFunc(levels, func(v uint8) bool { return v != 0 }) {
				t.Fatalf("non-speech activated the indicator: %v %v", err, levels)
			}
		})
	}
}

func TestSpeechActivityHasPromptOnsetAndSmoothRelease(t *testing.T) {
	data := speechPCM(3, func(at float64) float64 {
		if at < .5 || at >= 2 || at >= 1 && at < 1.1 {
			return 0
		}
		return speechFixture(at)
	})
	levels, err := speechLevels(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	first := slices.IndexFunc(levels, func(v uint8) bool { return v != 0 })
	if first < 24 || first > 28 {
		t.Fatal("speech onset is early or late", first)
	}
	for _, window := range []int{30, 50, 54, 75, 95} {
		if levels[window] == 0 {
			t.Fatal("speech or a short word gap lost its indicator", window)
		}
	}
	if slices.ContainsFunc(levels[120:], func(v uint8) bool { return v != 0 }) {
		t.Fatal("indicator did not fade after speech ended")
	}
}

func TestSpeechActivityHonorsCancellationAndReadErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := speechLevels(ctx, bytes.NewReader(nil)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was ignored", err)
	}
	if _, err := speechLevels(context.Background(), bytes.NewReader([]byte{1})); err == nil {
		t.Fatal("accepted an incomplete PCM sample")
	}
}
