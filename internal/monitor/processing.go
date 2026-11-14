package monitor

import (
	"fmt"
	"path/filepath"
	"time"

	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/state"
)

// processCopy runs independently of the discovery queue and the raw recorder.
// Limit concurrent converters; shutdown cancels both running and queued jobs.
// The caller holds e.mu, so the processor and recording settings are captured
// before another UI operation can change them.
func (e *Engine) processCopy(id, source string, cfg state.Config) {
	process := e.process
	queued := time.Now()
	quiet := false
	if it := e.items[id]; it != nil {
		quiet = it.broadcastID != "" && !it.segmented
	}
	e.wg.Go(func() {
		select {
		case e.processing <- struct{}{}:
			defer func() { <-e.processing }()
		case <-e.ctx.Done():
			return
		}
		started := time.Now()
		details := fmt.Sprintf("Source file: %q. Output format: %q. Queue wait: %s.",
			filepath.Base(source), cfg.OutputExtension, started.Sub(queued).Round(time.Millisecond))
		if !quiet {
			e.log.WriteDetails("INFO", id, "Creating a processed copy; original MKV is preserved", details, nil)
		}
		path, err := process(e.ctx, source, cfg)
		details += fmt.Sprintf(" Processing ran for %s.", time.Since(started).Round(time.Millisecond))
		if path != "" {
			details += fmt.Sprintf(" Output file: %q.", filepath.Base(path))
		}
		if e.ctx.Err() != nil {
			if !quiet {
				e.log.WriteDetails("INFO", id, "Processing interrupted on shutdown; original MKV is preserved", details, err)
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
			e.log.WriteDetails(level, id, "Processed copy failed: "+fault.Reason(err), details, err)
		} else if !quiet {
			e.log.WriteDetails("INFO", id, "Processed copy ready; original MKV kept", details, nil)
		}
	})
}
