package app

import (
	"context"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestLogDetailsCanBeCopiedAndSearchedWithoutClutteringMessage(t *testing.T) {
	a := accountFixture(t)
	a.page = "logs"
	a.log.WriteError("WARN", "demo", "Stream setup timed out; queued to retry", context.DeadlineExceeded)
	a.refresh()
	tt := ui.NewTester(a.view, 1400, 900)
	message, ok := tt.Find("Stream setup timed out; queued to retry")
	if !ok || tt.HasText("context deadline exceeded") {
		t.Fatal("diagnostic cause leaked into the main table or summary was hidden")
	}
	tt.RightClickAt(message.X+10, message.Y+message.H/2)
	if err := tt.ChooseMenuItem("Copy diagnostic details"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tt.Clipboard(), "context deadline exceeded") || !strings.Contains(tt.Clipboard(), "Stream setup timed out") {
		t.Fatal("copied diagnostics lost their readable context")
	}
	a.logQuery = "deadline exceeded"
	tt.Frame()
	if len(a.logRows()) != 1 {
		t.Fatal("technical cause cannot be found by searching logs")
	}
}
