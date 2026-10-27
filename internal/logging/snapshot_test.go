package logging

import "testing"

func TestLogSnapshotsDetectClearsAndNewEntries(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, revision := l.EntriesSince(0)
	if revision == 0 {
		t.Fatal("empty log has no initial revision")
	}
	l.Write("INFO", "", "Synthetic entry")
	entries, next := l.EntriesSince(revision)
	if next <= revision || len(entries) != 1 {
		t.Fatal("new entry was missed")
	}
	entries[0].Message = "edited"
	if l.Entries()[0].Message != "Synthetic entry" {
		t.Fatal("snapshot aliases live entries")
	}
	if n := testing.AllocsPerRun(100, func() { l.EntriesSince(next) }); n != 0 {
		t.Fatal("unchanged tail allocates", n)
	}
	l.ClearView()
	entries, cleared := l.EntriesSince(next)
	if len(entries) != 0 || cleared <= next {
		t.Fatal("clear did not invalidate the snapshot")
	}
	l.Write("WARN", "", "After clear")
	entries, after := l.EntriesSince(cleared)
	if len(entries) != 1 || after <= cleared || entries[0].Sequence != 2 {
		t.Fatal("entry after clear was missed")
	}
}
