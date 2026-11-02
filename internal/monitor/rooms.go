package monitor

import (
	"context"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/state"
)

type nativeRoomClient interface {
	RoomKind(context.Context, bigo.Stream, *bigolistener.Hub) (bigo.RoomKind, error)
}

func (e *Engine) inspectRoom(ctx context.Context, id string, source bigo.Stream, cfg state.Config) bigo.Stream {
	client, ok := e.client.(nativeRoomClient)
	// Web status 4 identifies an app-only room, not its subtype. Only explicit
	// native flags can establish that it is an audio-only multi-guest room.
	if !ok || !cfg.NativeListening || source.Alive != 1 || source.RoomStatus != 4 || source.UID == 0 {
		return source
	}
	e.mu.Lock()
	it := e.items[id]
	if it == nil || it.row.Streamer.Paused {
		e.mu.Unlock()
		return source
	}
	it.row.Streamer.UID = source.UID
	e.subscribeLocked(it)
	e.mu.Unlock()
	if kind, err := client.RoomKind(ctx, source, e.hub); err == nil {
		source.RoomKind = kind
	}
	// An unavailable subtype leaves recording on its existing path; it must
	// never be mistaken for positive evidence of an audio-only room.
	return source
}

func (e *Engine) audioRoomLocked(it *item, room string) {
	it.row.Status = "Live"
	it.row.RoomKind = bigo.RoomAudioMultiGuest
	it.row.Error = ""
	it.row.Live = true
	it.pending = false
	it.retryUntil = time.Time{}
	if it.audioRoom != room || it.audioRoom == "" {
		e.log.Write("INFO", it.row.Streamer.ID, "Live in an audio-only multi-guest room; video recording skipped. Monitoring remains enabled.")
		it.audioRoom = room
	}
}
