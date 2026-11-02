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
	uid uint32
}

func (c participantCapture) Next(ctx context.Context) (*bigo.CapturePart, error) {
	part, err := c.Capture.Next(ctx)
	if part != nil {
		part.ParticipantUID = c.uid
	}
	return part, err
}

func startGuestRoom(ctx context.Context, cancel context.CancelFunc, ffmpeg string, streamer state.Streamer, cfg state.Config, room bigo.GuestRoom, report func(string, string)) *Handle {
	h := &Handle{cancel: cancel, done: make(chan struct{}), activeFiles: make(map[string]time.Time)}
	go func() {
		defer close(h.done)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { room.Close() })
		defer stop()
		defer room.Close()
		var workers sync.WaitGroup
		var roomErr error
		for {
			participant, err := room.Next(ctx)
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, io.EOF) {
					roomErr = err
				}
				break
			}
			workers.Go(func() {
				capture := participantCapture{Capture: participant.Capture, uid: participant.UID}
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
				h.appendGuestChange(FileChange{Path: child.File(), StartedAt: child.Started()})
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
						h.appendGuestChange(FileChange{Path: result.Path, StartedAt: child.Started(), Result: &result})
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
