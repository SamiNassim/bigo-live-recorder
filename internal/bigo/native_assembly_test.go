package bigo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNativeAssemblyWaitsForEarlierKeyframe(t *testing.T) {
	var assembly nativeVideoAssembler
	data := binary.LittleEndian.AppendUint32(nil, 1000)
	data = binary.LittleEndian.AppendUint32(data, 1000)
	data = append(data, 0, 0, 0, 0, 1, 0x26, 1, 2, 3)
	first := nativeVideoPart{frame: 42, split: true, key: true, codec: 5, data: data[:12]}
	if frames, err := assembly.add(first); err != nil || len(frames) != 0 {
		t.Fatal("incomplete keyframe was emitted", frames, err)
	}
	next := nativeVideoPart{frame: 43, codec: 5, data: data}
	if frames, err := assembly.add(next); err != nil || len(frames) != 0 {
		t.Fatal("newer frame overtook the incomplete keyframe", frames, err)
	}
	last := first
	last.index, last.last, last.data = 1, true, data[12:]
	if frames, err := assembly.add(last); err != nil || len(frames) != 2 || !frames[0].key || frames[1].key {
		t.Fatal("late fragment did not restore the keyframe before its dependent frame", frames, err)
	} else if !bytes.Equal(frames[0].data, data[9:]) || !bytes.Equal(frames[1].data, next.data[9:]) {
		t.Fatal("reordering changed compressed video bytes")
	}
}

func nativeTestVideo(id uint32, key bool) nativeVideoPart {
	data := binary.LittleEndian.AppendUint32(nil, id*40)
	data = binary.LittleEndian.AppendUint32(data, id*40)
	data = append(data, 0, 0, 0, 0, 1, 0x26, 1, byte(id))
	return nativeVideoPart{frame: id, key: key, codec: 5, data: data}
}

func splitNativeTestVideo(part nativeVideoPart) (nativeVideoPart, nativeVideoPart) {
	first, last := part, part
	first.split, first.data = true, part.data[:12]
	last.split, last.last, last.index, last.data = true, true, 1, part.data[12:]
	return first, last
}

func TestNativeAssemblyPreservesDecodeOrder(t *testing.T) {
	var assembly nativeVideoAssembler
	first, last := splitNativeTestVideo(nativeTestVideo(42, true))
	next, later := nativeTestVideo(43, false), nativeTestVideo(44, false)
	// B-frame presentation order differs from decode order. Sequence numbers,
	// rather than presentation timestamps or fragment completion, order output.
	binary.LittleEndian.PutUint32(next.data, 1800)
	binary.LittleEndian.PutUint32(later.data, 1760)
	nextHead, nextTail := splitNativeTestVideo(next)
	for _, p := range []nativeVideoPart{first, nextHead, later} {
		if frames, err := assembly.add(p); err != nil || len(frames) != 0 {
			t.Fatal("dependent frame escaped the reassembly buffer", frames, err)
		}
	}
	frames, err := assembly.add(last)
	if err != nil || len(frames) != 1 || !frames[0].key {
		t.Fatal("ready keyframe was not released", frames, err)
	}
	frames, err = assembly.add(nextTail)
	if err != nil || len(frames) != 2 || frames[0].pts != 1800 || frames[1].pts != 1760 {
		t.Fatal("late fragments or B-frame decode order were lost", frames, err)
	}
	if !bytes.Equal(frames[0].data, next.data[9:]) || !bytes.Equal(frames[1].data, later.data[9:]) {
		t.Fatal("late frames changed payloads")
	}
	if len(assembly.frames) != 0 || assembly.bytes != 0 {
		t.Fatal("draining retained buffered frames")
	}
}

func TestNativeAssemblyAllowsProviderSequenceGaps(t *testing.T) {
	var assembly nativeVideoAssembler
	for _, p := range []nativeVideoPart{nativeTestVideo(42, true), nativeTestVideo(900, false), nativeTestVideo(904, false), nativeTestVideo(1800, true)} {
		frames, err := assembly.add(p)
		if err != nil || len(frames) != 1 || frames[0].pts != p.frame*40 {
			t.Fatal("provider sequence gap blocked complete video", frames, err)
		}
	}
	if len(assembly.frames) != 0 || assembly.bytes != 0 {
		t.Fatal("sequence gaps retained video")
	}
}

func TestNativeAssemblyRetainsEachIncompleteFrame(t *testing.T) {
	var assembly nativeVideoAssembler
	first, firstTail := splitNativeTestVideo(nativeTestVideo(42, true))
	second, secondTail := splitNativeTestVideo(nativeTestVideo(43, false))
	third := nativeTestVideo(44, false)
	for _, p := range []nativeVideoPart{first, secondTail, secondTail, third} {
		if frames, err := assembly.add(p); err != nil || len(frames) != 0 {
			t.Fatal("incomplete frames were overtaken", frames, err)
		}
	}
	// A duplicate final fragment must not inflate the buffered-byte accounting.
	wantBytes := len(first.data) + len(secondTail.data) + len(third.data)
	if assembly.bytes != wantBytes {
		t.Fatal("duplicate fragment was counted twice", assembly.bytes, wantBytes)
	}
	frames, err := assembly.add(firstTail)
	if err != nil || len(frames) != 1 || frames[0].pts != 42*40 {
		t.Fatal("drain passed the next incomplete frame", frames, err)
	}
	frames, err = assembly.add(second)
	if err != nil || len(frames) != 2 || frames[0].pts != 43*40 || frames[1].pts != 44*40 {
		t.Fatal("final fragment did not release buffered frames", frames, err)
	}
}

func TestNativeAssemblyStartsAtKeyframeAndIgnoresReplays(t *testing.T) {
	var assembly nativeVideoAssembler
	partial, _ := splitNativeTestVideo(nativeTestVideo(40, false))
	for _, p := range []nativeVideoPart{partial, nativeTestVideo(41, false)} {
		if frames, err := assembly.add(p); err != nil || len(frames) != 0 {
			t.Fatal("startup emitted an undecodable partial GOP", frames, err)
		}
	}
	key := nativeTestVideo(42, true)
	frames, err := assembly.add(key)
	if err != nil || len(frames) != 1 || !frames[0].key {
		t.Fatal("joining mid-frame blocked the first keyframe", frames, err)
	}
	for _, p := range []nativeVideoPart{partial, key, key} {
		if frames, err := assembly.add(p); err != nil || len(frames) != 0 {
			t.Fatal("an older or replayed frame was emitted", frames, err)
		}
	}
	if len(assembly.frames) != 0 || assembly.bytes != 0 {
		t.Fatal("startup or replay fragments retained memory")
	}
	frames, err = assembly.add(nativeTestVideo(43, false))
	if err != nil || len(frames) != 1 {
		t.Fatal("replays blocked subsequent video", frames, err)
	}
}

func TestNativeAssemblySequenceWrap(t *testing.T) {
	var assembly nativeVideoAssembler
	first, last := splitNativeTestVideo(nativeTestVideo(math.MaxUint32-1, true))
	for _, p := range []nativeVideoPart{first, nativeTestVideo(0, false), nativeTestVideo(math.MaxUint32, false)} {
		if frames, err := assembly.add(p); err != nil || len(frames) != 0 {
			t.Fatal("sequence wrap reordered video", frames, err)
		}
	}
	frames, err := assembly.add(last)
	if err != nil || len(frames) != 3 || assembly.last != 0 {
		t.Fatal("wrapped frames did not drain in sequence", frames, err)
	}
	for i, id := range []uint32{math.MaxUint32 - 1, math.MaxUint32, 0} {
		if frames[i].pts != id*40 {
			t.Fatal("sequence wrap changed decode order", frames)
		}
	}
}

func TestNativeAssemblyBoundsPendingFramesAndBytes(t *testing.T) {
	t.Run("frame window", func(t *testing.T) {
		var assembly nativeVideoAssembler
		if _, err := assembly.add(nativeTestVideo(40, true)); err != nil {
			t.Fatal(err)
		}
		first, _ := splitNativeTestVideo(nativeTestVideo(42, true))
		if _, err := assembly.add(first); err != nil {
			t.Fatal(err)
		}
		for i := uint32(1); i < maxNativePendingFrames; i++ {
			if frames, err := assembly.add(nativeTestVideo(42+i, false)); err != nil || len(frames) != 0 {
				t.Fatal("buffer discarded or prematurely emitted a frame", frames, err)
			}
		}
		if _, err := assembly.add(nativeTestVideo(42+maxNativePendingFrames, false)); err == nil || !strings.Contains(err.Error(), "reassembly") {
			t.Fatal("permanently missing fragments did not trigger recovery", err)
		}
		if len(assembly.frames) != maxNativePendingFrames {
			t.Fatal("pending-frame limit was exceeded", len(assembly.frames))
		}
	})
	t.Run("bytes including complete frames", func(t *testing.T) {
		var assembly nativeVideoAssembler
		if _, err := assembly.add(nativeTestVideo(40, true)); err != nil {
			t.Fatal(err)
		}
		first, _ := splitNativeTestVideo(nativeTestVideo(42, true))
		if _, err := assembly.add(first); err != nil {
			t.Fatal(err)
		}
		next := nativeTestVideo(43, false)
		next.data = append(next.data, make([]byte, maxNativeFrameBytes-len(first.data)-len(next.data))...)
		if frames, err := assembly.add(next); err != nil || len(frames) != 0 {
			t.Fatal("complete frame escaped its incomplete predecessor", frames, err)
		}
		if _, err := assembly.add(nativeTestVideo(44, false)); err == nil {
			t.Fatal("complete buffered frames bypassed the byte limit")
		}
		if assembly.bytes != maxNativeFrameBytes {
			t.Fatal("byte limit was exceeded", assembly.bytes)
		}
	})
}

func TestNativeAssemblyStartsAfterJoiningPartialKeyframe(t *testing.T) {
	for _, pending := range []uint32{4, maxNativePendingFrames + 8} {
		var assembly nativeVideoAssembler
		_, tail := splitNativeTestVideo(nativeTestVideo(42, true))
		parts := []nativeVideoPart{tail}
		for i := uint32(1); i <= pending; i++ {
			parts = append(parts, nativeTestVideo(42+i, false))
		}
		for _, part := range parts {
			if frames, err := assembly.add(part); err != nil || len(frames) != 0 {
				t.Fatal("partial startup keyframe caused an error or undecodable output", frames, err)
			}
		}
		frames, err := assembly.add(nativeTestVideo(43+pending, true))
		if err != nil || len(frames) != 1 || !frames[0].key {
			t.Fatal("partial startup frame blocked a complete keyframe", frames, err)
		}
		if len(assembly.frames) != 0 || assembly.bytes != 0 {
			t.Fatal("startup retained an undecodable GOP")
		}
	}
}

func TestNativeAssemblyProtectsActiveKeyframeFromLaterKeyframe(t *testing.T) {
	var assembly nativeVideoAssembler
	if _, err := assembly.add(nativeTestVideo(40, true)); err != nil {
		t.Fatal(err)
	}
	first, last := splitNativeTestVideo(nativeTestVideo(42, true))
	for _, part := range []nativeVideoPart{first, nativeTestVideo(43, false), nativeTestVideo(44, true)} {
		if frames, err := assembly.add(part); err != nil || len(frames) != 0 {
			t.Fatal("active keyframe was overtaken", frames, err)
		}
	}
	frames, err := assembly.add(last)
	if err != nil || len(frames) != 3 || !frames[0].key || frames[0].pts != 42*40 || !frames[2].key {
		t.Fatal("active keyframe was discarded as startup data", frames, err)
	}
}

func TestNativeAssemblyRejectsConflictingFinalIndices(t *testing.T) {
	for _, index := range []int{-1, 4096} {
		var assembly nativeVideoAssembler
		part := nativeTestVideo(42, true)
		part.split, part.index = true, index
		if _, err := assembly.add(part); err == nil {
			t.Fatal("invalid fragment index was accepted", index)
		}
	}
	var assembly nativeVideoAssembler
	_, last := splitNativeTestVideo(nativeTestVideo(42, true))
	if _, err := assembly.add(last); err != nil {
		t.Fatal(err)
	}
	last.index = 2
	if _, err := assembly.add(last); err == nil {
		t.Fatal("conflicting final fragment indices were accepted")
	}
}

func writeNativeTestVideo(w io.Writer, p nativeVideoPart) error {
	kind := byte(2)
	if p.key {
		kind = 1
	}
	body := []byte{0, 0, 0, kind, 0, 0}
	body = binary.LittleEndian.AppendUint32(body, p.frame)
	flags := byte(0)
	if p.split {
		flags |= 16
	}
	if p.last {
		flags |= 32
	}
	if p.owner != 0 {
		flags |= 1
	}
	body = append(body, p.codec, flags)
	if p.owner != 0 {
		body = binary.LittleEndian.AppendUint32(body, p.owner)
	}
	if p.split {
		body = append(body, byte(p.index))
	}
	body = append(body, p.data...)
	packet := binary.LittleEndian.AppendUint32(nil, uint32(len(body)+10))
	packet = binary.LittleEndian.AppendUint32(packet, 5)
	packet = binary.LittleEndian.AppendUint16(packet, 200)
	packet = append(packet, body...)
	_, err := w.Write(packet)
	return err
}

func TestNativeReceiverEmitsInterleavedFramesInOrder(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	client.SetDeadline(time.Now().Add(3 * time.Second))
	server.SetDeadline(time.Now().Add(3 * time.Second))
	conn := nativeMediaConn{Conn: client, video: true, confirmed: true}
	out := make(chan nativeMediaFrame, 3)
	done := make(chan error, 1)
	go func() { done <- conn.receive(context.Background(), out) }()
	first, last := splitNativeTestVideo(nativeTestVideo(42, true))
	for _, p := range []nativeVideoPart{first, nativeTestVideo(43, false), last} {
		if err := writeNativeTestVideo(server, p); err != nil {
			t.Fatal(err)
		}
	}
	server.Close()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receiver did not finish")
	}
	if len(out) != 2 {
		t.Fatal("receiver lost a delayed keyframe", len(out))
	}
	if a, b := <-out, <-out; !a.key || b.key || a.pts != 42*40 || b.pts != 43*40 {
		t.Fatal("receiver changed decode order", a, b)
	}
}
