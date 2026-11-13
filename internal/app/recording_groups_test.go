package app

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestRecordingGroupUsesSavedHostAndGuestUsernames(t *testing.T) {
	a, b := groupedRecordingFixture(t)
	const host = "Demo · Host 🌙"
	if err := a.store.Update(func(d *state.Data) error {
		for i := range d.Recordings {
			r := &d.Recordings[i]
			if r.BroadcastID == b.ID && r.GuestName != "" {
				r.Name = host + " · " + r.GuestID + " (" + r.GuestName + ")"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.refresh()
	tt := ui.NewTester(a.view, 1400, 900)
	if !tt.HasText(host) || tt.HasText(b.StreamerID) {
		t.Fatal("group header did not use the saved host name", tt.Texts())
	}
	a.recordList.Open.Add(recordingKey{ID: b.ID, Group: true})
	tt.Frame()
	for _, label := range []string{"First Guest", "Second Guest", "Video · guest.one", "Audio · guest.two"} {
		if !tt.HasText(label) {
			t.Fatal("expanded guest label is missing", label, tt.Texts())
		}
	}
}

func TestRecordingGroupActionsFitWithoutHorizontalScrolling(t *testing.T) {
	for _, width := range []int{1050, 1262, 1400} {
		for _, replay := range []bool{true, false} {
			t.Run(fmt.Sprintf("width_%d/replay_%t", width, replay), func(t *testing.T) {
				a, _ := groupedRecordingFixture(t)
				label := "Replay"
				if !replay {
					label = "Folder"
					if err := a.store.Update(func(d *state.Data) error {
						d.Recordings = slices.DeleteFunc(d.Recordings, func(r state.Recording) bool { return r.ID == "replay:"+r.BroadcastID })
						d.Broadcasts[0].ReplayPath = ""
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					a.refresh()
				}
				tt := ui.NewTester(a.view, width, 900)
				button, ok := tt.Find(label)
				// Keep both the native button border and the cell's right
				// padding inside the table, including at minimum window size.
				if !ok || button.X+button.W > float32(width)-40 {
					t.Fatal("broadcast action is clipped at the right edge", label, button, width)
				}
			})
		}
	}
}

func TestRecordingGroupsSeparateBroadcastsAndKeepReplayOutOfSourceTotals(t *testing.T) {
	start := time.Unix(1900000000, 0)
	b := state.Broadcast{ID: "room-one", StreamerID: "host.example", StartedAt: start, ReplayPath: "replay.mkv"}
	records := []state.Recording{
		{ID: "video", BroadcastID: b.ID, StartedAt: start.Add(time.Minute), EndedAt: start.Add(3 * time.Minute), Bytes: 100, GuestUID: 900000001},
		{ID: "audio", BroadcastID: b.ID, StartedAt: start.Add(2 * time.Minute), EndedAt: start.Add(4 * time.Minute), Bytes: 200, GuestUID: 900000001},
		{ID: "guest-two", BroadcastID: b.ID, StartedAt: start.Add(3 * time.Minute), EndedAt: start.Add(5 * time.Minute), Bytes: 300, GuestUID: 900000002},
		{ID: "replay:" + b.ID, BroadcastID: b.ID, StartedAt: start, EndedAt: start.Add(time.Hour), Bytes: 9000, Path: b.ReplayPath},
		{ID: "other-room", StreamerID: b.StreamerID, BroadcastID: "room-two", StartedAt: start.Add(10 * time.Minute), EndedAt: start.Add(20 * time.Minute)},
		{ID: b.ID, Name: "Normal recording", StartedAt: start.Add(30 * time.Minute)},
	}
	tree := buildRecordingTree(records, []state.Broadcast{b})
	want := []recordingKey{{ID: b.ID}, {ID: "room-two", Group: true}, {ID: b.ID, Group: true}}
	if !slices.Equal(tree.roots, want) {
		t.Fatal("broadcasts were split by segment or conflated by host", tree.roots)
	}
	g := tree.groups[b.ID]
	if g.files != 3 || g.guests != 2 || g.bytes != 600 || g.active || !g.started.Equal(start) || !g.ended.Equal(start.Add(5*time.Minute)) {
		t.Fatal("derived replay inflated capture totals or duration", g)
	}
	wantChildren := []recordingKey{{ID: "replay:" + b.ID}, {ID: "video"}, {ID: "audio"}, {ID: "guest-two"}}
	if !slices.Equal(g.children, wantChildren) {
		t.Fatal("replay is not first or guest media is not chronological", g.children)
	}
	replayStart, replayEnd := tree.replaySpan(b.ID)
	if !replayStart.Equal(start.Add(time.Minute)) || !replayEnd.Equal(g.ended) {
		t.Fatal("replay duration used export completion time", replayStart, replayEnd)
	}
}

func TestRecordingGroupCacheObservesMetadataAndReleasesRemovedGroups(t *testing.T) {
	a := tableFixture()
	a.data.Recordings[0].BroadcastID = "room-one"
	a.data.Broadcasts = []state.Broadcast{{ID: "room-one", StreamerID: "host.example"}}
	key := recordingKey{ID: "room-one", Group: true}
	a.recordingGroups()
	a.recordList.Open.Add(key)
	if n := testing.AllocsPerRun(100, func() { a.recordingGroups() }); n != 0 {
		t.Fatal("unchanged recording groups allocate", n)
	}
	a.data.Broadcasts[0].ReplayPending = true
	if !a.recordingGroups().groups[key.ID].broadcast.ReplayPending || !a.recordList.Open.Has(key) {
		t.Fatal("metadata update was hidden or collapsed the group")
	}
	a.data.Recordings = nil
	if tree := a.recordingGroups(); len(tree.roots) != 0 || len(tree.groups) != 0 || a.recordList.Open.Has(key) {
		t.Fatal("clearing history restored an empty broadcast group")
	}
}

func groupedRecordingFixture(t *testing.T) (*application, state.Broadcast) {
	t.Helper()
	a := accountFixture(t)
	a.preview = false
	a.page = "recordings"
	start := time.Now().Add(-time.Hour)
	dir := t.TempDir()
	b := state.Broadcast{ID: "fixture-room", StreamerID: "host.example", Directory: dir, StartedAt: start, ReplayPath: filepath.Join(dir, "replay.mkv"), ReplayThrough: start.Add(10 * time.Minute)}
	if err := a.store.Update(func(d *state.Data) error {
		d.Broadcasts = []state.Broadcast{b}
		d.Recordings = []state.Recording{
			{ID: "video", StreamerID: b.StreamerID, BroadcastID: b.ID, GuestID: "guest.one", GuestName: "First Guest", Path: filepath.Join(dir, "video.mkv"), StartedAt: start, EndedAt: start.Add(5 * time.Minute)},
			{ID: "audio", StreamerID: b.StreamerID, BroadcastID: b.ID, GuestID: "guest.two", GuestName: "Second Guest", Path: filepath.Join(dir, "audio.mka"), StartedAt: start.Add(time.Minute), EndedAt: start.Add(10 * time.Minute)},
			{ID: "replay:" + b.ID, StreamerID: b.StreamerID, BroadcastID: b.ID, Path: b.ReplayPath, StartedAt: start, EndedAt: time.Now()},
			{ID: "normal", Name: "Normal Demo", StartedAt: start.Add(-time.Minute), EndedAt: start},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.refresh()
	return a, b
}

func TestRecordingGroupsExpandWithDisclosureAndKeepStateAcrossTabs(t *testing.T) {
	a, b := groupedRecordingFixture(t)
	tt := ui.NewTester(a.view, 1400, 900)
	key := recordingKey{ID: b.ID, Group: true}
	if a.recordList.Rows() != 2 || tt.HasText("First Guest") || !tt.HasText("Replay ready") {
		t.Fatal("multi-guest recordings are not collapsed into one row", tt.Texts())
	}
	name, _ := tt.Find(b.StreamerID)
	kind, _ := tt.Find("Multi-guest")
	tt.ClickAt(name.X-8, (name.Y+kind.Y+kind.H)/2)
	if !a.recordList.Open.Has(key) || a.recordList.Rows() != 5 || !tt.HasText("First Guest") || !tt.HasText("Room replay") {
		t.Fatal("disclosure did not expand guest files", tt.Texts())
	}
	if a.recordList.Item(1).ID != "replay:"+b.ID {
		t.Fatal("the replay is not the first expanded row")
	}
	a.page = "settings"
	tt.Frame()
	a.page = "recordings"
	tt.Frame()
	if !tt.HasText("Second Guest") || !a.recordList.Open.Has(key) {
		t.Fatal("switching tabs collapsed the group")
	}
	if err := tt.Click(b.StreamerID); err != nil {
		t.Fatal(err)
	}
	tt.Key(0, ui.KeyLeft)
	if a.recordList.Open.Has(key) || tt.HasText("First Guest") {
		t.Fatal("keyboard collapse failed")
	}
	tt.Key(0, ui.KeyRight)
	if !a.recordList.Open.Has(key) || !tt.HasText("First Guest") {
		t.Fatal("keyboard expansion failed")
	}
}

func TestSplitBroadcastShowsWholeDurationAndExpandableClips(t *testing.T) {
	a := accountFixture(t)
	a.page = "recordings"
	start := time.Now().Add(-time.Hour)
	split := start.Add(30 * time.Minute)
	dir := t.TempDir()
	b := state.Broadcast{ID: "split:fixture", StreamerID: "host.example", Directory: dir, StartedAt: start, EndedAt: start.Add(45 * time.Minute), Segmented: true}
	a.data.Broadcasts = []state.Broadcast{b}
	a.data.Recordings = []state.Recording{
		{ID: "first", Name: "Demo Host", BroadcastID: b.ID, Path: filepath.Join(dir, "first.mkv"), StartedAt: start, EndedAt: split},
		{ID: "second", Name: "Demo Host", BroadcastID: b.ID, Path: filepath.Join(dir, "second.mkv"), StartedAt: split, EndedAt: b.EndedAt},
	}
	tt := ui.NewTester(a.view, 1400, 900)
	if a.recordList.Rows() != 1 || !tt.HasText("45m 00s") || !tt.HasText("2 clips") || tt.HasText("Clip 1") {
		t.Fatal("split broadcast did not show a single row with its total duration", tt.Texts())
	}
	name, _ := tt.Find("Demo Host")
	kind, _ := tt.Find("Split recording")
	tt.ClickAt(name.X-8, (name.Y+kind.Y+kind.H)/2)
	if a.recordList.Rows() != 3 || !tt.HasText("Clip 1") || !tt.HasText("Clip 2") ||
		!tt.HasText("30m 00s") || !tt.HasText("15m 00s") {
		t.Fatal("expanded group did not show individual clips and durations", tt.Texts())
	}
	col, _ := tt.Find("Started")
	clip, _ := tt.Find("Clip 2")
	tt.RightClickAt(col.X+8, clip.Y+clip.H/2)
	if slices.Contains(tt.Menu(), "Assemble room replay") {
		t.Fatal("a timed clip showed multi-guest replay actions", tt.Menu())
	}
	if err := tt.ChooseMenuItem("Copy file path"); err != nil || tt.Clipboard() != filepath.Join(dir, "second.mkv") {
		t.Fatal("clip action did not target the selected file", err, tt.Clipboard())
	}
}

func TestRecordingGroupMenusTargetFoldersAndIndividualFiles(t *testing.T) {
	a, b := groupedRecordingFixture(t)
	tt := ui.NewTester(a.view, 1400, 900)
	name, _ := tt.Find(b.StreamerID)
	column, _ := tt.Find("Started")
	tt.RightClickAt(column.X+8, name.Y+name.H/2)
	if err := tt.ChooseMenuItem("Copy folder path"); err != nil || tt.Clipboard() != b.Directory {
		t.Fatal("group menu did not target its broadcast folder", err, tt.Clipboard())
	}
	tt.RightClickAt(column.X+8, name.Y+name.H/2)
	if err := tt.ChooseMenuItem("Expand recordings"); err != nil {
		t.Fatal(err)
	}
	guest, _ := tt.Find("Second Guest")
	tt.RightClickAt(column.X+8, guest.Y+guest.H/2)
	if err := tt.ChooseMenuItem("Copy file path"); err != nil || tt.Clipboard() != filepath.Join(b.Directory, "audio.mka") {
		t.Fatal("guest menu targeted the host or another file", err, tt.Clipboard())
	}
}

func TestRecordingGroupReplayStatusRefreshesWithoutNewFiles(t *testing.T) {
	a, _ := groupedRecordingFixture(t)
	tt := ui.NewTester(a.view, 1400, 900)
	if err := a.store.Update(func(d *state.Data) error { d.Broadcasts[0].ReplayPending = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !a.refreshVisible() {
		t.Fatal("a replay metadata update did not repaint Recordings")
	}
	tt.Frame()
	if !tt.HasText("Creating replay…") || a.recordList.Rows() != 2 {
		t.Fatal("replay status was stale or expanded its group")
	}
}

func TestRecordingGroupDistinguishesSavedReplayFromNewCapture(t *testing.T) {
	a, b := groupedRecordingFixture(t)
	if err := a.store.Update(func(d *state.Data) error {
		d.Recordings = append(d.Recordings, state.Recording{ID: "later", BroadcastID: b.ID, Path: filepath.Join(b.Directory, "later.mka"), StartedAt: b.ReplayThrough.Add(time.Minute)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.refresh()
	a.recordList.Open.Add(recordingKey{ID: b.ID, Group: true})
	tt := ui.NewTester(a.view, 1400, 900)
	if !tt.HasText("Recording · saved replay") || !tt.HasText("Saved replay · earlier capture") || tt.HasText("Replay ready") {
		t.Fatal("an older snapshot looked like the current live replay", tt.Texts())
	}
	if err := a.store.Update(func(d *state.Data) error {
		d.Recordings[len(d.Recordings)-1].EndedAt = b.ReplayThrough.Add(2 * time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.refresh()
	tt.Frame()
	if !tt.HasText("Earlier replay saved") || tt.HasText("Replay ready") {
		t.Fatal("new finalized media was presented as already in the replay", tt.Texts())
	}
}
