package monitor

import (
	"fmt"
	"path/filepath"
	"time"

	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

func recordingSettings(streamer state.Streamer, cfg state.Config) string {
	quality := streamer.Quality
	if quality == "" {
		quality = cfg.StreamQuality
	}
	return fmt.Sprintf("Requested quality: %s. Split interval: %d min (0 disables timed splitting). Processed copies: %t.", quality, cfg.SplitMinutes, cfg.PostProcess)
}

func recordingDetails(result recording.Result) string {
	details := fmt.Sprintf("Bytes on disk: %d.", result.Bytes)
	if result.Path != "" {
		details = fmt.Sprintf("File: %q. %s", filepath.Base(result.Path), details)
	}
	if result.Source != "" {
		details += fmt.Sprintf(" Media source: %s.", result.Source)
	}
	if !result.StartedAt.IsZero() {
		details += fmt.Sprintf(" Opened at %s. File open for %s.", result.StartedAt.Format(time.RFC3339Nano), result.EndedAt.Sub(result.StartedAt).Round(time.Millisecond))
	}
	if result.FFmpegWarnings != "" {
		details += "\nFFmpeg warning output (tail):\n" + result.FFmpegWarnings
	}
	return details
}

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
