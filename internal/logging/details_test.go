package logging

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bigo-live-recorder/internal/fault"
)

func TestDiagnosticDetailsSurviveRotationAndReloadSeparately(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenSize(dir, 3000)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		l.WriteError("WARN", "demo", "Stream setup timed out; queued to retry", fault.Wrap("waiting for playable audio", context.DeadlineExceeded))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSize(dir, 3000)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entries := reopened.Entries()
	if len(entries) == 0 || entries[len(entries)-1].Sequence != 50 {
		t.Fatal("lost latest failure on reload")
	}
	for _, entry := range entries {
		if strings.Contains(entry.Message, "context deadline") || !strings.Contains(entry.Details, "waiting for playable audio: context deadline exceeded") {
			t.Fatal("readable and diagnostic text were mixed or lost", entry)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.log*"))
	if len(files) != 3 {
		t.Fatal("diagnostic logging stopped rotating", files)
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.Size() > 3000 {
			t.Fatal("diagnostic file exceeded its limit", err)
		}
	}
}
