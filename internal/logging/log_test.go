package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotationAndReload(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenSize(dir, 800)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		l.Write("warning", "alice", strings.Repeat("x", 80))
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.log*"))
	if len(names) != 3 {
		t.Fatalf("files=%v", names)
	}
	for _, name := range names {
		st, _ := os.Stat(name)
		if st.Size() > 800 {
			t.Fatalf("unbounded segment: %d", st.Size())
		}
	}
	r, err := OpenSize(dir, 800)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	e := r.Entries()
	if len(e) == 0 || e[len(e)-1].Sequence != 50 {
		t.Fatalf("lost tail: %+v", e)
	}
	r.ClearView()
	if len(r.Entries()) != 0 {
		t.Fatal("view was not cleared")
	}
}
