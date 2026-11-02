package bigo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func guestAudio(pts uint32) nativeMediaFrame {
	return nativeMediaFrame{pts: pts, dts: pts, codec: 7, data: []byte{0xfc, 0xff, 0xfe}}
}

func TestGuestCaptureRetainsAudioBeforeFirstCameraKeyframe(t *testing.T) {
	c := nativeCaptureFixture(t, []nativeMediaFrame{guestAudio(1000), guestAudio(1020), nativeCaptureVideo(1040, true, 2, 1), guestAudio(1040), nativeCaptureVideo(1080, false, 2, 2), guestAudio(1060)})
	c.guest = true
	part, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(part)
	part.Close()
	if err != nil || part.AudioOnly {
		t.Fatal("camera capture failed", err)
	}
	got := nativeCapturePayloads(t, data)
	if len(got[256]) != 2 || len(got[257]) != 4 {
		t.Fatal("startup discarded original media", len(got[256]), len(got[257]))
	}
	for _, packet := range got[257] {
		if !bytes.Equal(packet, opusAccessUnit(guestAudio(0).data)) {
			t.Fatal("audio was modified")
		}
	}
}

func TestGuestAudioOnlyUpgradePreservesLateAudioAndCamera(t *testing.T) {
	// The first audio-only part is exposed while the feed stays open. The
	// camera begins later and audio from before its keyframe arrives late.
	c := newGuestCapture(context.Background(), StreamRecovery{})
	defer c.Close()
	if err := c.queue.push(guestAudio(1000)); err != nil {
		t.Fatal(err)
	}
	part, err := c.Next(context.Background())
	if err != nil || !part.AudioOnly {
		t.Fatal("expected audio-only capture", err)
	}
	for _, frame := range []nativeMediaFrame{guestAudio(1020), nativeCaptureVideo(1060, true, 2, 1), guestAudio(1040), guestAudio(1060), nativeCaptureVideo(1100, false, 2, 2), guestAudio(1080)} {
		if err := c.queue.push(frame); err != nil {
			t.Fatal(err)
		}
	}
	c.queue.finish(nil)
	first, err := io.ReadAll(part)
	part.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := nativeCapturePayloads(t, first)
	if len(got[256]) != 0 || len(got[257]) != 3 {
		t.Fatal("audio-only boundary lost late audio", len(got[256]), len(got[257]))
	}
	part, err = c.Next(context.Background())
	if err != nil || part.AudioOnly || part.Reason == "" {
		t.Fatal("camera upgrade failed", err)
	}
	second, err := io.ReadAll(part)
	part.Close()
	if err != nil {
		t.Fatal(err)
	}
	got = nativeCapturePayloads(t, second)
	if len(got[256]) != 2 || len(got[257]) != 2 {
		t.Fatal("camera upgrade lost received media", len(got[256]), len(got[257]))
	}
	if _, err := c.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal("expected clean end", err)
	}
}

func TestGuestCameraGapDoesNotSplitTheFile(t *testing.T) {
	c := nativeCaptureFixture(t, []nativeMediaFrame{nativeCaptureVideo(1000, true, 2, 1), guestAudio(1000), guestAudio(1020), guestAudio(1040), guestAudio(1060), nativeCaptureVideo(1080, true, 2, 2), guestAudio(1080)})
	c.guest = true
	part, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(part)
	part.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := nativeCapturePayloads(t, data)
	if len(got[256]) != 2 || len(got[257]) != 5 {
		t.Fatal("camera gap interrupted original media")
	}
	if _, err := c.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal("unnecessary file split", err)
	}
}

func TestGuestLateStartupAudioPrecedingVideoIsRetained(t *testing.T) {
	c := nativeCaptureFixture(t, []nativeMediaFrame{nativeCaptureVideo(1040, true, 2, 1), guestAudio(1000), guestAudio(1020), guestAudio(1040)})
	c.guest = true
	part, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(part)
	part.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := nativeCapturePayloads(t, data)
	if len(got[256]) != 1 || len(got[257]) != 3 {
		t.Fatal("late startup audio was discarded", len(got[257]))
	}
}

func TestGuestPacketReplayWindowAcceptsLateUniquePackets(t *testing.T) {
	var window guestPacketWindow
	for _, value := range []uint32{100, 102, 101, 0xffffffff, 0} {
		if !window.accept(value) {
			t.Fatal("unique late packet rejected")
		}
	}
	for _, value := range []uint32{101, 102, 0xffffffff, 0} {
		if window.accept(value) {
			t.Fatal("replayed packet accepted")
		}
	}
}

func TestGuestJitterBufferOrdersTracksWithoutWaitingForVideo(t *testing.T) {
	c := newGuestCapture(context.Background(), StreamRecovery{})
	defer c.Close()
	m := guestMember{capture: c, latest: 1300, pending: []nativeMediaFrame{guestAudio(1000), nativeCaptureVideo(1040, true, 2, 1), guestAudio(1100)}}
	if err := m.flush(false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []uint32{1000, 1040, 1100} {
		frame, err := c.queue.next(context.Background())
		if err != nil || frame.pts != want {
			t.Fatal("incorrect jitter order", frame.pts, err)
		}
	}
	if len(m.pending) != 0 {
		t.Fatal("audio-only tail held for video")
	}
}

func TestGuestRoomCanStopDuringPlaybackRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	r := &guestRoom{ctx: ctx, cancel: cancel, recovery: StreamRecovery{Refresh: func(ctx context.Context) (Stream, error) { close(entered); <-ctx.Done(); return Stream{}, ctx.Err() }}}
	refreshed := make(chan struct{})
	go func() { r.refresh(); close(refreshed) }()
	<-entered
	stopped := make(chan struct{})
	go func() { r.finish(nil); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("recovery held the room state lock during a network request")
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("recovery did not honor cancellation")
	}
}
