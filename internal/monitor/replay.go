package monitor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"bigo-live-recorder/internal/state"
	"bigo-live-recorder/internal/timeline"
)

func replayCutoff(d state.Data, id string) (time.Time, bool) {
	var last time.Time
	for _, r := range d.Recordings {
		if r.BroadcastID != id || r.ID == "replay:"+id {
			continue
		}
		if r.EndedAt.IsZero() {
			return time.Time{}, false
		}
		if r.EndedAt.After(last) {
			last = r.EndedAt
		}
	}
	return last, true
}

// A capture can finish before BIGO confirms that its room disappeared (for
// example when its mode changes). Export the saved part without closing the
// broadcast; later guest sessions can still extend the same folder and replay.
func (e *Engine) resumeReplays() {
	d := e.store.Data()
	for _, b := range d.Broadcasts {
		cutoff, ready := replayCutoff(d, b.ID)
		if !ready || cutoff.IsZero() || b.ReplayError != "" || !b.ReplayPending && !cutoff.After(b.ReplayThrough) {
			continue
		}
		if _, err := os.Stat(filepath.Join(b.Directory, timeline.JournalName)); err == nil {
			if err := e.QueueReplay(b.ID); err != nil && e.ctx.Err() == nil {
				e.log.WriteError("WARN", b.StreamerID, "Could not queue the saved room replay; original recordings are preserved.", err)
			}
		}
	}
}

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
		cutoff, ready := replayCutoff(*d, id)
		if !ready {
			return errors.New("guest recordings are still being finalized")
		}
		b.ReplayPending = true
		b.ReplayError = ""
		broadcast = *b
		// History entries may have been removed while their raw files and
		// journal remain. Include every finalized file up to this request,
		// rather than silently omitting the removed entries from its replay.
		broadcast.ReplayThrough = maxTime(cutoff, time.Now())
		return nil
	})
	if err != nil {
		return err
	}
	e.replayJobs[id] = true
	cfg := e.store.Config()
	e.wg.Go(func() {
		followup := false
		defer func() {
			e.replayMu.Lock()
			delete(e.replayJobs, id)
			e.replayMu.Unlock()
			if followup && e.ctx.Err() == nil {
				d := e.store.Data()
				if slices.ContainsFunc(d.Broadcasts, func(b state.Broadcast) bool { return b.ID == id && b.ReplayPending }) {
					if cutoff, ready := replayCutoff(d, id); ready && !cutoff.IsZero() {
						e.QueueReplay(id)
					}
				}
			}
		}()
		select {
		case e.replaySlot <- struct{}{}:
			defer func() { <-e.replaySlot }()
		case <-e.ctx.Done():
			return
		}
		if !broadcast.EndedAt.IsZero() {
			if err := timeline.MarkEnded(broadcast.Directory, broadcast.ID, broadcast.EndedAt); err != nil {
				e.log.WriteError("WARN", broadcast.StreamerID, "Could not save the confirmed room end in its timeline; original recordings are preserved.", err)
			}
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
			cutoff, ready := replayCutoff(*d, id)
			for i := range d.Broadcasts {
				if d.Broadcasts[i].ID == id {
					b := &d.Broadcasts[i]
					b.ReplayPending = !ready || cutoff.After(broadcast.ReplayThrough)
					if err != nil {
						b.ReplayError = err.Error()
					} else {
						b.ReplayPath = path
						b.ReplayThrough = broadcast.ReplayThrough
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
		} else {
			followup = true
		}
		if err != nil {
			e.log.WriteError("WARN", broadcast.StreamerID, "Could not assemble the room replay. Original guest files and timing evidence are preserved; retry from recording history.", err)
		} else {
			e.log.Write("INFO", broadcast.StreamerID, fmt.Sprintf("Multi-guest replay ready (%d bytes). Original guest files kept. Replay: %s", size, path))
		}
	})
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
