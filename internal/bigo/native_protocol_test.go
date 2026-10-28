package bigo

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigolistener"
)

func TestPJoinChannel2WireFormat(t *testing.T) {
	got := marshalPJoinChannel2(0x01020304, 0x10203040, 0xf000002a)
	want, err := hex.DecodeString("04030201403020102a0000f0bb1000000000013c000000000000000a000000")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("channel request=%x, want %x", got, want)
	}
}

func nativeResponseFixture(hls string) []byte {
	b := binary.LittleEndian.AppendUint16(nil, 200)
	for _, v := range []uint32{7, 900000001, 0xf000002a, 900000002} {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	blob := func(s string) { b = binary.LittleEndian.AppendUint16(b, uint16(len(s))); b = append(b, s...) }
	blob("ticket")
	b = binary.LittleEndian.AppendUint32(b, 123)
	b = binary.LittleEndian.AppendUint32(b, 456)
	// One proxy with a numeric IP, two short TCP ports and one short UDP port.
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 0x01020304)
	b = binary.LittleEndian.AppendUint32(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 443)
	b = binary.LittleEndian.AppendUint16(b, 8443)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 3478)
	b = binary.LittleEndian.AppendUint32(b, 0) // video proxy list
	b = binary.LittleEndian.AppendUint64(b, 1) // source timestamp
	b = binary.LittleEndian.AppendUint32(b, 0) // source map
	b = binary.LittleEndian.AppendUint32(b, 0) // flags
	b = binary.LittleEndian.AppendUint16(b, 0) // proxy type
	b = append(b, 0)                           // echo type
	b = binary.LittleEndian.AppendUint32(b, 0) // echo list
	b = binary.LittleEndian.AppendUint64(b, 900000002)
	b = binary.LittleEndian.AppendUint64(b, 0x12345678f000002a)
	b = binary.LittleEndian.AppendUint32(b, 0) // microphone map
	b = binary.LittleEndian.AppendUint32(b, 1) // CDN strings
	blob("cdn.example")
	if hls == "" {
		b = binary.LittleEndian.AppendUint32(b, 0)
	} else {
		b = binary.LittleEndian.AppendUint32(b, 1)
		blob("hls")
		blob(hls)
	}
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint64(b, 3)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return b
}

func TestPJoinChannelResponseParsesNumericProxyAddresses(t *testing.T) {
	const url = "https://cdn.example/source.m3u8"
	got, err := unmarshalPJoinChannelRes(nativeResponseFixture(url))
	if err != nil {
		t.Fatal(err)
	}
	if got.code != 200 || got.sequence != 7 || got.sid != 0xf000002a || got.extra["hls"] != url {
		t.Fatalf("response metadata: %+v", got)
	}
	if len(got.media) != 1 || got.media[0].ip != 0x01020304 || !reflect.DeepEqual(got.media[0].tcp, []uint16{443, 8443}) || !reflect.DeepEqual(got.media[0].udp, []uint16{3478}) {
		t.Fatalf("incorrect proxy metadata: %+v", got.media)
	}
}

func TestNativeChannelResponseRejectsTruncationAndUnboundedCounts(t *testing.T) {
	b := nativeResponseFixture("https://cdn.example/source.m3u8")
	for _, size := range []int{0, 1, 6, 17, 18, 19, 25, 33, 35, len(b) - 1} {
		if _, err := unmarshalPJoinChannelRes(b[:size]); err == nil {
			t.Errorf("accepted partial response at %d bytes", size)
		}
	}
	for _, count := range []uint32{0xffffffff, 4097, 1000} {
		bad := append([]byte(nil), b...)
		binary.LittleEndian.PutUint32(bad[34:38], count)
		if _, err := unmarshalPJoinChannelRes(bad); err == nil {
			t.Errorf("accepted proxy count %d", count)
		}
	}
}

func TestNativeChannelResponseAllowsOlderSuffixVersions(t *testing.T) {
	b := nativeResponseFixture("")
	// The mandatory fields and both proxy lists end at byte 60.
	_, err := unmarshalPJoinChannelRes(b[:60])
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativePlaybackPreservesIdentityAndDoesNotInventQuality(t *testing.T) {
	source := Stream{Alive: 1, UID: 900000002, RoomID: "1311768468895236138", Name: "Demo", Avatar: "https://cdn.example/avatar.png", HLS: "https://cdn.example/web.m3u8", CDN: []string{"https://cdn.example/backup.m3u8"}}
	got, err := nativePlayback(source, "https://cdn.example/native.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != source.UID || got.RoomID != source.RoomID || got.Name != source.Name || got.Avatar != source.Avatar || got.HLS == source.HLS || len(got.CDN) != 0 || got.Quality == "origin" {
		t.Fatalf("native lookup lost identity or claimed an unverified resolution: %+v", got)
	}
	if source.HLS != "https://cdn.example/web.m3u8" || len(source.CDN) != 1 {
		t.Fatal("native lookup mutated the fallback")
	}
	if _, err := nativePlayback(source, ""); !errors.Is(err, ErrOriginUnavailable) {
		t.Fatalf("missing native URL: %v", err)
	}
	for _, url := range []string{"file:///stream.ts", "http://user:password@cdn.example/stream", "bad"} {
		if _, err := nativePlayback(source, url); err == nil {
			t.Error("accepted an invalid native playback URL")
		}
	}
}

func TestNativeSequenceIsSafeForConcurrentRecordings(t *testing.T) {
	var wg sync.WaitGroup
	results := make(chan uint32, 100)
	for range 100 {
		wg.Go(func() { results <- nextPJoinSeq() })
	}
	wg.Wait()
	close(results)
	seen := make(map[uint32]bool)
	for sequence := range results {
		if sequence == 0 || seen[sequence] {
			t.Fatalf("invalid/repeated request sequence %d", sequence)
		}
		seen[sequence] = true
	}
}

func FuzzNativeMediaResponse(f *testing.F) {
	f.Add(nativeResponseFixture("https://cdn.example/live.m3u8"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = unmarshalPJoinChannelRes(data) })
}

func TestNativeReaderBounds(t *testing.T) {
	r := &nativeReader{data: []byte{1}}
	r.u32()
	if !errors.Is(r.err, io.ErrUnexpectedEOF) {
		t.Fatal(r.err)
	}
	if r.take(1) != nil {
		t.Fatal("reader continued after failure")
	}
}

func TestNativeChannelRejectionRetainsProviderCode(t *testing.T) {
	body := binary.LittleEndian.AppendUint16(nil, 403)
	body = binary.LittleEndian.AppendUint32(body, 7)
	response, err := unmarshalPJoinChannelRes(body)
	if err != nil || response.code != 403 || response.sequence != 7 {
		t.Fatalf("rejection: code=%d seq=%d error=%v", response.code, response.sequence, err)
	}
}

func TestNativeLookupWithoutConnectionHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := NewClient().OriginStream(ctx, Stream{Alive: 1, RoomID: "8589934634"}, &bigolistener.Hub{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a shared connection returned %v", err)
	}
	if _, err := NewClient().OriginStream(context.Background(), Stream{}, nil); !errors.Is(err, bigolistener.ErrNoConnection) {
		t.Fatalf("missing listener returned %v", err)
	}
}
