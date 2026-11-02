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
	base := cfg.RecordingPath
	if cfg.FolderPerStreamer {
		base = filepath.Join(base, safeName(streamer.ID)+"_"+safeName(streamer.DisplayName()))
	}
	return state.Broadcast{ID: fmt.Sprintf("%d:%s", source.UID, source.RoomID), StreamerID: streamer.ID, OwnerUID: source.UID, RoomID: source.RoomID, StartedAt: started,
		Directory: filepath.Join(base, safeName(streamer.ID)+"_"+started.Format("2006-01-02_15-04-05")+"_room-"+safeName(source.RoomID))}
}

func (h *Handle) Broadcast() state.Broadcast { return h.broadcast }
