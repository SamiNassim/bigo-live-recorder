package bigo

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOpusPacketDurations(t *testing.T) {
	for _, test := range []struct {
		data []byte
		want int
	}{
		{[]byte{0x80}, 120}, {[]byte{0x88}, 240}, {[]byte{0x90}, 480}, {[]byte{0x98}, 960},
		{[]byte{0}, 480}, {[]byte{8}, 960}, {[]byte{16}, 1920}, {[]byte{24}, 2880},
		{[]byte{0x60}, 480}, {[]byte{0x68}, 960}, {[]byte{0xfd, 0, 0}, 1920}, {[]byte{0xff, 3}, 2880},
	} {
		got, err := opusSamples(test.data)
		if err != nil || got != test.want {
			t.Fatal("incorrect Opus duration", test.data, got, err)
		}
	}
	for _, data := range [][]byte{nil, {0xff}, {0xff, 0}, {0xff, 7}} {
		if _, err := opusSamples(data); err == nil {
			t.Fatal("accepted invalid Opus duration", data)
		}
	}
}

func TestGuestOpusRetainsSourcePayloadAndTimestamps(t *testing.T) {
	dtx := []byte{0xfc, 0xff, 0xfe}
	b := []byte{2, 0, 0x40, 0xb8}
	b = binary.LittleEndian.AppendUint32(b, 12)
	b = binary.LittleEndian.AppendUint32(b, 9000)
	b = binary.LittleEndian.AppendUint32(b, 900000001)
	b = binary.LittleEndian.AppendUint32(b, 42)
	b = append(b, 1, 7)
	b = binary.LittleEndian.AppendUint32(b, 20)
	for range 2 {
		b = append(b, 0)
		b = binary.LittleEndian.AppendUint16(b, 0xfc03)
		b = append(b, dtx...)
	}
	packet, err := parseNativeAudio(b)
	if err != nil || packet.owner != 900000001 || packet.sequence != 12 {
		t.Fatal("lost source identity", packet, err)
	}
	frames, err := decodeGuestAudio(packet)
	if err != nil || len(frames) != 2 {
		t.Fatal("Opus framing failed", err)
	}
	for i, frame := range frames {
		if frame.codec != 7 || frame.pts != uint32(9000+20*i) || !bytes.Equal(frame.data, dtx) {
			t.Fatal("Opus bytes or timing changed", frame)
		}
	}
	for n := range len(b) {
		if _, err := parseNativeAudio(b[:n]); err == nil {
			t.Fatal("accepted truncated audio packet", n)
		}
	}
}

func TestOpusAccessUnitLengthBoundaries(t *testing.T) {
	for _, size := range []int{1, 254, 255, 256, 510, 511, 1275} {
		data := bytes.Repeat([]byte{0xa5}, size)
		unit := opusAccessUnit(data)
		if !bytes.Equal(unit[:2], []byte{0x7f, 0xe0}) {
			t.Fatal("missing Opus control header")
		}
		n, off := 0, 2
		for {
			v := unit[off]
			off++
			n += int(v)
			if v != 255 {
				break
			}
		}
		if n != size || !bytes.Equal(unit[off:], data) {
			t.Fatal("Opus access-unit packaging changed the source", size)
		}
	}
}

func TestOpusTransportRemuxPreservesEveryPacket(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	var transport bytes.Buffer
	mux := nativeTSMux{out: &transport, audioOnly: true, opus: true}
	dtx := []byte{0xfc, 0xff, 0xfe}
	for i := range 12 {
		pts := uint32(1000 + 20*i)
		if err := mux.writeFrame(nativeMediaFrame{data: dtx, pts: pts, dts: pts, codec: 7}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	source, output := filepath.Join(dir, "audio.ts"), filepath.Join(dir, "packets.bin")
	if err := os.WriteFile(source, transport.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", source, "-map", "0:a:0", "-c", "copy", "-f", "data", output)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Opus remux: %v: %s", err, out)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, bytes.Repeat(dtx, 12)) {
		t.Fatal("Opus payload changed during remux", err)
	}
}
