package timeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalRecoveryAndOwnership(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, "fixture-room")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Entry{Kind: "file_end", File: "fixture.mkv"}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	f, err := os.OpenFile(filepath.Join(dir, JournalName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"kind":"file`)
	f.Close()
	j, err = Open(dir, "fixture-room")
	if err != nil {
		t.Fatal(err)
	}
	j.Append(Entry{Kind: "broadcast_end"})
	j.Close()
	entries, err := Read(dir)
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if _, err := Open(dir, "other-room"); err == nil {
		t.Fatal("accepted another room's timeline")
	}
}

func TestFrameSidecarSurvivesPartialTail(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, "fixture-room")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	w, err := j.Create(Entry{File: "fixture.mkv", Epoch: time.Unix(1900000000, 0)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Frame{Received: 123456789, SourcePTS: 0xfffffff0, SourceDTS: 0xffffffe0, PTS: 9000, DTS: 8500, Duration: 20 * time.Millisecond, Checksum: 1234, Video: true, Key: true, Codec: 4}
	w.Write(want)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".timeline", "fixture.mkv.bin")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{1, 2, 3})
	f.Close()
	n := 0
	err = Walk(path, func(got Frame) error {
		n++
		if got != want {
			t.Fatalf("got=%+v want=%+v", got, want)
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestLocalPathRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../outside.mkv", "https://example.invalid/video.mkv", filepath.Join(string(filepath.Separator), "outside.mkv")} {
		if _, err := LocalPath(dir, name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	os.WriteFile(outside, []byte("media"), 0644)
	if err := os.Symlink(outside, filepath.Join(dir, "linked.mkv")); err == nil {
		if _, err := LocalPath(dir, "linked.mkv"); err == nil {
			t.Fatal("accepted escaping symlink")
		}
	}
}
