package monitor

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"bigo-live-recorder/internal/state"
	"bigo-live-recorder/internal/timeline"
)

// QueueReplay can also retry a failed/interrupted export from recording history.
// It never stops capture or performs network discovery. One replay runs at once.
func (e *Engine) QueueReplay(id string) error {
	e.replayMu.Lock()
	defer e.replayMu.Unlock()
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if e.replayJobs[id] {
		return nil
	}
	var broadcast state.Broadcast
	err := e.store.Update(func(d *state.Data) error {
		index := slices.IndexFunc(d.Broadcasts, func(b state.Broadcast) bool { return b.ID == id })
		if index < 0 {
			return errors.New("the broadcast is no longer in recording history")
		}
		b := &d.Broadcasts[index]
		if b.EndedAt.IsZero() {
			return errors.New("the room has not ended yet; its replay will be assembled after recording finishes")
		}
		if slices.ContainsFunc(d.Recordings, func(r state.Recording) bool { return r.BroadcastID == id && r.EndedAt.IsZero() }) {
			return errors.New("guest recordings are still being finalized")
		}
		b.ReplayPending = true
		b.ReplayError = ""
		broadcast = *b
		return nil
	})
	if err != nil {
		return err
	}
	e.replayJobs[id] = true
	cfg := e.store.Config()
	e.wg.Go(func() {
		defer func() { e.replayMu.Lock(); delete(e.replayJobs, id); e.replayMu.Unlock() }()
		select {
		case e.replaySlot <- struct{}{}:
			defer func() { <-e.replaySlot }()
		case <-e.ctx.Done():
			return
		}
		if err := timeline.MarkEnded(broadcast.Directory, broadcast.ID, broadcast.EndedAt); err != nil {
			e.log.WriteError("WARN", broadcast.StreamerID, "Could not save the confirmed room end in its timeline; original recordings are preserved.", err)
		}
		e.log.Write("INFO", broadcast.StreamerID, "Reconciling the multi-guest timeline and creating one replay video. Original guest files will be kept.")
		path, err := e.replay(e.ctx, broadcast, cfg)
		if e.ctx.Err() != nil {
			return
		} // Pending survives shutdown for the next launch.
		var size int64
		if err == nil {
			if info, statErr := os.Stat(path); statErr != nil {
				err = statErr
			} else if info.Size() == 0 {
				err = errors.New("the replay output is empty")
			} else {
				size = info.Size()
			}
		}
		saveErr := e.store.Update(func(d *state.Data) error {
			for i := range d.Broadcasts {
				if d.Broadcasts[i].ID == id {
					b := &d.Broadcasts[i]
					b.ReplayPending = false
					if err != nil {
						b.ReplayError = err.Error()
					} else {
						b.ReplayPath = path
						b.ReplayError = ""
					}
				}
			}
			if err == nil && slices.ContainsFunc(d.Recordings, func(r state.Recording) bool { return r.BroadcastID == id }) {
				record := state.Recording{ID: "replay:" + id, StreamerID: broadcast.StreamerID, Name: broadcast.StreamerID + " · Multi-guest replay", Path: path, StartedAt: broadcast.StartedAt, EndedAt: time.Now(), Bytes: size, BroadcastID: id}
				if i := slices.IndexFunc(d.Recordings, func(r state.Recording) bool { return r.ID == record.ID }); i >= 0 {
					d.Recordings[i] = record
				} else {
					d.Recordings = append(d.Recordings, record)
				}
			}
			return nil
		})
		if saveErr != nil {
			e.log.WriteError("ERROR", broadcast.StreamerID, "Could not save replay processing status; check configuration folder permissions and disk space.", saveErr)
		}
		if err != nil {
			e.log.WriteError("WARN", broadcast.StreamerID, "Could not assemble the room replay. Original guest files and timing evidence are preserved; retry from recording history.", err)
		} else {
			e.log.Write("INFO", broadcast.StreamerID, fmt.Sprintf("Multi-guest replay ready (%d bytes). Original guest files kept. Replay: %s", size, path))
		}
	})
	return nil
}
