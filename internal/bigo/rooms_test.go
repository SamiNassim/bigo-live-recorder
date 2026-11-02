package bigo

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestNativeRoomClassificationRequiresBothExplicitFlags(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags uint32
		want  RoomKind
	}{
		{"normal video", 0, RoomUnknown},
		{"camera-capable multi-guest", 1 << 2, RoomUnknown},
		{"single voice", 1 << 4, RoomUnknown},
		{"audio-only multi-guest", 1<<2 | 1<<4, RoomAudioMultiGuest},
		{"audio-only with other flags", 1<<2 | 1<<4 | 1<<8, RoomAudioMultiGuest},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := nativeResponseFixture("")
			binary.LittleEndian.PutUint32(body[72:76], test.flags)
			room, err := unmarshalPJoinChannelRes(body)
			if err != nil || room.roomKind() != test.want {
				t.Fatalf("kind=%v error=%v", room.roomKind(), err)
			}
		})
	}
	older, err := unmarshalPJoinChannelRes(nativeResponseFixture("")[:60])
	if err != nil || older.roomKind() != RoomUnknown {
		t.Fatal("missing flags were treated as a known room type", err)
	}
}

func TestAudioOnlyRoomNeverOpensMediaEvenWithStalePlayback(t *testing.T) {
	source := Stream{Alive: 1, RoomKind: RoomAudioMultiGuest, HLS: "https://invalid.example/stale.m3u8", native: &pJoinChannelRes{}}
	capture, err := OpenCapture(context.Background(), nil, source, "", StreamRecovery{})
	if capture != nil || !errors.Is(err, ErrAudioOnlyRoom) {
		t.Fatal("attempted recording for an audio-only room", err)
	}
}

func TestNativeCaptureFinalizesWhenRecoveryFindsAnAudioRoom(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := make(chan error, 1)
	failed <- io.ErrUnexpectedEOF
	session := &nativeMediaSession{ctx: ctx, cleanup: func() {}, workers: &sync.WaitGroup{}, failed: failed}
	var messages []string
	capture := &nativeCapture{ctx: ctx, cancel: cancel, queue: newNativeFrameQueue(32), done: make(chan struct{}), session: session,
		recovery: StreamRecovery{
			Refresh: func(context.Context) (Stream, error) { return Stream{Alive: 1, RoomKind: RoomAudioMultiGuest}, nil },
			Report: func(level, message string) {
				if level == "INFO" {
					messages = append(messages, message)
				}
			},
		}}
	capture.receive(Stream{Alive: 1}, session)
	defer capture.Close()
	if _, err := capture.queue.next(ctx); !errors.Is(err, io.EOF) {
		t.Fatal("audio-room transition failed instead of finalizing", err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "audio-only") {
		t.Fatal("audio-room transition was not explained", messages)
	}
}
