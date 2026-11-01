package monitor

import (
	"context"
	"strings"
	"testing"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/fault"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

func TestStartupFailureLogsOperationAndActualRetryPolicy(t *testing.T) {
	for _, test := range []struct {
		name, level string
		err         error
	}{
		{"timeout", "WARN", context.DeadlineExceeded},
		{"local failure", "ERROR", fault.Action("recording folder is not writable", nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, log := fixture(t, false, false)
			e := newEngine(store, log, &fakeClient{}, func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error) {
				return nil, test.err
			})
			defer e.Close()
			waitFor(t, func() bool {
				for _, entry := range log.Entries() {
					if strings.HasPrefix(entry.Message, "Could not start recording:") {
						if entry.Level != test.level || !strings.Contains(entry.Message, "Next check queued in 15s") || entry.Details == "" {
							t.Fatal("startup failure lost severity, operation, retry or diagnostics", entry)
						}
						if strings.Contains(entry.Message, "context deadline exceeded") || strings.Contains(entry.Message, "Broadcast ended") {
							t.Fatal("timeout was not explained honestly", entry)
						}
						return true
					}
				}
				return false
			})
		})
	}
}

func TestRecordingFailureDoesNotClaimSuccessfulSave(t *testing.T) {
	for _, bytes := range []int64{0, 1234} {
		level, message := recordingFailure(recording.Result{Bytes: bytes, Err: context.DeadlineExceeded}, false)
		if level != "WARN" || strings.Contains(message, "Broadcast ended") || strings.Contains(message, "Recording saved") {
			t.Fatal(level, message)
		}
		if (bytes > 0) != strings.Contains(message, "Existing recording kept") {
			t.Fatal("saved-file claim does not match the result", bytes, message)
		}
	}
	cfg := state.Defaults()
	cfg.NativeListening, cfg.PollingBackup = true, false
	if message := retryMessage(cfg, &item{initial: true}); !strings.Contains(message, "next live notification") || strings.Contains(message, "queued") {
		t.Fatal("invented a retry when native-only monitoring has none scheduled", message)
	}
}
