package ffmpeg

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDiscoveryPrefersPATHAndHonorsExplicitSelection(t *testing.T) {
	dir := t.TempDir()
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	fromPATH := filepath.Join(dir, name)
	if err := os.WriteFile(fromPATH, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got, err := Find(""); err != nil || got != fromPATH {
		t.Fatal(got, err)
	}
	selected := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(selected, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := Find(selected); err != nil || got != selected {
		t.Fatal(got, err)
	}
	if _, err := Find(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("silently ignored an invalid explicit selection")
	}
}
