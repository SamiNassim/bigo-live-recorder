package bigo

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"bigo-live-recorder/internal/fault"
)

// One transport goroutine owns each log. Socket retries belong to the same
// interruption until complete media frames arrive on that transport again.
type guestReconnectLog struct {
	recovery StreamRecovery
	track    string
	enabled  bool
	since    time.Time
	failures int
	warned   bool
	recent   []error
}

func (l *guestReconnectLog) failed(now time.Time, cause error) {
	// A room can have no cameras. Report video reconnections only after this
	// transport has delivered media.
	if !l.enabled {
		return
	}
	first := l.since.IsZero()
	if first {
		l.since = now
	}
	l.failures++
	// Preserve the first failure and the seven most recent failures. Diagnostics
	// remain bounded even when a camera connection stays idle for hours.
	if len(l.recent) == 8 {
		copy(l.recent[1:], l.recent[2:])
		l.recent = l.recent[:7]
	}
	l.recent = append(l.recent, fmt.Errorf("failure %d (+%s): %w", l.failures, now.Sub(l.since).Round(time.Second), cause))
	if first {
		l.recovery.reportError("INFO", "Room "+l.track+" connection reconnecting.", l.cause(cause))
	}
	l.waiting(now)
}

func (l *guestReconnectLog) waiting(now time.Time) {
	if l.since.IsZero() || l.warned || now.Sub(l.since) < time.Minute {
		return
	}
	l.warned = true
	l.recovery.reportError("WARN", fmt.Sprintf("Room %s connection is still reconnecting after %s.", l.track, now.Sub(l.since).Round(time.Second)), l.cause(nil))
}

func (l *guestReconnectLog) resumed(now time.Time) {
	if !l.since.IsZero() {
		l.recovery.reportError("INFO", fmt.Sprintf("Room %s reception resumed after %s.", l.track, now.Sub(l.since).Round(time.Second)), l.cause(nil))
	}
	*l = guestReconnectLog{recovery: l.recovery, track: l.track, enabled: true}
}

func (l *guestReconnectLog) cause(fallback error) error {
	if len(l.recent) == 0 {
		return fallback
	}
	return fault.Wrap(fmt.Sprintf("Room %s recovery: %d failures", l.track, l.failures), errors.Join(slices.Clone(l.recent)...))
}
