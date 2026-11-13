package app

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func (a *application) visibleReplayProgressChanged(previous map[string]recording.ReplayProgress) bool {
	first, last := a.recordList.List.Visible()
	for row := max(0, first); row <= min(last, a.recordList.Rows()-1); row++ {
		key := a.recordList.Item(row)
		g := a.recordTree.groups[key.ID]
		if key.Group && g.broadcast.ReplayPending && !g.active && previous[key.ID] != a.replayProgress[key.ID] {
			return true
		}
	}
	return false
}

func (a *application) recordings(c *ui.Context) {
	t := c.Theme()
	hasCompleted := slices.ContainsFunc(a.data.Recordings, func(r state.Recording) bool { return !r.EndedAt.IsZero() })
	header(c, "Recordings", "Expand a recording group to see its clips or guest files.", func() {
		if ui.Button(c, "Clear history…").Disabled(a.busy || a.preview || !hasCompleted).Clicked() {
			a.clearHistoryOpen = true
		}
		if ui.PrimaryButton(c, "Open recordings folder").Clicked() {
			a.openFolder(a.cfg.RecordingPath)
		}
	})
	tree := a.recordingGroups()
	a.recordList.List.Selected = &a.recordSelection
	a.recordList.List.Label = func(row int) string {
		key := a.recordList.Item(row)
		if key.Group {
			group := tree.groups[key.ID]
			return group.kind() + " · " + group.name()
		}
		name, _ := tree.recordingTitle(key)
		return name
	}
	services := c.Services()
	a.recordList.List.CellMenu = func(m *ui.Menu, row, _ int) {
		key := a.recordList.Item(row)
		if key.Group {
			a.recordingGroupMenu(m, key, tree.groups[key.ID], services)
		} else {
			a.recordingMenu(m, tree.file(key), services)
		}
	}
	ui.Column(c).Grow(1).MinHeight(0).FillWidth().Background(t.Surface).Border(1, t.Border).Radius(8).Children(func() {
		ui.OutlineTable(c, &a.recordList, []ui.TableColumn{{Title: "Streamer", Width: 200}, {Title: "Started", Width: 150}, {Title: "Duration", Width: 90}, {Title: "Size", Width: 85}, {Title: "File", MinWidth: 140}, {Title: "", ID: "open", Width: 100, MinWidth: 100}}, tree.roots, tree.children, func(key recordingKey, col int) {
			if key.Group {
				a.recordingGroupCell(c, key, tree.groups[key.ID], col)
				return
			}
			r := tree.file(key)
			switch col {
			case 0:
				name, detail := tree.recordingTitle(key)
				if r.ID == "replay:"+r.BroadcastID && tree.groups[r.BroadcastID].replayIsPartial() {
					detail = "Saved replay · earlier capture"
				}
				ui.Column(c).Grow(1).MinWidth(0).Gap(3).Padding(7, 0).Children(func() {
					ui.Text(c, name).FontWeight(600).SingleLine().Tooltip(name)
					if detail != "" {
						ui.Text(c, detail).FontSize(11).TextColor(t.TextMuted).SingleLine().Tooltip(detail)
					}
				})
			case 1:
				ui.Text(c, timestamp(r.StartedAt)).FontSize(11)
			case 2:
				if r.ID == "replay:"+r.BroadcastID {
					start, end := tree.replaySpan(r.BroadcastID)
					if end.IsZero() {
						ui.Text(c, "—").FontSize(11)
					} else {
						a.recordingDuration(c, key, start, end)
					}
				} else {
					a.recordingDuration(c, key, r.StartedAt, r.EndedAt)
				}
			case 3:
				label := bytesLabel(r.Bytes)
				if r.EndedAt.IsZero() {
					label = "Recording…"
				}
				ui.Text(c, label).FontSize(11)
			case 4:
				file := filepath.Base(r.Path)
				if r.Path == "" {
					file = "—"
				}
				ui.Text(c, file).FontSize(11).SingleLine().Tooltip(r.Path + "\n" + r.ProcessedPath + "\n" + r.Error)
			case 5:
				if ui.Button(c, "Show").Clicked() {
					a.reveal(r.Path)
				}
			}
		}).Grow(1).MinHeight(0).FillWidth()
		label := fmt.Sprintf("%d files in history", len(a.data.Recordings))
		if len(tree.groups) > 0 {
			kind := "recording groups"
			if len(tree.groups) == 1 {
				kind = "recording group"
			}
			label += fmt.Sprintf(" · %d %s", len(tree.groups), kind)
		}
		ui.Text(c, label+" · clearing history never deletes files").FontSize(11).TextColor(t.TextMuted).Padding(14, 18)
	})
}

func (a *application) recordingGroupCell(c *ui.Context, key recordingKey, g recordingGroup, col int) {
	t := c.Theme()
	switch col {
	case 0:
		label := ui.Column(c).Grow(1).MinWidth(0).Gap(3).Padding(7, 0).Children(func() {
			ui.Text(c, g.name()).FontWeight(600).SingleLine().Tooltip(g.name() + "\nBIGO ID: " + g.broadcast.StreamerID)
			ui.Text(c, g.kind()).FontSize(11).TextColor(t.TextMuted)
		})
		if label.RowDoubleClicked() {
			a.toggleRecordingGroup(key)
		}
	case 1:
		ui.Text(c, timestamp(g.started)).FontSize(11)
	case 2:
		if g.files == 0 {
			ui.Text(c, "—").FontSize(11)
		} else {
			ended := g.ended
			if g.active {
				ended = time.Time{}
			}
			a.recordingDuration(c, key, g.started, ended)
		}
	case 3:
		hint := "Total size of original guest files; the replay is separate."
		if g.broadcast.Segmented {
			hint = "Total size of the clips in history."
		} else if g.files == 0 {
			hint = "Replay file size."
		}
		ui.Text(c, bytesLabel(g.bytes)).FontSize(11).Tooltip(hint)
	case 4:
		ui.Column(c).Grow(1).MinWidth(0).Gap(3).Padding(7, 0).Children(func() {
			detail := fmt.Sprintf("%d guest files", g.files)
			if g.broadcast.Segmented {
				detail = fmt.Sprintf("%d clips", g.files)
				if g.files == 1 {
					detail = "1 clip"
				}
			} else if g.guests > 0 {
				detail += fmt.Sprintf(" · %d guests", g.guests)
			}
			if g.files == 0 {
				detail = "Replay only"
			}
			status, color := "Captured", t.TextMuted
			progressHint := ""
			switch {
			case g.active:
				status, color = "Recording…", t.Success
				if g.broadcast.ReplayPath != "" {
					status = "Recording · saved replay"
				}
			case g.broadcast.ReplayPending:
				progressHint = detail + "\nProgress is for the current stage."
				detail = "Creating replay…"
				status, color = "Queued…", t.Warning
				if p, ok := a.replayProgress[g.broadcast.ID]; ok {
					status = p.Stage + "…"
					if p.Percent >= 0 {
						status = fmt.Sprintf("%s %d%%", p.Stage, p.Percent)
					}
				}
			case g.broadcast.ReplayError != "":
				status, color = "Replay needs attention", t.Warning
			case g.replayIsPartial():
				status, color = "Earlier replay saved", t.TextMuted
			case g.broadcast.ReplayPath != "":
				status, color = "Replay ready", t.Success
			}
			hint := g.broadcast.ReplayError
			if progressHint != "" {
				hint = progressHint
			}
			if hint == "" && g.broadcast.ReplayPath != "" {
				hint = g.replayHint()
			}
			ui.Text(c, detail).FontSize(12).SingleLine().Tooltip(hint)
			ui.Text(c, status).FontSize(11).TextColor(color).SingleLine().Tooltip(hint)
		})
	case 5:
		if g.broadcast.ReplayPath != "" {
			if ui.Button(c, "Replay").Disabled(a.busy).Tooltip(g.replayHint()).Clicked() {
				a.work(func() error { return mygo.Shell.OpenPath(g.broadcast.ReplayPath) })
			}
		} else if ui.Button(c, "Folder").Disabled(g.broadcast.Directory == "").Clicked() {
			a.openFolder(g.broadcast.Directory)
		}
	}
}

func (a *application) recordingDuration(c *ui.Context, key recordingKey, started, ended time.Time) {
	d := c.Now().Sub(started)
	if !ended.IsZero() {
		d = ended.Sub(started)
	} else {
		first, last := a.recordList.List.Visible()
		for row := max(0, first); row <= last && row < a.recordList.Rows(); row++ {
			if a.recordList.Item(row) == key {
				refreshClock(c, d, false)
				break
			}
		}
	}
	ui.Text(c, duration(d)).FontSize(11)
}

func (tree *recordingTree) recordingTitle(key recordingKey) (string, string) {
	r := tree.file(key)
	if clip := tree.clips[key.ID]; clip > 0 {
		return fmt.Sprintf("Clip %d", clip), ""
	}
	if r.BroadcastID == "" {
		return r.Name, ""
	}
	if r.ID == "replay:"+r.BroadcastID {
		return "Room replay", "Combined video"
	}
	name := r.GuestName
	if name == "" {
		name = r.GuestID
	}
	if name == "" {
		name = "Unidentified guest"
	}
	detail := "Video"
	if strings.EqualFold(filepath.Ext(r.Path), ".mka") {
		detail = "Audio"
	}
	if r.GuestID != "" && r.GuestID != name {
		detail += " · " + r.GuestID
	}
	return name, detail
}

func (a *application) toggleRecordingGroup(key recordingKey) {
	if a.recordList.Open.Has(key) {
		a.recordList.Open.Remove(key)
	} else {
		a.recordList.Open.Add(key)
	}
}
