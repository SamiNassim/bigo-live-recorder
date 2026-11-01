package fault

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func TestReadableErrorsRetainDiagnosticCauseAndErrorIdentity(t *testing.T) {
	cause := &net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234}, Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.20"), Port: 5678}, Err: os.ErrDeadlineExceeded}
	err := fmt.Errorf("receive stream: %w", Wrap("no playable native video arrived for 20 seconds", cause))
	if got := Reason(err); !strings.Contains(got, "video") || strings.Contains(got, "192.0.2") || strings.Contains(got, "tcp") {
		t.Fatal("transport details leaked into the readable message", got)
	}
	var network *net.OpError
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &network) {
		t.Fatal("wrapping lost the typed network cause")
	}
	if detail := Details(err); !strings.Contains(detail, "receive stream") || !strings.Contains(detail, "192.0.2.10:1234") || !strings.Contains(detail, "i/o timeout") {
		t.Fatal("diagnostic cause was lost", detail)
	}
}

func TestTimeoutDoesNotClaimBroadcastEndedOrHideLocalFailures(t *testing.T) {
	err := errors.Join(errors.New("setup"), Wrap("waiting in the request queue timed out", context.DeadlineExceeded))
	if Reason(err) != "waiting in the request queue timed out" || !errors.Is(err, context.DeadlineExceeded) || RequiresAction(err) {
		t.Fatal(err)
	}
	local := Action("could not write the recording", os.ErrPermission)
	if !RequiresAction(fmt.Errorf("record: %w", local)) || !errors.Is(local, os.ErrPermission) || !strings.Contains(Details(local), "permission denied") {
		t.Fatal("local failure classification or cause lost", local)
	}
}
