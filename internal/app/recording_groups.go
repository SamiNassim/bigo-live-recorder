package app

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"bigo-live-recorder/internal/state"
)

// Group and file keys remain distinct even when their provider IDs coincide.
type recordingKey struct {
	ID    string
	Group bool
}

type recordingGroup struct {
	broadcast state.Broadcast
	hostName  string
	started   time.Time
	ended     time.Time
	bytes     int64
	files     int
	guests    int
	active    bool
	children  []recordingKey
}

type recordingTree struct {
	records    []state.Recording
	broadcasts []state.Broadcast
	roots      []recordingKey
	files      map[string]int
	groups     map[string]recordingGroup
	valid      bool
}

func (a *application) recordingGroups() *recordingTree {
	records := a.recordingRows()
	if a.recordTree.valid && slices.Equal(a.recordTree.records, records) && slices.Equal(a.recordTree.broadcasts, a.data.Broadcasts) {
		return &a.recordTree
	}
	a.recordTree = buildRecordingTree(records, a.data.Broadcasts)
	for key := range a.recordList.Open.All() {
		if _, exists := a.recordTree.groups[key.ID]; !key.Group || !exists {
			a.recordList.Open.Remove(key)
		}
	}
	return &a.recordTree
}

func buildRecordingTree(records []state.Recording, broadcasts []state.Broadcast) recordingTree {
	tree := recordingTree{records: records, broadcasts: slices.Clone(broadcasts), files: make(map[string]int, len(records)), groups: make(map[string]recordingGroup), valid: true}
	metadata := make(map[string]state.Broadcast, len(broadcasts))
	for _, b := range broadcasts {
		metadata[b.ID] = b
	}
	type guestKey struct {
		uid uint32
		id  string
	}
	guests := make(map[string]map[guestKey]bool)
	for i, r := range records {
		key := recordingKey{ID: r.ID}
		tree.files[r.ID] = i
		if r.BroadcastID == "" {
			tree.roots = append(tree.roots, key)
			continue
		}
		g, exists := tree.groups[r.BroadcastID]
		if !exists {
			g.broadcast = metadata[r.BroadcastID]
			g.broadcast.ID = r.BroadcastID
			if g.broadcast.StreamerID == "" {
				g.broadcast.StreamerID = r.StreamerID
			}
			if g.broadcast.Directory == "" && r.Path != "" {
				g.broadcast.Directory = filepath.Dir(r.Path)
			}
			tree.roots = append(tree.roots, recordingKey{ID: r.BroadcastID, Group: true})
			guests[r.BroadcastID] = make(map[guestKey]bool)
		}
		g.children = append(g.children, key)
		if r.ID == "replay:"+r.BroadcastID || r.Path != "" && r.Path == g.broadcast.ReplayPath {
			g.broadcast.ReplayPath = r.Path
		} else {
			if g.hostName == "" && r.Name != "" {
				g.hostName = r.Name
				if r.GuestName != "" {
					g.hostName = strings.TrimSuffix(g.hostName, " · "+r.GuestID+" ("+r.GuestName+")")
				}
			}
			g.files++
			g.bytes += r.Bytes
			g.active = g.active || r.EndedAt.IsZero()
			if g.started.IsZero() || r.StartedAt.Before(g.started) {
				g.started = r.StartedAt
			}
			if r.EndedAt.After(g.ended) {
				g.ended = r.EndedAt
			}
			if r.GuestUID != 0 {
				guests[r.BroadcastID][guestKey{uid: r.GuestUID}] = true
			} else if r.GuestID != "" {
				guests[r.BroadcastID][guestKey{id: r.GuestID}] = true
			}
		}
		tree.groups[r.BroadcastID] = g
	}
	for id, g := range tree.groups {
		if !g.broadcast.StartedAt.IsZero() {
			g.started = g.broadcast.StartedAt
		}
		if g.files == 0 { // Only the replay remains in history.
			r := tree.file(g.children[0])
			g.started, g.bytes = r.StartedAt, r.Bytes
		}
		g.guests = len(guests[id])
		slices.SortFunc(g.children, func(a, b recordingKey) int {
			left, right := tree.file(a), tree.file(b)
			leftReplay, rightReplay := left.Path == g.broadcast.ReplayPath, right.Path == g.broadcast.ReplayPath
			if leftReplay != rightReplay {
				if leftReplay {
					return -1
				}
				return 1
			}
			if order := left.StartedAt.Compare(right.StartedAt); order != 0 {
				return order
			}
			return cmp.Compare(a.ID, b.ID)
		})
		tree.groups[id] = g
	}
	slices.SortFunc(tree.roots, func(a, b recordingKey) int {
		if order := tree.started(b).Compare(tree.started(a)); order != 0 {
			return order
		}
		if a.Group != b.Group {
			if a.Group {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return tree
}

// A replay's history EndedAt is its export completion time, not a media
// timestamp. Show the saved capture span rather than the hours spent waiting
// for or processing an export. Never include a later capture in an older replay.
func (tree *recordingTree) replaySpan(id string) (time.Time, time.Time) {
	g := tree.groups[id]
	var start, end time.Time
	for _, key := range g.children {
		r := tree.file(key)
		if r.ID == "replay:"+id || r.Path != "" && r.Path == g.broadcast.ReplayPath || r.EndedAt.IsZero() ||
			!g.broadcast.ReplayThrough.IsZero() && r.EndedAt.After(g.broadcast.ReplayThrough) {
			continue
		}
		if start.IsZero() || r.StartedAt.Before(start) {
			start = r.StartedAt
		}
		if r.EndedAt.After(end) {
			end = r.EndedAt
		}
	}
	return start, end
}

func (tree *recordingTree) file(key recordingKey) state.Recording {
	return tree.records[tree.files[key.ID]]
}

func (tree *recordingTree) started(key recordingKey) time.Time {
	if key.Group {
		return tree.groups[key.ID].started
	}
	return tree.file(key).StartedAt
}

func (tree *recordingTree) children(key recordingKey) []recordingKey {
	if key.Group {
		return tree.groups[key.ID].children
	}
	return nil
}

func (g recordingGroup) name() string {
	if g.hostName != "" {
		return g.hostName
	}
	if g.broadcast.StreamerID != "" {
		return g.broadcast.StreamerID
	}
	return "Multi-guest broadcast"
}
