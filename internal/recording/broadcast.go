package recording

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
)

// Options provides persistent room identity and optional guest profile lookup.
type Options struct {
	Broadcast    state.Broadcast
	GuestProfile func(context.Context, uint32) (bigo.Profile, error)
}

// NewBroadcast proposes a folder for a newly observed multi-guest broadcast.
// The monitor persists this choice and reuses it for the full room lifetime.
func NewBroadcast(streamer state.Streamer, source bigo.Stream, cfg state.Config, started time.Time) state.Broadcast {
	base := recordingDirectory(streamer, cfg)
	return state.Broadcast{ID: fmt.Sprintf("%d:%s", source.UID, source.RoomID), StreamerID: streamer.ID, OwnerUID: source.UID, RoomID: source.RoomID, StartedAt: started,
		Directory: filepath.Join(base, safeName(streamer.ID)+"_"+started.Format("2006-01-02_15-04-05")+"_room-"+safeName(source.RoomID))}
}

// NewSegmentedBroadcast uses the provider room when available. With no room
// identity, each capture gets its own group and folder.
func NewSegmentedBroadcast(streamer state.Streamer, source bigo.Stream, cfg state.Config, now time.Time) state.Broadcast {
	room := source.RoomID
	if room == "" || room == "0" {
		room = fmt.Sprintf("capture-%d", now.UnixNano())
	}
	id := fmt.Sprintf("split:%q:%q", streamer.ID, room)
	folder := safeName(streamer.ID) + "_" + now.Format("2006-01-02_15-04-05") + "_stream-" + safeName(room)
	return state.Broadcast{ID: id, StreamerID: streamer.ID, OwnerUID: source.UID, RoomID: source.RoomID, Segmented: true,
		Directory: filepath.Join(recordingDirectory(streamer, cfg), folder)}
}

func (h *Handle) Broadcast() state.Broadcast { return h.broadcast }
