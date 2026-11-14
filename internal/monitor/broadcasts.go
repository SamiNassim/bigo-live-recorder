package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
	"bigo-live-recorder/internal/timeline"
)

type guestProfileClient interface {
	Profile(context.Context, uint32, *bigolistener.Hub) (bigo.Profile, error)
}

func (e *Engine) broadcastOptions(ctx context.Context, streamer state.Streamer, source bigo.Stream, cfg state.Config) (recording.Options, error) {
	options := recording.Options{}
	if source.RoomKind != bigo.RoomAudioMultiGuest && source.RoomKind != bigo.RoomHybridMultiGuest {
		if cfg.SplitMinutes > 0 {
			options.Broadcast = recording.NewSegmentedBroadcast(streamer, source, cfg, time.Now())
			for _, saved := range e.store.Data().Broadcasts {
				if saved.ID == options.Broadcast.ID {
					options.Broadcast = saved
					break
				}
			}
		}
		return options, nil
	}
	candidate := recording.NewBroadcast(streamer, source, cfg, time.Now())
	data := e.store.Data()
	known := make(map[uint32]bigo.Profile)
	for _, saved := range data.Recordings {
		if saved.BroadcastID == candidate.ID && saved.GuestUID != 0 && saved.GuestID != "" && saved.GuestIdentityVersion == bigo.ProfileIdentityVersion {
			known[saved.GuestUID] = bigo.Profile{UID: saved.GuestUID, ID: saved.GuestID, Name: saved.GuestName, IdentityVersion: saved.GuestIdentityVersion}
		}
	}
	if client, ok := e.client.(guestProfileClient); ok {
		options.GuestProfile = func(ctx context.Context, uid uint32) (bigo.Profile, error) {
			if profile, exists := known[uid]; exists {
				return profile, nil
			}
			return client.Profile(ctx, uid, e.hub)
		}
	}
	for _, saved := range data.Broadcasts {
		if saved.ID == candidate.ID {
			options.Broadcast = saved
			return options, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return options, err
	}
	err := e.store.Update(func(data *state.Data) error {
		for _, saved := range data.Broadcasts {
			if saved.ID == candidate.ID {
				options.Broadcast = saved
				return nil
			}
		}
		data.Broadcasts = append(data.Broadcasts, candidate)
		options.Broadcast = candidate
		return nil
	})
	if err != nil {
		return options, fault.Action("could not save the broadcast folder and identity; check configuration folder permissions and available disk space", err)
	}
	return options, nil
}

func (e *Engine) broadcastNotified(id string) bool {
	for _, saved := range e.store.Data().Broadcasts {
		if saved.ID == id {
			return saved.Notified
		}
	}
	return false
}

// Keep the stream clock on the broadcast start or the capture's first file.
func recordingClockStart(h activeRecording) time.Time {
	if source, ok := h.(interface{ Broadcast() state.Broadcast }); ok {
		if broadcast := source.Broadcast(); broadcast.ID != "" && !broadcast.StartedAt.IsZero() {
			return broadcast.StartedAt
		}
	}
	if files, ok := h.(interface{ FirstFile() (string, time.Time) }); ok {
		_, started := files.FirstFile()
		return started
	}
	return h.Started()
}

func (e *Engine) notifyRecording(it *item, path string) {
	if it.recordNotified {
		return
	}
	it.recordNotified = true
	message := "Recording started"
	details := fmt.Sprintf("File: %q. %s", filepath.Base(path), recordingSettings(it.row.Streamer, it.config))
	if it.broadcastID != "" && !it.segmented {
		e.persist(func(data *state.Data) {
			for i := range data.Broadcasts {
				if data.Broadcasts[i].ID == it.broadcastID {
					data.Broadcasts[i].Notified = true
					message = "Multi-guest broadcast recording started"
					details += fmt.Sprintf(" Broadcast folder: %q.", filepath.Base(data.Broadcasts[i].Directory))
				}
			}
		})
	}
	e.log.WriteDetails("INFO", it.row.Streamer.ID, message, details, nil)
	select {
	case e.events <- Event{it.row.Streamer.DisplayName(), it.row.Streamer.ID, path}:
	default:
	}
}

func finishBroadcastFile(data *state.Data, path string, result recording.Result) {
	for i := range data.Recordings {
		saved := &data.Recordings[i]
		if saved.Path != path || !saved.EndedAt.IsZero() {
			continue
		}
		saved.EndedAt, saved.Bytes = result.EndedAt, result.Bytes
		if result.Err != nil {
			saved.Error = result.Err.Error()
		}
		for j := range data.Broadcasts {
			if data.Broadcasts[j].ID == saved.BroadcastID {
				if result.Bytes > 0 {
					data.Broadcasts[j].Files++
					data.Broadcasts[j].Bytes += result.Bytes
				}
			}
		}
	}
}

func (e *Engine) finishBroadcast(id string) {
	var ended state.Broadcast
	err := e.store.Update(func(data *state.Data) error {
		for i := range data.Broadcasts {
			if data.Broadcasts[i].ID == id && data.Broadcasts[i].EndedAt.IsZero() {
				data.Broadcasts[i].EndedAt = time.Now()
				ended = data.Broadcasts[i]
				break
			}
		}
		return nil
	})
	if err != nil {
		e.log.WriteError("ERROR", "storage", "Could not save broadcast completion; check available disk space and configuration folder permissions.", err)
		return
	}
	if ended.ID != "" && ended.Files > 0 {
		e.log.WriteDetails("INFO", ended.StreamerID, "Multi-guest broadcast finalized",
			fmt.Sprintf("Room ended. Broadcast folder: %q. Files saved: %d. Bytes on disk: %d.", filepath.Base(ended.Directory), ended.Files, ended.Bytes), nil)
		if _, err := os.Stat(filepath.Join(ended.Directory, timeline.JournalName)); err == nil {
			e.persist(func(d *state.Data) {
				for i := range d.Broadcasts {
					if d.Broadcasts[i].ID == ended.ID {
						d.Broadcasts[i].ReplayPending = true
					}
				}
			})
			if e.ctx.Err() != nil {
				return
			}
			if err := e.QueueReplay(ended.ID); err != nil {
				e.log.WriteError("WARN", ended.StreamerID, "Could not queue the room replay; original recordings are preserved.", err)
			}
		}
	}
}

func (e *Engine) finishRoom(streamer, room string) {
	for _, saved := range e.store.Data().Broadcasts {
		if !saved.Segmented && saved.StreamerID == streamer && saved.RoomID == room && saved.EndedAt.IsZero() {
			e.finishBroadcast(saved.ID)
		}
	}
}

// A failed transport, an empty room, or a paused recorder is not proof that a
// broadcast ended. Verify old rooms only during an otherwise scheduled check.
func (e *Engine) verifyPreviousBroadcasts(ctx context.Context, id string, current bigo.Stream, cfg state.Config) {
	client, ok := e.client.(nativeStreamClient)
	if !ok || !cfg.NativeListening {
		return
	}
	for _, saved := range e.store.Data().Broadcasts {
		if saved.Segmented || saved.StreamerID != id || !saved.EndedAt.IsZero() || (current.Alive == 1 && current.RoomID == saved.RoomID) {
			continue
		}
		e.mu.Lock()
		if e.verifyingBroadcasts == nil {
			e.verifyingBroadcasts = make(map[string]bool)
		}
		if e.verificationSlots == nil {
			e.verificationSlots = make(chan struct{}, 1)
		}
		slots := e.verificationSlots
		pending := e.verifyingBroadcasts[saved.ID]
		e.verifyingBroadcasts[saved.ID] = true
		e.mu.Unlock()
		if pending {
			continue
		}
		e.wg.Go(func() {
			defer func() { e.mu.Lock(); delete(e.verifyingBroadcasts, saved.ID); e.mu.Unlock() }()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			verify, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			_, err := client.OriginStream(verify, bigo.Stream{Alive: 1, UID: saved.OwnerUID, RoomID: saved.RoomID}, e.hub)
			if errors.Is(err, bigo.ErrRoomEnded) {
				e.finishBroadcast(saved.ID)
			}
		})
	}
}
