package bigo

import (
	"context"

	"bigo-live-recorder/internal/bigolistener"
)

// RoomKind describes an explicitly identified room mode. Missing video or web
// playback alone never establishes that a room is audio-only.
type RoomKind uint8

const (
	RoomUnknown RoomKind = iota
	RoomAudioMultiGuest
	RoomHybridMultiGuest
)

func (r pJoinChannelRes) roomKind() RoomKind {
	const multiGuest, voice = 1 << 2, 1 << 4
	if r.flags&(multiGuest|voice) == multiGuest|voice {
		return RoomAudioMultiGuest
	}
	if r.flags&multiGuest != 0 {
		return RoomHybridMultiGuest
	}
	return RoomUnknown
}

// RoomKind reads room flags through the existing shared native connection. It
// does not join the room, open media sockets, or create another account login.
func (c *Client) RoomKind(ctx context.Context, source Stream, hub *bigolistener.Hub) (RoomKind, error) {
	room, err := queryNativeRoom(ctx, source, hub)
	if err != nil {
		return RoomUnknown, err
	}
	return room.roomKind(), nil
}
