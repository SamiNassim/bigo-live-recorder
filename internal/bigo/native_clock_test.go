package bigo

import (
	"bytes"
	"testing"
	"time"
)

func TestNativeTimestampJumpDoesNotPoisonSharedClock(t *testing.T) {
	for _, video := range []bool{true, false} {
		t.Run(map[bool]string{true: "video", false: "audio"}[video], func(t *testing.T) {
			var output bytes.Buffer
			clock := &nativeTSClock{}
			mux := nativeTSMux{out: &output, clock: clock}
			if err := mux.writeFrame(nativeMediaFrame{video: true, key: true, codec: 2, pts: 4000, dts: 4000}); err != nil {
				t.Fatal(err)
			}
			if err := mux.writeFrame(nativeMediaFrame{pts: 4014, dts: 4014}); err != nil {
				t.Fatal(err)
			}
			size, pts, dts := output.Len(), clock.lastPTS, clock.lastDTS
			if err := mux.writeFrame(nativeMediaFrame{video: video, codec: 2, pts: 24000, dts: 24000}); err == nil {
				t.Fatal("accepted a twenty-second timestamp jump")
			}
			if output.Len() != size || clock.lastPTS != pts || clock.lastDTS != dts {
				t.Fatal("a rejected timestamp changed the recording")
			}
			if err := mux.writeFrame(nativeMediaFrame{video: video, codec: 2, pts: 4040, dts: 4040}); err != nil {
				t.Fatal("valid media remained pinned to the rejected timestamp", err)
			}
		})
	}
}

func TestNativeFirstAudioCannotUseUnrelatedClock(t *testing.T) {
	var output bytes.Buffer
	mux := nativeTSMux{out: &output}
	if err := mux.writeFrame(nativeMediaFrame{video: true, key: true, codec: 2, pts: 4000, dts: 4000}); err != nil {
		t.Fatal(err)
	}
	size := output.Len()
	if err := mux.writeFrame(nativeMediaFrame{pts: 24000}); err == nil || output.Len() != size {
		t.Fatal("a different audio clock reached the output", err)
	}
}

func TestNativeClockResetCannotSilentlyDiscardAllAudio(t *testing.T) {
	var output bytes.Buffer
	mux := nativeTSMux{out: &output}
	if err := mux.writeFrame(nativeMediaFrame{video: true, key: true, codec: 2, pts: 50000, dts: 50000}); err != nil {
		t.Fatal(err)
	}
	if err := mux.writeFrame(nativeMediaFrame{pts: 50014}); err != nil {
		t.Fatal(err)
	}
	for _, video := range []bool{false, true} {
		if err := mux.writeFrame(nativeMediaFrame{video: video, codec: 2, pts: 1, dts: 1}); err == nil {
			t.Fatal("a source clock reset was silently ignored", video)
		}
	}
}

func TestNativeClockAllowsRealGapsAndJitter(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		current, previous int64
		age               time.Duration
		jump              bool
	}{
		{21000 * 90, 1000 * 90, 20 * time.Second, false},
		{6000 * 90, 1000 * 90, time.Millisecond, false},
		{1000 * 90, 1100 * 90, time.Millisecond, false},
		{21000 * 90, 1000 * 90, time.Millisecond, true},
		{1000 * 90, 21000 * 90, time.Millisecond, true},
	} {
		if got := nativeClockJump(tt.current, tt.previous, now, now.Add(-tt.age)); got != tt.jump {
			t.Fatal(tt, got)
		}
	}
}
