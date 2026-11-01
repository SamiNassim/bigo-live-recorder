package recording

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bigo-live-recorder/internal/fault"
)

func processingFailure(ctxErr, err error, stderr string) error {
	if ctxErr != nil {
		message := "Processing was interrupted; original MKV kept"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			message = "Processing exceeded its 30-minute time limit; original MKV kept"
		}
		return fault.Wrap(message, errors.Join(ctxErr, ffmpegCause(err, stderr)))
	}
	return fault.Action(ffmpegMessage("create the processed copy", stderr)+"; original MKV kept", ffmpegCause(err, stderr))
}

func ffmpegCause(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w; FFmpeg output: %s", err, stderr)
}

func ffmpegMessage(operation, stderr string) string {
	message := "FFmpeg could not " + operation
	text := strings.ToLower(stderr)
	switch {
	case strings.Contains(text, "no space left"), strings.Contains(text, "not enough space on the disk"):
		return message + ": the disk is full. Free disk space before trying again"
	case strings.Contains(text, "permission denied"), strings.Contains(text, "access is denied"):
		return message + ": access to the file was denied. Check the output folder permissions"
	case strings.Contains(text, "unknown encoder"), strings.Contains(text, "encoder not found"):
		return message + ": the selected encoder is unavailable in this FFmpeg installation"
	case strings.Contains(text, "unrecognized option"), strings.Contains(text, "error splitting the argument list"):
		return message + ": check the custom output options"
	default:
		return message + ". The diagnostic log contains FFmpeg's error output"
	}
}
