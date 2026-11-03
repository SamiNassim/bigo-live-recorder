package bigo

import (
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func TestTimingSurvivesDiskBufferAndVideoAssembly(t *testing.T) {
	q := newNativeFrameQueue(1)
	defer q.close()
	want := nativeMediaFrame{data: []byte{1, 2, 3}, received: 123456789, pts: 4000, dts: 3980, key: true, video: true, codec: 4}
	if err := q.push(want); err != nil {
		t.Fatal(err)
	}
	got, err := q.next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.received != want.received || got.pts != want.pts || got.dts != want.dts {
		t.Fatalf("lost timing: %+v", got)
	}
	data := make([]byte, 12)
	binary.LittleEndian.PutUint32(data, 4000)
	binary.LittleEndian.PutUint32(data[4:], 3980)
	a := &nativeVideoAssembler{}
	frames, err := a.add(nativeVideoPart{frame: 1, key: true, codec: 4, data: data, received: want.received})
	if err != nil || len(frames) != 1 || frames[0].received != want.received {
		t.Fatalf("frames=%+v err=%v", frames, err)
	}
}

func TestGuestPartKeepsReceiptTimeBeforeFileStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	capture := newGuestCapture(ctx, StreamRecovery{})
	defer capture.Close()
	capture.epoch = time.Unix(1900000000, 0)
	for i := range 6 {
		if err := capture.queue.push(nativeMediaFrame{data: []byte{0xfc, 0xff, 0xfe}, codec: 7, pts: 40000000 + uint32(i*20), dts: 40000000 + uint32(i*20), received: int64(time.Second) + int64(i)*int64(20*time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	capture.queue.finish(nil)
	part, err := capture.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if part.Timing == nil || !part.Timing.Epoch.Equal(capture.epoch) {
		t.Fatal("missing capture epoch")
	}
	var samples []MediaTime
	part.Timing.Observe(func(sample MediaTime) { samples = append(samples, sample) })
	if _, err := io.Copy(io.Discard, part); err != nil {
		t.Fatal(err)
	}
	part.Close()
	if len(samples) != 6 || samples[0].Received != time.Second || samples[5].PTS != 9000 {
		t.Fatalf("timing changed during startup: %+v", samples)
	}
}

func TestGuestClockResetKeepsAllAudioAndContinuesWithoutSocketReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	capture := newGuestCapture(ctx, StreamRecovery{})
	defer capture.Close()
	capture.epoch = time.Unix(1900000000, 0)
	for i := range 100 {
		pts := uint32(40000000 + i*20)
		if i >= 50 {
			pts = 900000000 + uint32((i-50)*20)
		}
		capture.queue.push(nativeMediaFrame{data: []byte{0xfc, 0xff, 0xfe}, codec: 7, pts: pts, dts: pts, received: int64(time.Second) + int64(i)*int64(20*time.Millisecond)})
	}
	capture.queue.finish(nil)
	n := 0
	parts := 0
	for {
		part, err := capture.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		parts++
		part.Timing.Observe(func(MediaTime) { n++ })
		if _, err := io.Copy(io.Discard, part); err != nil {
			t.Fatal(err)
		}
		part.Close()
	}
	if parts != 2 || n != 100 {
		t.Fatalf("lost audio or stalled after clock reset: parts=%d frames=%d", parts, n)
	}
}

func TestMuxTimingRecordsBothSourceAndOutputClocks(t *testing.T) {
	var samples []MediaTime
	timing := &CaptureTiming{}
	timing.Observe(func(sample MediaTime) { samples = append(samples, sample) })
	mux := nativeTSMux{out: io.Discard, audioOnly: true, opus: true, timing: timing}
	for i := range 3 {
		if err := mux.writeFrame(nativeMediaFrame{data: []byte{0xfc, 0xff, 0xfe}, codec: 7, pts: 90000000 + uint32(i*20), dts: 90000000 + uint32(i*20), received: int64(1000000000 + i*20000000)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(samples) != 3 || samples[0].SourcePTS != 90000000 || samples[0].PTS != 0 || samples[2].PTS != 3600 || samples[0].Received != 1000000000 {
		t.Fatalf("samples=%+v", samples)
	}
}
