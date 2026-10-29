package bigo

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestNativePackedFramingRetainsMediaHeader(t *testing.T) {
	media := []byte{0, 0, 0x8c, 0xc1, 1, 2, 3, 4, 5, 6, 7, 8}
	binary.LittleEndian.PutUint32(media, uint32(0xc18c0000|len(media)<<4|5))
	packet, err := readNativePacket(bytes.NewReader(media))
	if err != nil || packet.uri != 5 || packet.status != 200 || !bytes.Equal(packet.data, media) {
		t.Fatalf("packed packet=%+v error=%v", packet, err)
	}
	for _, size := range []uint32{0, 3, 9, maxNativeFrameBytes + 1} {
		data := binary.LittleEndian.AppendUint32(nil, size)
		if _, err := readNativePacket(bytes.NewReader(data)); err == nil {
			t.Fatalf("accepted length %d", size)
		}
	}
}

func TestNativeVideoAssemblyWaitsForLastFragment(t *testing.T) {
	var assembly nativeVideoAssembler
	data := binary.LittleEndian.AppendUint32(nil, 1234)
	data = binary.LittleEndian.AppendUint32(data, 1234)
	data = append(data, 0, 0, 0, 0, 1, 0x65, 0x88)
	// The small optional count field is not the total for large keyframes.
	for i := range 200 {
		part := nativeVideoPart{frame: 42, index: i, count: 72, split: true, last: i == 199, key: true, codec: 2, data: []byte{0}}
		if i == 0 {
			part.data = data
		}
		frames, err := assembly.add(part)
		if err != nil {
			t.Fatal(err)
		}
		if i < 199 && len(frames) != 0 {
			t.Fatalf("emitted incomplete frame at fragment %d", i)
		}
		if i == 199 && (len(frames) != 1 || frames[0].pts != 1234 || len(frames[0].data) != len(data)-9+199) {
			t.Fatalf("assembled frames=%+v", frames)
		}
	}
	if assembly.bytes != 0 || len(assembly.frames) != 0 {
		t.Fatal("completed frame retained its fragments")
	}
}

func TestNativeAudioPreservesBothAACFrames(t *testing.T) {
	adts := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0x21}
	// AAC LC, 44.1 kHz stereo, eight bytes including its ADTS header.
	b := []byte{2, 0, 0x40, 0xa0}
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 4000)
	b = append(b, 0, 6)
	b = binary.LittleEndian.AppendUint32(b, 0)
	for range 2 {
		b = append(b, 0)
		b = binary.LittleEndian.AppendUint16(b, 0xfc08)
		b = append(b, adts...)
	}
	frames, err := decodeNativeAudio(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].pts != 4000 || frames[1].pts != 4023 {
		t.Fatalf("AAC timestamps=%+v", frames)
	}
	for _, f := range frames {
		if !bytes.Equal(f.data, adts) {
			t.Fatal("AAC source bytes changed")
		}
	}
	b[len(b)-8] = 0
	if _, err := decodeNativeAudio(b); err == nil {
		t.Fatal("accepted invalid AAC header")
	}
}

func TestNativeViewerSubscriptionAndCorrelatedReply(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	client.SetDeadline(time.Now().Add(time.Second))
	server.SetDeadline(time.Now().Add(time.Second))
	c := nativeMediaConn{Conn: client, viewer: 0x10203040, sid: 0x50607080, video: true}
	body := make([]byte, 21)
	binary.LittleEndian.PutUint32(body, c.viewer)
	binary.LittleEndian.PutUint32(body[4:], c.sid)
	binary.LittleEndian.PutUint32(body[16:], 1)
	body[20] = 96
	done := make(chan error, 1)
	go func() { done <- c.control(nativePacket{uri: 0x36b702, status: 200, data: body}) }()
	ack, err := readNativePacket(server)
	if err != nil {
		t.Fatal(err)
	}
	if ack.uri != 0x36b802 || !bytes.Equal(ack.data, body[:16]) {
		t.Fatal("incorrect stream notification acknowledgment")
	}
	sub, err := readNativePacket(server)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if sub.uri != 0x5902 || len(sub.data) != 71 || binary.LittleEndian.Uint32(sub.data[4:]) != 1 {
		t.Fatal("subscription did not select viewer playback")
	}
	r := nativeReader{data: sub.data}
	r.take(16)
	if r.u32() != 0 || r.u32() != 0 || r.u32() != 1 || r.u8() != 0 {
		t.Fatal("viewer playback is not in the third channel list")
	}
	if r.u8() != 0x98 || r.u32() != c.sid || r.u8() != 0 || r.u8() != 0 {
		t.Fatal("Origin mode or room identity changed")
	}
	r.take(32)
	if !bytes.Equal(r.take(3), []byte{5, 3, 0}) || r.err != nil || r.remaining() != 0 {
		t.Fatal("wrong audience role extension")
	}
	reply := make([]byte, 12)
	binary.LittleEndian.PutUint32(reply, c.viewer)
	binary.LittleEndian.PutUint64(reply[4:], c.subscription+1)
	if err = c.control(nativePacket{uri: 0x5a02, status: 200, data: reply}); err != nil || c.confirmed {
		t.Fatal("accepted mismatched subscription response")
	}
	binary.LittleEndian.PutUint64(reply[4:], c.subscription)
	if err = c.control(nativePacket{uri: 0x5a02, status: 200, data: reply}); err != nil || !c.confirmed {
		t.Fatal("subscription was not confirmed")
	}
}

func TestNativeLoginNegotiatesSourceModesWithoutDeviceIdentity(t *testing.T) {
	channel := pJoinChannelRes{source: 0x10203040, sid: 0x50607080, cookie: []byte("fixture-ticket"), timestamp: 123}
	r := nativeReader{data: nativeLoginBody(&channel, 0x33445566, true)}
	if r.u32() != channel.source || r.u32() != channel.sid || r.u32() != 0 || !bytes.Equal(r.blob(), channel.cookie) || r.u32() != 123 {
		t.Fatal("media ticket identity changed")
	}
	if r.u32() != 0x94 || r.u32() != 0x33445566 {
		t.Fatal("login flags or owner changed")
	}
	r.u64()
	if r.u8() != 1 || r.u32() != 0 || r.u32() != 1 || r.u8() != 0 {
		t.Fatal("combined login did not select audience playback")
	}
	r.u8()
	r.blob()
	r.blob()
	r.blob()
	if r.u32() != 10 || r.u32() != 1 || !bytes.Equal(r.take(4), []byte{0x8e, 0, 0, 1}) || r.u32() != 0 || r.u8() != 0x17 {
		t.Fatal("native negotiation capabilities changed")
	}
	if r.u32() != 5 || !bytes.Equal(r.take(5), []byte{0, 1, 2, 3, 4}) {
		t.Fatal("native source modes were not advertised")
	}
	if r.u32() != 2 {
		t.Fatal("login contains unexpected device metadata")
	}
	metadata := make(map[string]string)
	for range 2 {
		key := string(r.blob())
		metadata[key] = string(r.blob())
	}
	if metadata["client_version"] != nativeMediaClientVersion || metadata["os"] != "0" || !bytes.Equal(r.take(2), []byte{3, 0}) || r.err != nil || r.remaining() != 0 {
		t.Fatal("incomplete native protocol identity")
	}
	encoded, err := json.Marshal(Stream{Alive: 1, native: &channel})
	if err != nil || bytes.Contains(encoded, channel.cookie) {
		t.Fatal("media ticket leaked into serialized streamer data")
	}
}

func TestNativeTSRetainsPayloadAndHandlesTimestampWrap(t *testing.T) {
	var output bytes.Buffer
	mux := nativeTSMux{out: &output}
	video := []byte{0, 0, 0, 1, 0x65, 0x88, 0x99}
	if err := mux.writeFrame(nativeMediaFrame{video: true, key: true, codec: 2, pts: 0xfffffff0, dts: 0xfffffff0, data: video}); err != nil {
		t.Fatal(err)
	}
	if err := mux.writeFrame(nativeMediaFrame{video: true, codec: 2, pts: 20, dts: 20, data: video}); err != nil {
		t.Fatal(err)
	}
	b := output.Bytes()
	if len(b)%188 != 0 {
		t.Fatal("partial transport packet")
	}
	var elementary []byte
	for off := 0; off < len(b); off += 188 {
		p := b[off : off+188]
		if p[0] != 0x47 {
			t.Fatal("invalid transport sync")
		}
		pid := uint16(p[1]&31)<<8 | uint16(p[2])
		start := 4
		if p[3]&0x20 != 0 {
			start += 1 + int(p[4])
		}
		if pid == 256 {
			data := p[start:]
			if p[1]&0x40 != 0 {
				data = data[9+int(data[8]):]
			}
			elementary = append(elementary, data...)
		} else if pid == 0 || pid == 4096 {
			section := p[start+1:]
			size := 3 + int(section[1]&15)<<8 + int(section[2])
			crc := uint32(0xffffffff)
			for _, v := range section[:size] {
				crc ^= uint32(v) << 24
				for range 8 {
					if crc&0x80000000 != 0 {
						crc = crc<<1 ^ 0x04c11db7
					} else {
						crc <<= 1
					}
				}
			}
			if crc != 0 {
				t.Fatal("invalid transport program CRC")
			}
		}
	}
	if !bytes.Equal(elementary, append(append([]byte(nil), video...), video...)) {
		t.Fatal("transport mux changed the source bitstream")
	}
	if mux.lastVideoDTS != 36*90 {
		t.Fatalf("wrapped timestamp=%d", mux.lastVideoDTS)
	}
	if err := mux.writeFrame(nativeMediaFrame{video: true, key: true, codec: 5}); err == nil {
		t.Fatal("silently mixed incompatible video codecs")
	}
}

func TestNativeRecoveryClockKeepsTimestampsAndCodec(t *testing.T) {
	clock := &nativeTSClock{}
	first := nativeTSMux{out: io.Discard, clock: clock}
	for _, pts := range []uint32{4000, 4100} {
		if err := first.writeFrame(nativeMediaFrame{video: true, key: true, codec: 5, pts: pts, dts: pts, data: []byte{0, 0, 0, 1, 0x40}}); err != nil {
			t.Fatal(err)
		}
	}
	reconnected := nativeTSMux{out: io.Discard, clock: clock}
	if err := reconnected.writeFrame(nativeMediaFrame{video: true, key: true, codec: 4, pts: 4300, dts: 4300, data: []byte{0, 0, 0, 1, 0x40}}); err != nil {
		t.Fatal(err)
	}
	if clock.lastPTS != 300*90 {
		t.Fatalf("recovery lost the source clock: %d", clock.lastPTS)
	}
	restarted := nativeTSMux{out: io.Discard, clock: clock}
	if err := restarted.writeFrame(nativeMediaFrame{video: true, key: true, codec: 5, pts: 0, dts: 0, data: []byte{0, 0, 0, 1, 0x40}}); err != nil {
		t.Fatal(err)
	}
	if clock.lastPTS != 301*90 {
		t.Fatalf("restarted encoder moved timestamps backwards: %d", clock.lastPTS)
	}
	other := nativeTSMux{out: io.Discard, clock: clock}
	if err := other.writeFrame(nativeMediaFrame{video: true, key: true, codec: 2}); err == nil {
		t.Fatal("recovery changed the codec of an existing MKV")
	}
}

func TestNativeRecoveryKeepsTransportContinuity(t *testing.T) {
	var output bytes.Buffer
	clock := &nativeTSClock{}
	for _, pts := range []uint32{1000, 1100} {
		mux := nativeTSMux{out: &output, clock: clock}
		if err := mux.writeFrame(nativeMediaFrame{video: true, key: true, codec: 5, pts: pts, dts: pts, data: make([]byte, 500)}); err != nil {
			t.Fatal(err)
		}
		if err := mux.writeFrame(nativeMediaFrame{pts: pts, data: []byte{0xff, 0xf1}}); err != nil {
			t.Fatal(err)
		}
	}
	previous := make(map[uint16]byte)
	data := output.Bytes()
	for offset := 0; offset < len(data); offset += 188 {
		packet := data[offset : offset+188]
		pid := uint16(packet[1]&31)<<8 | uint16(packet[2])
		counter := packet[3] & 15
		if last, seen := previous[pid]; seen && counter != (last+1)&15 {
			t.Fatalf("recovery reset transport continuity for PID %d: %d after %d", pid, counter, last)
		}
		previous[pid] = counter
	}
	if len(previous) != 4 {
		t.Fatal("missing audio, video or program tables")
	}
}

func TestNativeRecoverySkipsCachedAudioBeforeNewKeyframe(t *testing.T) {
	clock := &nativeTSClock{}
	first := nativeTSMux{out: io.Discard, clock: clock}
	if err := first.writeFrame(nativeMediaFrame{video: true, key: true, codec: 5, pts: 1000, dts: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := first.writeFrame(nativeMediaFrame{pts: 2000}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	reconnected := nativeTSMux{out: &output, clock: clock}
	if err := reconnected.writeFrame(nativeMediaFrame{video: true, key: true, codec: 5, pts: 1500, dts: 1500}); err != nil {
		t.Fatal(err)
	}
	size := output.Len()
	if err := reconnected.writeFrame(nativeMediaFrame{pts: 1400, data: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if output.Len() != size {
		t.Fatal("replayed audio moved the recording clock backward")
	}
	if err := reconnected.writeFrame(nativeMediaFrame{pts: 1600, data: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	if output.Len() <= size {
		t.Fatal("new audio was discarded after recovery")
	}
}

func TestNativeStartupCancellationClosesBothMediaConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 2)
	closed := make(chan struct{}, 2)
	listen := func() mediaProxy {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if _, err = readNativePacket(conn); err != nil {
				return
			}
			ready <- struct{}{}
			io.Copy(io.Discard, conn)
			closed <- struct{}{}
		}()
		return mediaProxy{ip: 0x0100007f, tcp: []uint16{uint16(listener.Addr().(*net.TCPAddr).Port)}}
	}
	channel := &pJoinChannelRes{source: 123, sid: 42, cookie: []byte("fixture-ticket"), media: []mediaProxy{listen()}, video: []mediaProxy{listen()}}
	done := make(chan error, 1)
	go func() {
		reader, err := openNativeStream(ctx, Stream{native: channel})
		if reader != nil {
			reader.Close()
		}
		done <- err
	}()
	for range 2 {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatal("media login did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("media startup did not honor cancellation")
	}
	for range 2 {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("media connection leaked after cancellation")
		}
	}
}

func FuzzNativeMediaPackets(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{0x45, 0, 0, 0x80})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		_, _ = decodeNativeVideo(data)
		_, _ = decodeNativeAudio(data)
		_, _ = readNativePacket(bytes.NewReader(data))
	})
}
