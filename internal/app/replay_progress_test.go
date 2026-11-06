package app

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestReplayStageProgressFitsAndPreservesRecordingGroups(t *testing.T) {
	for _, width := range []int{1050, 1400} {
		for _, theme := range []string{"light", "dark"} {
			t.Run(fmt.Sprintf("%d/%s", width, theme), func(t *testing.T) {
				a, b := groupedRecordingFixture(t)
				a.cfg.Theme = theme
				if err := a.store.Update(func(d *state.Data) error { d.Broadcasts[0].ReplayPending = true; return nil }); err != nil {
					t.Fatal(err)
				}
				a.refresh()
				a.replayProgress = make(map[string]recording.ReplayProgress)
				tt := ui.NewTester(a.view, width, 650)
				root := &a.recordTree.roots[0]
				for _, p := range []recording.ReplayProgress{
					{Stage: "Queued", Percent: -1}, {Stage: "Preparing", Percent: -1},
					{Stage: "Rendering", Percent: 42}, {Stage: "Rendering", Percent: 43},
					{Stage: "Joining", Percent: 0}, {Stage: "Validating", Percent: 73},
					{Stage: "Saving", Percent: -1},
				} {
					previous := a.replayProgress
					a.replayProgress = map[string]recording.ReplayProgress{b.ID: p}
					if !a.visibleReplayProgressChanged(previous) {
						t.Fatal("visible replay progress did not request a refresh", p)
					}
					tt.Frame()
					label := p.Stage + "…"
					if p.Percent >= 0 {
						label = fmt.Sprintf("%s %d%%", p.Stage, p.Percent)
					}
					title, titleOK := tt.Find("Creating replay…")
					stage, stageOK := tt.Find(label)
					button, buttonOK := tt.Find("Replay")
					if !titleOK || !stageOK || !buttonOK || title.X+title.W > button.X || stage.X+stage.W > button.X || stage.Y <= title.Y {
						t.Fatal("replay progress missing or clipped", label, title, stage, button)
					}
					if &a.recordTree.roots[0] != root || a.recordList.Rows() != 2 {
						t.Fatal("progress rebuilt or expanded the recording groups")
					}
					if a.visibleReplayProgressChanged(a.replayProgress) {
						t.Fatal("unchanged progress requested another frame")
					}
				}
				previous := a.replayProgress
				a.replayProgress = map[string]recording.ReplayProgress{
					b.ID: previous[b.ID], "hidden-room": {Stage: "Rendering", Percent: 12},
				}
				if a.visibleReplayProgressChanged(previous) {
					t.Fatal("a hidden replay forced a redraw")
				}
				a.replayProgress[b.ID] = recording.ReplayProgress{Stage: "Rendering", Percent: 42}
				tt.Frame()
				if dir := os.Getenv("BIGO_UI_SNAPSHOTS"); dir != "" {
					if err := os.MkdirAll(dir, 0755); err != nil {
						t.Fatal(err)
					}
					f, err := os.Create(filepath.Join(dir, fmt.Sprintf("replay-progress-%d-%s.png", width, theme)))
					if err != nil {
						t.Fatal(err)
					}
					err = png.Encode(f, tt.Image())
					closeErr := f.Close()
					if err != nil || closeErr != nil {
						t.Fatal(err, closeErr)
					}
				}
				if err := a.store.Update(func(d *state.Data) error { d.Broadcasts[0].ReplayPending = false; return nil }); err != nil {
					t.Fatal(err)
				}
				a.refresh()
				tt.Frame()
				if tt.HasText("Creating replay…") || tt.HasText("Rendering 42%") || !tt.HasText("Replay ready") {
					t.Fatal("completed replay retained transient progress", tt.Texts())
				}
			})
		}
	}
}
