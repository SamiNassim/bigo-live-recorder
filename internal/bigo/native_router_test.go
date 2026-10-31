package bigo

import (
	"encoding/binary"
	"testing"
)

func TestNativeRoutingRejectsAnotherOwnerBeforeReassembly(t *testing.T) {
	r := nativeVideoRouter{owner: 1234}
	first := nativeTestVideo(100, true)
	first.owner = 1234
	first.mode = 16
	other := first
	other.owner = 5678
	other.codec = 2
	if frames, err := r.add(other); err != nil || len(frames) != 0 {
		t.Fatal("another owner entered the primary stream", err)
	}
	frames, err := r.add(first)
	if err != nil || len(frames) != 1 || frames[0].codec != 5 {
		t.Fatal("primary source was changed", frames, err)
	}
}

func TestNativeRoutingSeparatesOpaqueSubstreamFragments(t *testing.T) {
	r := nativeVideoRouter{owner: 1234}
	first, last := splitNativeTestVideo(nativeTestVideo(100, true))
	first.owner, last.owner = 1234, 1234
	first.mode, last.mode = 16, 16
	if frames, err := r.add(first); err != nil || len(frames) != 0 {
		t.Fatal(err)
	}
	other := nativeTestVideo(100, true)
	other.owner = 1234
	other.mode = 17
	other.codec = 2
	frames, err := r.add(other)
	if err != nil || len(frames) != 1 {
		t.Fatal("independent substream collided with fragments", err)
	}
	frames, err = r.add(last)
	if err != nil || len(frames) != 0 {
		t.Fatal("equal-time retired substream overtook the active source", err)
	}
	next := nativeTestVideo(101, true)
	next.owner = 1234
	next.mode = 16
	frames, err = r.add(next)
	if err != nil || len(frames) != 1 || frames[0].codec != 5 {
		t.Fatal("newer primary keyframe did not restore the source", err)
	}
}

func TestNativeVideoDecodesSubstreamIdentity(t *testing.T) {
	b := []byte{0, 0, 0, 1, 0, 0}
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = append(b, 5|32, 0, 2, 16, 1, 2, 3)
	p, err := decodeNativeVideo(b)
	if err != nil || p.mode != 16 {
		t.Fatal("native substream identity was lost", p.mode, err)
	}
}
