package bigo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/fault"
)

func TestNativeRecoveryRefreshesKnownRoomBeforeWeb(t *testing.T) {
	source := Stream{Alive: 1, UID: 77, RoomID: "42"}
	native := source
	native.native = &pJoinChannelRes{}
	web := Stream{Alive: 1, UID: 77, RoomID: "43", HLS: "https://cdn.example/live.m3u8"}
	for _, tt := range []struct {
		name    string
		room    Stream
		err     error
		want    Stream
		wantWeb bool
	}{
		{name: "same room", room: native, want: native},
		{name: "confirmed end", err: ErrRoomEnded},
		{name: "native lookup failed", err: context.DeadlineExceeded, want: web, wantWeb: true},
		{name: "different room", room: Stream{Alive: 1, RoomID: "43", native: &pJoinChannelRes{}}, want: web, wantWeb: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &nativeCapture{recovery: StreamRecovery{
				Room: func(ctx context.Context, current Stream) (Stream, error) {
					if _, ok := ctx.Deadline(); !ok || current.RoomID != source.RoomID {
						t.Fatal("native lookup did not use the existing room and a bounded deadline")
					}
					return tt.room, tt.err
				},
				Refresh: func(context.Context) (Stream, error) {
					if !tt.wantWeb {
						t.Fatal("a resolved native room triggered a web request")
					}
					return web, nil
				},
			}}
			got, err := c.refreshOrigin(context.Background(), source)
			if err != nil || got.RoomID != tt.want.RoomID || got.Alive != tt.want.Alive || got.native != tt.want.native {
				t.Fatal("unexpected recovery source", got.RoomID, err)
			}
		})
	}
}

func TestNativeMediaEOFIdentifiesFailedReceiver(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, video := range []bool{false, true} {
		conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		remote, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		failed := &nativeMediaConn{Conn: conn, video: video, setupStarted: started}
		peer := &nativeMediaConn{video: !video, setupStarted: started}
		peer.lastPlayable.Store(1) // The peer delivered media in the setup clock tick.
		errs := make(chan error, 1)
		go func() {
			errs <- failed.trackFailure(peer, failed.receive(context.Background(), nil))
		}()
		remote.Close()
		select {
		case err = <-errs:
		case <-time.After(time.Second):
			t.Fatal("receiver did not detect the closed socket")
		}
		details := fault.Details(err)
		if errors.Is(err, io.EOF) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s disconnect would change recovery behavior: %v", failed.track(), err)
		}
		if !strings.Contains(details, "Origin "+failed.track()+" receiver stopped") ||
			!strings.Contains(details, failed.track()+" none received") ||
			!strings.Contains(details, "ago") || !strings.Contains(details, "EOF") {
			t.Fatalf("%s disconnect lost track or transport context: %s", failed.track(), details)
		}
	}
}

func TestNativeAudioEOFRecoversWhileRecordingVideo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh bool
	}{
		{name: "existing route"},
		{name: "replacement EOF then fresh route", refresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testNativeAudioRecovery(t, tc.refresh) })
	}
}

func testNativeAudioRecovery(t *testing.T, refresh bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listen := func() (*net.TCPListener, mediaProxy) {
		t.Helper()
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		return listener, mediaProxy{ip: 0x0100007f, tcp: []uint16{uint16(listener.Addr().(*net.TCPAddr).Port)}}
	}
	videoListener, videoRoute := listen()
	audioListener, audioRoute := listen()
	var freshAudio *net.TCPListener
	var freshRoute mediaProxy
	if refresh {
		freshAudio, freshRoute = listen()
	}
	initialVideo, loseAudio, readyAudio, sendAudio, endVideo := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	wait := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		case <-ctx.Done():
			return false
		}
	}
	accept := func(listener *net.TCPListener, ticket string) net.Conn {
		conn, err := listener.AcceptTCP()
		if err != nil {
			return nil
		}
		conn.SetDeadline(time.Now().Add(9 * time.Second))
		login, err := readNativePacket(conn)
		if err != nil || !bytes.Contains(login.data, []byte(ticket)) {
			conn.Close()
			return nil
		}
		return conn
	}
	go func() {
		conn := accept(videoListener, "synthetic-ticket")
		if conn == nil {
			return
		}
		defer conn.Close()
		reply := binary.LittleEndian.AppendUint32(nil, 1234)
		reply = binary.LittleEndian.AppendUint64(reply, 0)
		if writeNativeTestPacket(conn, 0x5a02, reply) != nil || writeNativeTestVideo(conn, nativeTestVideo(25, true)) != nil {
			return
		}
		close(initialVideo)
		if !wait(readyAudio) {
			return
		}
		// Video stays on the original connection, including dependent
		// frames during audio recovery before the next keyframe.
		for id := uint32(26); id <= 29; id++ {
			if writeNativeTestVideo(conn, nativeTestVideo(id, false)) != nil {
				return
			}
		}
		close(sendAudio)
		if wait(endVideo) {
			writeNativeTestVideo(conn, nativeTestVideo(30, true))
		}
	}()
	go func() {
		send := func(conn net.Conn, pts uint32, marker byte) error {
			sample := nativeCaptureAudio(pts).data
			sample[7] = marker
			body := nativeTestAudioPacket(sample)
			binary.LittleEndian.PutUint32(body[8:], pts)
			return writeNativeTestPacket(conn, 2, body)
		}
		first := accept(audioListener, "synthetic-ticket")
		if first == nil {
			return
		}
		if !wait(initialVideo) {
			first.Close()
			return
		}
		ticker := time.NewTicker(10 * time.Millisecond)
	initial:
		for {
			select {
			case <-loseAudio:
				break initial
			case <-ctx.Done():
				ticker.Stop()
				first.Close()
				return
			case <-ticker.C:
				if send(first, 1000, 1) != nil {
					ticker.Stop()
					first.Close()
					return
				}
			}
		}
		ticker.Stop()
		first.Close()
		second := accept(audioListener, "synthetic-ticket")
		if second == nil {
			return
		}
		if refresh {
			second.Close()
			second = accept(freshAudio, "synthetic-fresh-ticket")
			if second == nil {
				return
			}
		}
		defer second.Close()
		close(readyAudio)
		if wait(sendAudio) {
			if send(second, 1000, 2) != nil || send(second, 21000, 3) != nil || send(second, 1160, 4) != nil {
				return
			}
			io.Copy(io.Discard, second)
		}
	}()
	var refreshes, roomLookups atomic.Int32
	resumed := make(chan struct{})
	capture, err := newNativeCapture(ctx, Stream{Alive: 1, UID: 77, RoomID: "42",
		native: &pJoinChannelRes{source: 1234, sid: 42, cookie: []byte("synthetic-ticket"), video: []mediaProxy{videoRoute}, media: []mediaProxy{audioRoute}}}, StreamRecovery{
		Refresh: func(context.Context) (Stream, error) {
			refreshes.Add(1)
			return Stream{}, nil
		},
		Room: func(ctx context.Context, source Stream) (Stream, error) {
			if !refresh || roomLookups.Add(1) != 1 {
				return Stream{}, ErrRoomEnded
			}
			// The refreshed route and login must both come from the listener.
			channel := *source.native
			channel.cookie = []byte("synthetic-fresh-ticket")
			channel.media = []mediaProxy{freshRoute}
			source.native = &channel
			return source, ctx.Err()
		},
		Report: func(_, message string) {
			if message == "Origin audio resumed" {
				close(resumed)
			}
		},
	})
	if err != nil {
		t.Fatal(fault.Details(err))
	}
	defer capture.Close()
	part, err := capture.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()
	close(loseAudio)
	if !wait(resumed) || refreshes.Load() != 0 || (roomLookups.Load() == 1) != refresh {
		t.Fatal("audio recovery did not preserve video on the expected route", refreshes.Load(), roomLookups.Load())
	}
	close(endVideo)
	raw, err := io.ReadAll(part)
	if err != nil {
		t.Fatal(err)
	}
	packets := nativeCapturePayloads(t, raw)
	video, audio := packets[256], packets[257]
	if len(video) != 6 || len(audio) != 2 {
		t.Fatalf("expected six video frames and two aligned audio samples, got %d and %d", len(video), len(audio))
	}
	for i, frame := range video {
		if frame[len(frame)-1] != byte(25+i) {
			t.Fatalf("video frame %d changed", i)
		}
	}
	if audio[0][7] != 1 || audio[1][7] != 4 {
		t.Fatal("cached or out-of-epoch audio reached the recording")
	}
	if _, err := capture.Next(ctx); !errors.Is(err, io.EOF) || refreshes.Load() != 0 {
		t.Fatal("capture did not end normally after the video socket closed", err, refreshes.Load())
	}
}

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
