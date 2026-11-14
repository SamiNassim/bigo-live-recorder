package bigo

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
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
	if err := order.add(nativeCaptureAudio(21000), emit); err != nil || order.audioJump.IsZero() || order.count != 0 {
		t.Fatal("a future timestamp was not isolated", err)
	}
	order.audioJump = time.Now().Add(-nativeClockRecoveryWindow)
	if err := order.add(nativeCaptureVideo(1040, false, 5, 2), emit); !errors.Is(err, errNativeMediaOrderClock) {
		t.Fatal("an unresolved jump did not escalate", err)
	}
	if err := order.flush(nil, emit); err != nil || len(frames) != 1 {
		t.Fatal("unrelated audio was written", err, len(frames))
	}
}

func TestNativeOrderingKeepsVideoThroughTransientAudioJump(t *testing.T) {
	var order nativeMediaOrder
	defer order.close()
	var frames []nativeMediaFrame
	emit := func(f nativeMediaFrame) error { frames = append(frames, f); return nil }
	for _, f := range []nativeMediaFrame{
		nativeCaptureVideo(1000, true, 5, 1), nativeCaptureAudio(1010),
		nativeCaptureAudio(21000), nativeCaptureVideo(1040, false, 5, 2),
		nativeCaptureAudio(1010), nativeCaptureAudio(1050),
		nativeCaptureVideo(1080, false, 5, 3),
	} {
		if err := order.add(f, emit); err != nil {
			t.Fatal(err)
		}
	}
	if !order.audioJump.IsZero() || len(frames) != 5 {
		t.Fatal("rejected audio affected healthy video or valid audio", order.audioJump, len(frames))
	}
	for i, pts := range []uint32{1000, 1010, 1040, 1050, 1080} {
		if frames[i].pts != pts {
			t.Fatal("wrong media order", i, frames[i].pts, pts)
		}
	}
}

func TestNativeOrderingUsesSocketArrivalForPacedVideo(t *testing.T) {
	var order nativeMediaOrder
	defer order.close()
	first, resumed := nativeCaptureVideo(1000, true, 5, 1), nativeCaptureVideo(16000, true, 5, 2)
	first.received, resumed.received = 1, 1+int64(15*time.Second)
	count := 0
	for _, f := range []nativeMediaFrame{first, resumed} {
		if err := order.add(f, func(nativeMediaFrame) error { count++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if count != 2 || !order.videoJump.IsZero() {
		t.Fatal("buffered video was misclassified as a clock jump")
	}
}

func TestNativeOrderingWaitsForKeyframeAfterInvalidVideoPTS(t *testing.T) {
	var order nativeMediaOrder
	defer order.close()
	bad := nativeCaptureVideo(21000, false, 5, 2)
	bad.dts = 1040
	var frames []nativeMediaFrame
	emit := func(f nativeMediaFrame) error { frames = append(frames, f); return nil }
	for _, f := range []nativeMediaFrame{
		nativeCaptureVideo(1000, true, 5, 1), bad, nativeCaptureVideo(1080, false, 5, 3),
		nativeCaptureAudio(1010), nativeCaptureVideo(1120, true, 5, 4),
	} {
		if err := order.add(f, emit); err != nil {
			t.Fatal(err)
		}
	}
	if !order.videoJump.IsZero() || len(frames) != 3 || frames[1].video || frames[2].pts != 1120 {
		t.Fatal("video recovery emitted an undecodable frame or lost buffered audio", frames)
	}
}
