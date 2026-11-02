package monitor

import (
	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/state"
)

// processCopy runs independently of the discovery queue and the raw recorder.
// Limit concurrent converters; shutdown cancels both running and queued jobs.
// The caller holds e.mu, so the processor and recording settings are captured
// before another UI operation can change them.
func (e *Engine) processCopy(id, source string, cfg state.Config) {
	process := e.process
	quiet := false
	if it := e.items[id]; it != nil {
		quiet = it.broadcastID != ""
	}
	e.wg.Go(func() {
		select {
		case e.processing <- struct{}{}:
			defer func() { <-e.processing }()
		case <-e.ctx.Done():
			return
		}
		if !quiet {
			e.log.Write("INFO", id, "Creating a processed copy; original MKV is preserved")
		}
		path, err := process(e.ctx, source, cfg)
		if e.ctx.Err() != nil {
			if !quiet {
				e.log.Write("INFO", id, "Processing interrupted on shutdown; original MKV is preserved")
			}
			return
		}
		e.persist(func(d *state.Data) {
			for i := range d.Recordings {
				if d.Recordings[i].Path == source {
					d.Recordings[i].ProcessedPath = path
					if err != nil {
						d.Recordings[i].Error = err.Error()
					}
				}
			}
		})
		if err != nil {
			level := "WARN"
			if fault.RequiresAction(err) {
				level = "ERROR"
			}
			e.log.WriteError(level, id, "Processed copy failed: "+fault.Reason(err), err)
		} else if !quiet {
			e.log.Write("INFO", id, "Processed copy ready; original MKV kept")
		}
	})
}
