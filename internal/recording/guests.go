package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/state"
)

type participantCapture struct {
	bigo.Capture
	uid       uint32
	profile   bigo.Profile
	broadcast state.Broadcast
}

func (c participantCapture) Next(ctx context.Context) (*bigo.CapturePart, error) {
	part, err := c.Capture.Next(ctx)
	if part != nil {
		part.ParticipantUID = c.uid
		part.ParticipantID, part.ParticipantName = c.profile.ID, c.profile.Name
		part.BroadcastID, part.Directory = c.broadcast.ID, c.broadcast.Directory
	}
	return part, err
}

func startGuestRoom(ctx context.Context, cancel context.CancelFunc, ffmpeg string, streamer state.Streamer, cfg state.Config, room bigo.GuestRoom, report func(string, string), settings ...Options) *Handle {
	options := Options{}
	if len(settings) > 0 {
		options = settings[0]
	}
	h := &Handle{cancel: cancel, done: make(chan struct{}), activeFiles: make(map[string]time.Time)}
	h.broadcast = options.Broadcast
	go func() {
		defer close(h.done)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { room.Close() })
		defer stop()
		defer room.Close()
		var workers sync.WaitGroup
		var roomErr error
		ended := false
		profiles := &guestProfiles{resolve: options.GuestProfile, entries: make(map[uint32]*guestProfileEntry)}
		for {
			participant, err := room.Next(ctx)
			if err != nil {
				ended = errors.Is(err, bigo.ErrRoomEnded)
				if ctx.Err() == nil && !errors.Is(err, io.EOF) && !ended {
					roomErr = err
				}
				break
			}
			workers.Go(func() {
				profile, lookupErr := profiles.get(ctx, participant.UID)
				if profile.UID == 0 {
					profile.UID = participant.UID
				}
				if lookupErr != nil && ctx.Err() == nil && report != nil {
					report("WARN", fmt.Sprintf("Guest %d could not be identified; keeping the original media under its numeric UID.", participant.UID))
				}
				capture := participantCapture{Capture: participant.Capture, uid: participant.UID, profile: profile, broadcast: options.Broadcast}
				// The room owns cancellation. Closing one source closes only
				// its capture; it must never cancel its sibling recorders.
				child, err := startCapture(ctx, func() {}, ffmpeg, streamer, cfg, capture, nil)
				if err != nil {
					if ctx.Err() == nil && !errors.Is(err, io.EOF) && report != nil {
						report("WARN", fmt.Sprintf("Could not start guest %d: %s", participant.UID, fault.Reason(err)))
					}
					if fault.RequiresAction(err) {
						cancel()
					}
					return
				}
				h.appendGuestChange(FileChange{Path: child.File(), StartedAt: child.Started(), BroadcastID: options.Broadcast.ID, Guest: profile})
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						for _, change := range child.DrainFileChanges() {
							h.appendGuestChange(change)
						}
					case <-child.Done():
						for _, change := range child.DrainFileChanges() {
							h.appendGuestChange(change)
						}
						result := child.Result()
						h.appendGuestChange(FileChange{Path: result.Path, StartedAt: child.Started(), Result: &result, BroadcastID: options.Broadcast.ID, Guest: profile})
						if fault.RequiresAction(result.Err) {
							cancel()
						}
						return
					}
				}
			})
		}
		// At natural EOF, let each source drain all received packets before
		// closing its capture. Cancellation instead closes every pipe promptly.
		workers.Wait()
		h.mu.Lock()
		h.result.RoomEnded = ended
		if roomErr != nil {
			h.result.Err = errors.Join(h.result.Err, roomErr)
		}
		if h.result.EndedAt.IsZero() {
			h.result.EndedAt = time.Now()
		}
		h.mu.Unlock()
	}()
	return h
}

type guestProfileEntry struct {
	done    chan struct{}
	profile bigo.Profile
	err     error
}
type guestProfiles struct {
	mu      sync.Mutex
	resolve func(context.Context, uint32) (bigo.Profile, error)
	entries map[uint32]*guestProfileEntry
}

func (c *guestProfiles) get(ctx context.Context, uid uint32) (bigo.Profile, error) {
	c.mu.Lock()
	entry, exists := c.entries[uid]
	if !exists {
		entry = &guestProfileEntry{done: make(chan struct{})}
		c.entries[uid] = entry
	}
	c.mu.Unlock()
	if !exists {
		if c.resolve != nil {
			entry.profile, entry.err = c.resolve(ctx, uid)
			if entry.err == nil && entry.profile.UID != uid {
				entry.profile = bigo.Profile{}
				entry.err = errors.New("BIGO returned a profile for another guest")
			}
			if entry.err != nil {
				entry.profile = bigo.Profile{UID: uid}
			}
		}
		close(entry.done)
	}
	select {
	case <-ctx.Done():
		return bigo.Profile{}, ctx.Err()
	case <-entry.done:
		if exists {
			return entry.profile, nil
		}
		return entry.profile, entry.err
	}
}

func (h *Handle) appendGuestChange(change FileChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if change.Result == nil {
		h.currentPath, h.currentStarted = change.Path, change.StartedAt
		h.activeFiles[change.Path] = change.StartedAt
	} else {
		h.result = *change.Result
		// Individual errors are carried by FileChange. Result describes the
		// room's transport lifetime, avoiding duplicate failure notifications.
		h.result.Err = nil
		delete(h.activeFiles, change.Path)
		if change.Path == h.currentPath {
			h.currentPath, h.currentStarted = "", time.Time{}
			for path, started := range h.activeFiles {
				if started.After(h.currentStarted) {
					h.currentPath, h.currentStarted = path, started
				}
			}
		}
	}
	h.changes = append(h.changes, change)
}
