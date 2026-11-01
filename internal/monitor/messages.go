package monitor

import (
	"fmt"
	"time"

	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

func retryMessage(cfg state.Config, it *item) string {
	if !eligible(cfg, it.initial, it.pending) {
		return "Waiting for the next live notification or a manual retry."
	}
	return fmt.Sprintf("Next check queued in %s; the request queue may add a delay.", max(time.Second, time.Until(it.row.NextCheck).Round(time.Second)))
}

func recordingFailure(result recording.Result, previousFile bool) (level, message string) {
	level = "WARN"
	if fault.RequiresAction(result.Err) {
		level = "ERROR"
	}
	message = "Recording stopped: "
	if previousFile {
		message = "Could not finalize the previous recording file: "
	}
	message += fault.Reason(result.Err) + "."
	if result.Bytes > 0 {
		message += fmt.Sprintf(" Existing recording kept (%d bytes); file completeness could not be confirmed.", result.Bytes)
	} else {
		message += " No saved media could be confirmed."
	}
	return level, message
}
