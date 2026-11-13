package bigo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func nativeCaptureFixture(t *testing.T, frames []nativeMediaFrame) *nativeCapture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &nativeCapture{ctx: ctx, cancel: cancel, queue: newNativeFrameQueue(32), done: make(chan struct{})}
	for _, f := range frames {
		if err := c.queue.push(f); err != nil {
			t.Fatal(err)
		}
	}
	c.queue.finish(nil)
	close(c.done)
	t.Cleanup(func() { c.Close() })
	return c
}

func nativeCaptureVideo(pts uint32, key bool, codec byte, id byte) nativeMediaFrame {
	data := []byte{0, 0, 0, 1, 0x26, 1, id}
	if codec == 0 || codec == 2 || codec == 3 {
		data = []byte{0, 0, 0, 1, 0x65, id}
		if key {
			data = append([]byte{0, 0, 0, 1, 0x67, 1, 0, 0, 1, 0x68, 1}, data...)
		}
	} else if key {
		data = append([]byte{0, 0, 0, 1, 0x40, 1, 0, 0, 1, 0x42, 1, 0, 0, 1, 0x44, 1}, data...)
	}
	return nativeMediaFrame{video: true, key: key, codec: codec, pts: pts, dts: pts, data: data}
}
func nativeCaptureAudio(pts uint32) nativeMediaFrame {
	return nativeMediaFrame{pts: pts, dts: pts, data: []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0x21}}
}

func nativeCapturePayloads(t *testing.T, b []byte) map[uint16][][]byte {
	t.Helper()
	result := make(map[uint16][][]byte)
	pending := make(map[uint16][]byte)
	if len(b)%188 != 0 {
		t.Fatal("partial capture transport packet")
	}
	for off := 0; off < len(b); off += 188 {
		p := b[off : off+188]
		pid := uint16(p[1]&31)<<8 | uint16(p[2])
		if pid != 256 && pid != 257 {
			continue
		}
		start := 4
		if p[3]&0x20 != 0 {
			start += 1 + int(p[4])
		}
		data := p[start:]
		if p[1]&0x40 != 0 {
			if previous := pending[pid]; previous != nil {
				result[pid] = append(result[pid], previous)
			}
			pending[pid] = nil
			if len(data) < 9+int(data[8]) {
				t.Fatal("short capture PES header")
			}
			data = data[9+int(data[8]):]
		}
		pending[pid] = append(pending[pid], data...)
	}
	for pid, data := range pending {
		if data != nil {
			result[pid] = append(result[pid], data)
		}
	}
	return result
}

func TestNativeCaptureCodecRolloverPreservesEveryReceivedFrame(t *testing.T) {
	var frames []nativeMediaFrame
	for i, codec := range []byte{5, 5, 2, 2, 5, 5} {
		pts := uint32(1000 + i*40)
		frames = append(frames, nativeCaptureVideo(pts, i%2 == 0, codec, byte(i)), nativeCaptureAudio(pts+10))
	}
	c := nativeCaptureFixture(t, frames)
	var video, audio [][]byte
	for i := range 3 {
		part, err := c.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !part.Native || (i > 0 && part.Reason == "") {
			t.Fatal("missing rollover metadata")
		}
		b, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			t.Fatal(err)
		}
		payloads := nativeCapturePayloads(t, b)
		if len(payloads[256]) != 2 || len(payloads[257]) != 2 {
			t.Fatal("lost boundary media", i, len(payloads[256]), len(payloads[257]))
		}
		video = append(video, payloads[256]...)
		audio = append(audio, payloads[257]...)
	}
	if _, err := c.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for i := range 6 {
		if !bytes.Equal(video[i], frames[i*2].data) || !bytes.Equal(audio[i], frames[i*2+1].data) {
			t.Fatal("rollover changed source bytes", i)
		}
	}
}

func TestNativeCaptureCompatibleKeyframesKeepOnePart(t *testing.T) {
	var frames []nativeMediaFrame
	for i := range 6 {
		pts := uint32(1000 + i*40)
		frames = append(frames, nativeCaptureVideo(pts, i%2 == 0, 5, byte(i)), nativeCaptureAudio(pts+10))
	}
	c := nativeCaptureFixture(t, frames)
	part, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(part)
	part.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := nativeCapturePayloads(t, b); len(got[256]) != 6 || len(got[257]) != 6 {
		t.Fatal("compatible media was split or lost")
	}
	if _, err := c.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal("same-codec keyframes created another file", err)
	}
}

func TestNativeTimedSplitKeepsLateAudioAndSuppliesHeaders(t *testing.T) {
	first := nativeCaptureVideo(1000, true, 2, 1)
	boundary := nativeCaptureVideo(1120, true, 2, 4)
	boundary.data = []byte{0, 0, 0, 1, 0x65, 4}
	wantKeyframe := []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 1, 0, 0, 0, 1, 0x65, 4}
	c := nativeCaptureFixture(t, []nativeMediaFrame{
		first, nativeCaptureAudio(1010),
		nativeCaptureVideo(1040, true, 2, 2), nativeCaptureAudio(1050),
		nativeCaptureVideo(1080, false, 2, 3), boundary,
		nativeCaptureAudio(1090), nativeCaptureAudio(1120),
		nativeCaptureVideo(1160, false, 2, 5), nativeCaptureAudio(1170),
	})
	c.splitInterval = 70 * time.Millisecond
	for i, want := range []int{3, 2} {
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
		if len(got[256]) != want || len(got[257]) != want {
			t.Fatalf("part %d did not keep every frame: video %d, audio %d", i, len(got[256]), len(got[257]))
		}
		if i == 1 && (part.Reason == "" || !bytes.Equal(got[256][0], wantKeyframe)) {
			t.Fatal("timed split lost the new keyframe or its headers")
		}
	}
	if _, err := c.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestNativeCaptureAssignsLateAudioToTheCorrectFile(t *testing.T) {
	var frames []nativeMediaFrame
	for i, codec := range []byte{5, 5, 2, 2, 5, 5} {
		frames = append(frames, nativeCaptureVideo(uint32(1000+i*40), i%2 == 0, codec, byte(i)))
	}
	for i := range 6 {
		frames = append(frames, nativeCaptureAudio(uint32(1010+i*40)))
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
		payloads := nativeCapturePayloads(t, b)
		if len(payloads[256]) != 2 || len(payloads[257]) != 2 {
			t.Fatal("late audio crossed the file boundary", i, len(payloads[256]), len(payloads[257]))
		}
	}
}

func TestNativeCaptureAudioChangeKeepsAllAudioWithVideoPreroll(t *testing.T) {
	first := nativeCaptureAudio(1010)
	changed := nativeCaptureAudio(1050)
	changed.data = bytes.Clone(changed.data)
	changed.data[2] = 0x4c // AAC LC at 48 kHz.
	frames := []nativeMediaFrame{nativeCaptureVideo(1000, true, 5, 1), first, nativeCaptureVideo(1040, false, 5, 2), changed, nativeCaptureVideo(1080, false, 5, 3)}
	c := nativeCaptureFixture(t, frames)
	var audio [][]byte
	var video int
	for range 2 {
		part, err := c.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			t.Fatal(err)
		}
		payloads := nativeCapturePayloads(t, b)
		audio = append(audio, payloads[257]...)
		video += len(payloads[256])
	}
	if len(audio) != 2 || !bytes.Equal(audio[0], first.data) || !bytes.Equal(audio[1], changed.data) || video != 5 {
		t.Fatal("audio rollover lost media or pre-roll", len(audio), video)
	}
}

func TestNativeCaptureCancelUnblocksUnconsumedPart(t *testing.T) {
	c := nativeCaptureFixture(t, []nativeMediaFrame{nativeCaptureVideo(1000, true, 5, 1), nativeCaptureAudio(1010)})
	part, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	part.Close()
}

func TestNativeCaptureLateAudioChangeRetainsEarlierKeyframe(t *testing.T) {
	changed := nativeCaptureAudio(1050)
	changed.data = bytes.Clone(changed.data)
	changed.data[2] = 0x4c
	later := changed
	later.pts, later.dts = 1090, 1090
	c := nativeCaptureFixture(t, []nativeMediaFrame{
		nativeCaptureVideo(1000, true, 5, 1), nativeCaptureAudio(1010),
		nativeCaptureVideo(1040, false, 5, 2), nativeCaptureVideo(1080, true, 5, 3),
		changed, nativeCaptureVideo(1120, false, 5, 4), later,
	})
	var audio [][]byte
	for range 2 {
		part, err := c.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			t.Fatal(err)
		}
		audio = append(audio, nativeCapturePayloads(t, b)[257]...)
	}
	if len(audio) != 3 || !bytes.Equal(audio[1], changed.data) {
		t.Fatal("late audio configuration change lost samples", len(audio))
	}
}

func TestNativeCaptureLateAudioChangePrecedesVideoCut(t *testing.T) {
	changed := nativeCaptureAudio(1050)
	changed.data = bytes.Clone(changed.data)
	changed.data[2] = 0x4c
	later := changed
	later.pts, later.dts = 1090, 1090
	c := nativeCaptureFixture(t, []nativeMediaFrame{
		nativeCaptureVideo(1000, true, 5, 1), nativeCaptureAudio(1010),
		nativeCaptureVideo(1040, false, 5, 2), nativeCaptureVideo(1080, true, 2, 3),
		changed, later, nativeCaptureVideo(1120, false, 2, 4),
	})
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
		payloads := nativeCapturePayloads(t, b)
		if len(payloads[256]) != 2 || len(payloads[257]) != 1 {
			t.Fatal("overlapping encoding changes lost media", i, len(payloads[256]), len(payloads[257]))
		}
	}
}

func TestNativeCaptureLongPrerollSpillsWithoutDiscardingMedia(t *testing.T) {
	frames := []nativeMediaFrame{nativeCaptureVideo(1000, true, 5, 0), nativeCaptureAudio(1000)}
	for i := 1; i < 400; i++ {
		frames = append(frames, nativeCaptureVideo(uint32(1000+i), false, 5, byte(i)))
	}
	changed := nativeCaptureAudio(1400)
	changed.data = bytes.Clone(changed.data)
	changed.data[2] = 0x4c
	frames = append(frames, changed)
	c := nativeCaptureFixture(t, frames)
	for i := range 2 {
		part, err := c.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			t.Fatal(err)
		}
		payloads := nativeCapturePayloads(t, b)
		if len(payloads[256]) != 400 || len(payloads[257]) != 1 {
			t.Fatal("long pre-roll lost media", i, len(payloads[256]), len(payloads[257]))
		}
	}
}

func TestNativeCaptureSameCodecSocketRecoveryKeepsOnePart(t *testing.T) {
	c := nativeCaptureFixture(t, []nativeMediaFrame{
		nativeCaptureVideo(1000, true, 5, 1), nativeCaptureAudio(1010),
		nativeCaptureVideo(1040, false, 5, 2), nativeCaptureAudio(1050),
		{reset: true},
		nativeCaptureVideo(10, true, 5, 3), nativeCaptureAudio(20),
		nativeCaptureVideo(50, false, 5, 4), nativeCaptureAudio(60),
	})
	part, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(part)
	part.Close()
	if err != nil {
		t.Fatal(err)
	}
	payloads := nativeCapturePayloads(t, b)
	if len(payloads[256]) != 4 || len(payloads[257]) != 4 {
		t.Fatal("compatible socket recovery lost media", len(payloads[256]), len(payloads[257]))
	}
	if _, err = c.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal("compatible socket recovery split the recording", err)
	}
}

func TestNativeCaptureFailureAfterCodecCutKeepsReceivedVideo(t *testing.T) {
	c := nativeCaptureFixture(t, []nativeMediaFrame{
		nativeCaptureVideo(1000, true, 5, 1), nativeCaptureAudio(1010),
		nativeCaptureVideo(1040, true, 2, 2), nativeCaptureVideo(1080, false, 2, 3),
	})
	c.queue.err = io.ErrUnexpectedEOF
	for i := range 2 {
		part, err := c.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(part)
		part.Close()
		if (i == 0 && err != nil) || (i == 1 && !errors.Is(err, io.ErrUnexpectedEOF)) {
			t.Fatal("terminal failure was assigned to the wrong file", i, err)
		}
		payloads := nativeCapturePayloads(t, b)
		if len(payloads[256]) != i+1 || len(payloads[257]) != 1-i {
			t.Fatal("terminal failure discarded received media", i, len(payloads[256]), len(payloads[257]))
		}
	}
}
