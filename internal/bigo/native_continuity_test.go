package bigo

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeCodecChangesKeepBothMediaSockets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var connections, refreshes atomic.Int32
	videosSent := make(chan struct{})
	started := make(chan struct{})
	closed := make(chan struct{}, 2)
	listen := func(video bool) mediaProxy {
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
			connections.Add(1)
			defer conn.Close()
			defer func() { closed <- struct{}{} }()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err = readNativePacket(conn); err != nil {
				return
			}
			if video {
				reply := binary.LittleEndian.AppendUint32(nil, 7777)
				reply = binary.LittleEndian.AppendUint64(reply, 0)
				if err = writeNativeTestPacket(conn, 0x5a02, reply); err != nil {
					return
				}
				for i, codec := range []byte{5, 5, 2, 2, 5, 5, 2, 2} {
					if i == 1 {
						close(videosSent)
						select {
						case <-started:
						case <-ctx.Done():
							return
						}
					}
					part := nativeTestVideo(uint32(100+i), i%2 == 0)
					part.codec = codec
					binary.LittleEndian.PutUint32(part.data, uint32(1000+i*40))
					binary.LittleEndian.PutUint32(part.data[4:], uint32(1000+i*40))
					if err = writeNativeTestVideo(conn, part); err != nil {
						return
					}
				}
			} else {
				select {
				case <-videosSent:
				case <-ctx.Done():
					return
				}
				// Keep startup audio flowing until the client has consumed its
				// first keyframe; independent sockets have no receive ordering.
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
			startup:
				for {
					select {
					case <-started:
						break startup
					case <-ctx.Done():
						return
					case <-tick.C:
						body := nativeTestAudioPacket(nativeCaptureAudio(0).data)
						binary.LittleEndian.PutUint32(body[8:], 1010)
						if err = writeNativeTestPacket(conn, 2, body); err != nil {
							return
						}
					}
				}
				for i := 1; i < 8; i++ {
					body := nativeTestAudioPacket(nativeCaptureAudio(0).data)
					binary.LittleEndian.PutUint32(body[8:], uint32(1010+i*40))
					if err = writeNativeTestPacket(conn, 2, body); err != nil {
						return
					}
				}
			}
			io.Copy(io.Discard, conn)
		}()
		return mediaProxy{ip: 0x0100007f, tcp: []uint16{uint16(listener.Addr().(*net.TCPAddr).Port)}}
	}
	source := Stream{Alive: 1, UID: 1234, RoomID: "8589934634", native: &pJoinChannelRes{source: 7777, sid: 42, cookie: []byte("synthetic-ticket"), video: []mediaProxy{listen(true)}, media: []mediaProxy{listen(false)}}}
	c, err := newNativeCapture(ctx, source, StreamRecovery{Refresh: func(context.Context) (Stream, error) {
		refreshes.Add(1)
		return Stream{}, errors.New("unexpected playback refresh")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	close(started)
	for i := range 3 {
		part, err := c.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			t.Fatal(err)
		}
		payloads := nativeCapturePayloads(t, b)
		if len(payloads[256]) != 2 || len(payloads[257]) < 2 || (i > 0 && len(payloads[257]) != 2) {
			t.Fatal("codec transition lost media", i, len(payloads[256]), len(payloads[257]))
		}
	}
	if connections.Load() != 2 || refreshes.Load() != 0 {
		t.Fatal("format change reopened sockets or called the API", connections.Load(), refreshes.Load())
	}
	c.Close()
	for range 2 {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("media socket survived capture shutdown")
		}
	}
}
