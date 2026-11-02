package bigo

import (
	"bytes"
	"crypto/rc4"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestGuestDatagramsSurviveSmallPacketReaderCalls(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.Dial("udp", server.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = client.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var hello [1]byte
	_, address, err := server.ReadFrom(hello[:])
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0xa5}, 512)
	packet := binary.LittleEndian.AppendUint32(nil, uint32(10+len(body)))
	packet = binary.LittleEndian.AppendUint32(packet, 0x36b502)
	packet = binary.LittleEndian.AppendUint16(packet, 200)
	packet = append(packet, body...)
	stream := &nativeDatagramConn{Conn: client}
	for _, encrypted := range []bool{false, true} {
		payload := bytes.Clone(packet)
		if encrypted {
			key := []byte{1, 2, 3, 4}
			cipher, _ := rc4.NewCipher(key)
			cipher.XORKeyStream(payload, payload)
			payload = append(key, payload...)
		}
		if _, err := server.WriteTo([]byte{0, 0}, address); err != nil {
			t.Fatal(err)
		}
		if _, err := server.WriteTo(payload, address); err != nil {
			t.Fatal(err)
		}
		got, err := readNativePacket(stream)
		if err != nil || got.uri != 0x36b502 || !bytes.Equal(got.data, body) {
			t.Fatal("datagram was truncated or decoded incorrectly", err)
		}
	}
}

func TestGuestAudioSubscriptionRequiresMatchingUnmixedAcknowledgement(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	server.SetDeadline(time.Now().Add(3 * time.Second))
	c := &nativeMediaConn{Conn: client, viewer: 900000001, sid: 42, guest: true}
	done := make(chan error, 1)
	go func() { done <- c.subscribeGuestAudio() }()
	packet, err := readNativePacket(server)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	r := nativeReader{data: packet.data}
	if packet.uri != 0x5902 || r.u32() != c.viewer || r.u32() != 2 {
		t.Fatal("wrong guest subscription")
	}
	sequence := r.u64()
	for _, want := range [][]byte{nil, nil, {2}} {
		n := int(r.u32())
		if !bytes.Equal(r.take(n), want) {
			t.Fatal("wrong guest channel")
		}
	}
	if r.u32() != c.sid || r.u64() != uint64(c.viewer) || r.u64() != uint64(c.sid) || r.u8() != 4 || r.err != nil {
		t.Fatal("guest subscription enabled mixing or lost identity")
	}
	ack := binary.LittleEndian.AppendUint32(nil, c.viewer)
	ack = binary.LittleEndian.AppendUint64(ack, sequence-1)
	if err = c.control(nativePacket{uri: 0x5a02, status: 200, data: ack}); err != nil || c.confirmed {
		t.Fatal("stale subscription acknowledged")
	}
	binary.LittleEndian.PutUint64(ack[4:], sequence)
	if err = c.control(nativePacket{uri: 0x5a02, status: 200, data: ack}); err != nil || !c.confirmed {
		t.Fatal("matching subscription was not acknowledged")
	}
}

func TestGuestReplayFilterRetainsDistinctPayloadsAndTracks(t *testing.T) {
	var window guestFrameWindow
	a := guestAudio(1000)
	if !window.accept(a) || window.accept(a) {
		t.Fatal("audio retransmission handling failed")
	}
	b := a
	b.data = []byte{0xfc, 0xff, 0xfd}
	if !window.accept(b) || !window.accept(nativeCaptureVideo(1000, true, 2, 1)) {
		t.Fatal("different media was discarded")
	}
	for i := range 1024 {
		if !window.accept(guestAudio(uint32(1020 + i*20))) {
			t.Fatal("new audio was discarded")
		}
	}
	if len(window.seen) > 512 {
		t.Fatal("replay state grows without bound")
	}
}

var _ io.Reader = (*nativeDatagramConn)(nil)
