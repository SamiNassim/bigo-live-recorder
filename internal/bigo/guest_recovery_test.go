package bigo

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGuestRoomEndRequiresNativeConfirmation(t *testing.T) {
	for _, test := range []struct {
		name              string
		web               Stream
		webErr, nativeErr error
		ended             bool
	}{
		{name: "web offline but native room exists", web: Stream{}},
		{name: "new room lookup failed while previous room exists", webErr: ErrRoomEnded},
		{name: "native room disappeared", web: Stream{}, nativeErr: ErrRoomEnded, ended: true},
		{name: "native timeout is not an end", web: Stream{}, nativeErr: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := Stream{Alive: 1, UID: 900000001, RoomID: "8589934634", RoomKind: RoomHybridMultiGuest, native: &pJoinChannelRes{}}
			calls := 0
			r := &guestRoom{ctx: ctx, source: source, recovery: StreamRecovery{Refresh: func(context.Context) (Stream, error) { return test.web, test.webErr }, Room: func(_ context.Context, requested Stream) (Stream, error) {
				calls++
				if requested.RoomID != source.RoomID {
					t.Error("verified the wrong broadcast")
				}
				return source, test.nativeErr
			}}}
			fresh, err := r.refresh()
			if errors.Is(err, ErrRoomEnded) != test.ended || calls != 1 {
				t.Fatal("incorrect end confirmation", fresh, err, calls)
			}
			if test.nativeErr == nil && (err != nil || fresh.RoomID != source.RoomID) {
				t.Fatal("discarded a native room still in progress", err)
			}
		})
	}
}

func TestGuestTrackRecoveryDoesNotPollWhileOtherMediaAdvances(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := Stream{Alive: 1, UID: 900000001, RoomID: "8589934634", RoomKind: RoomHybridMultiGuest, native: &pJoinChannelRes{}}
	queries := 0
	r := &guestRoom{ctx: ctx, source: source, recovery: StreamRecovery{Refresh: func(context.Context) (Stream, error) {
		t.Error("polled playback during healthy reception")
		return source, nil
	}, Room: func(context.Context, Stream) (Stream, error) { queries++; return source, nil }}}
	r.lastMedia.Store(time.Now().UnixNano())
	if _, err := r.refresh(); err != nil || queries != 1 {
		t.Fatal("native-only track recovery failed", err)
	}
}
