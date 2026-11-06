package recording

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bigo-live-recorder/internal/fault"
)

func TestRecordingMissingVideoHeadersKeepsDiagnosticsWithoutRequiringAction(t *testing.T) {
	cause := errors.New("synthetic process failure")
	stderr := "dimensions not set\nCould not write header"
	err := recordingFFmpegFailure(ffmpegCause(cause, stderr), stderr)
	if fault.RequiresAction(err) || !errors.Is(err, cause) || !strings.Contains(fault.Reason(err), "codec headers") || !strings.Contains(fault.Details(err), stderr) {
		t.Fatal("source failure lost its meaning or diagnostics", err, fault.Details(err))
	}
	for _, local := range []string{"No space left on device", "not enough space on the disk", "Permission denied", "Access is denied"} {
		if err := recordingFFmpegFailure(cause, stderr+"\n"+local); !fault.RequiresAction(err) {
			t.Fatal("a local failure was treated as a transient guest source", err)
		}
	}
}

func TestProcessingFailureExplainsCauseWithoutLosingFFmpegOutput(t *testing.T) {
	for _, test := range []struct {
		stderr, want string
	}{
		{"Error writing trailer: No space left on device", "disk is full"},
		{"Permission denied", "folder permissions"},
		{"Unknown encoder 'example'", "encoder is unavailable"},
		{"Unrecognized option 'example'", "custom output options"},
		{"Invalid data found when processing input", "diagnostic log"},
	} {
		err := processingFailure(nil, errors.New("synthetic process failure"), test.stderr)
		if !fault.RequiresAction(err) || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "original MKV kept") || !strings.Contains(fault.Details(err), test.stderr) {
			t.Fatal("processing failure lost its meaning or diagnostics", err, fault.Details(err))
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := processingFailure(cause, errors.New("process killed"), "partial copy")
		if !errors.Is(err, cause) || fault.RequiresAction(err) || !strings.Contains(err.Error(), "original MKV kept") {
			t.Fatal("processing interruption lost its context", err)
		}
		if cause == context.DeadlineExceeded && !strings.Contains(err.Error(), "30-minute") {
			t.Fatal("processing deadline was unexplained", err)
		}
	}
}
