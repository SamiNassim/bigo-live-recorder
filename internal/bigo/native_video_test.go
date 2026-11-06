package bigo

import (
	"bytes"
	"testing"
)

func TestNativeVideoHeadersDoNotCrossParticipantsOrCodecs(t *testing.T) {
	first := nativeVideoHeaders{}
	full := nativeCaptureVideo(1000, true, 2, 1)
	if got, ready := first.prepare(full); !ready || !bytes.Equal(got.data, full.data) {
		t.Fatal("complete keyframe was changed or rejected")
	}
	bare := nativeMediaFrame{video: true, key: true, codec: 3, data: []byte{0, 0, 1, 0x65, 1}}
	if got, ready := first.prepare(bare); !ready || bytes.Equal(got.data, bare.data) {
		t.Fatal("compatible H.264 keyframe did not inherit its headers")
	}
	other := nativeVideoHeaders{}
	if _, ready := other.prepare(bare); ready {
		t.Fatal("a participant borrowed another participant's headers")
	}
	bare.codec, bare.data = 5, []byte{0, 0, 1, 0x26, 1, 1}
	if _, ready := first.prepare(bare); ready {
		t.Fatal("HEVC inherited H.264 headers")
	}
	bare.codec, bare.data = 2, []byte{0, 0, 1, 0x65, 1}
	if _, ready := first.prepare(bare); ready {
		t.Fatal("codec change restored stale headers")
	}
}
