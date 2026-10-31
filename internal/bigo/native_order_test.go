package bigo

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"strconv"
	"testing"
)

func TestNativeOrderingKeepsAudioInItsEncodingPhase(t *testing.T) {
	for seed := range uint64(50) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			var order nativeMediaOrder
			defer order.close()
			var frames []nativeMediaFrame
			emit := func(f nativeMediaFrame) error { frames = append(frames, f); return nil }
			codecs := []byte{5, 5, 2, 2, 5, 5}
			random := rand.New(rand.NewPCG(seed, seed+1))
			video, audio := 0, 0
			for video < 6 || audio < 6 {
				var f nativeMediaFrame
				if audio == 6 || (video < 6 && (video == 0 || random.IntN(2) == 0)) {
					f = nativeCaptureVideo(uint32(1000+video*40), video%2 == 0, codecs[video], byte(video))
					video++
				} else {
					f = nativeCaptureAudio(uint32(1010 + audio*40))
					audio++
				}
				if err := order.add(f, emit); err != nil {
					t.Fatal(err)
				}
			}
			if err := order.flush(nil, emit); err != nil {
				t.Fatal(err)
			}
			c := nativeCaptureFixture(t, frames)
			for i := range 3 {
				part, err := c.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(part)
				part.Close()
				if err != nil {
					t.Fatal(err)
				}
				got := nativeCapturePayloads(t, b)
				if len(got[256]) != 2 || len(got[257]) != 2 {
					t.Fatal("arrival order changed encoding boundaries", i, len(got[256]), len(got[257]))
				}
			}
			if _, err := c.Next(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeOrderingRejectsUnrelatedAudioClock(t *testing.T) {
	var order nativeMediaOrder
	defer order.close()
	var frames []nativeMediaFrame
	emit := func(f nativeMediaFrame) error { frames = append(frames, f); return nil }
	if err := order.add(nativeCaptureVideo(1000, true, 5, 1), emit); err != nil {
		t.Fatal(err)
	}
	if err := order.add(nativeCaptureAudio(21000), emit); !errors.Is(err, errNativeMediaOrderClock) {
		t.Fatal("a future timestamp accumulated in the audio buffer", err)
	}
	if err := order.flush(nil, emit); err != nil || len(frames) != 1 {
		t.Fatal("unrelated audio was written", err, len(frames))
	}
}
