package app

import (
	"fmt"

	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func (a *application) recordingMenu(m *ui.Menu, r state.Recording, services ui.Services) {
	if m.Item("Open recording").Disabled(a.busy || r.Path == "" || r.EndedAt.IsZero()).Chosen() {
		a.work(func() error { return mygo.Shell.OpenPath(r.Path) })
	}
	if m.Item("Show in folder").Disabled(r.Path == "").Chosen() {
		a.reveal(r.Path)
	}
	if m.Item("Copy file path").Disabled(r.Path == "").Chosen() {
		services.WriteClipboard(r.Path)
	}
	if r.ProcessedPath != "" {
		m.Separator()
		if m.Item("Open processed copy").Disabled(a.busy).Chosen() {
			a.work(func() error { return mygo.Shell.OpenPath(r.ProcessedPath) })
		}
		if m.Item("Show processed copy in folder").Chosen() {
			a.reveal(r.ProcessedPath)
		}
		if m.Item("Copy processed file path").Chosen() {
			services.WriteClipboard(r.ProcessedPath)
		}
	}
	if r.BroadcastID != "" {
		data := a.store.Data()
		for _, broadcast := range data.Broadcasts {
			if broadcast.ID != r.BroadcastID {
				continue
			}
			m.Separator()
			if broadcast.ReplayPath != "" && broadcast.ReplayPath != r.Path {
				if m.Item("Open room replay").Disabled(a.busy).Chosen() {
					a.work(func() error { return mygo.Shell.OpenPath(broadcast.ReplayPath) })
				}
			}
			label := "Assemble room replay"
			if broadcast.ReplayPending {
				label = "Room replay is processing…"
			} else if broadcast.ReplayPath != "" {
				label = "Rebuild room replay"
			} else if broadcast.ReplayError != "" {
				label = "Retry room replay"
			}
			active := false
			for _, raw := range data.Recordings {
				active = active || raw.BroadcastID == broadcast.ID && raw.EndedAt.IsZero()
			}
			if m.Item(label).Disabled(a.busy || a.preview || a.engine == nil || broadcast.ReplayPending || active).Chosen() {
				a.work(func() error { return a.engine.QueueReplay(broadcast.ID) })
			}
			break
		}
	}
	m.Separator()
	if m.Item("Remove from history").Disabled(a.busy || a.preview || r.EndedAt.IsZero()).Chosen() {
		a.work(func() error {
			if err := a.store.RemoveRecording(r.ID); err != nil {
				return err
			}
			a.update(func() { a.toasts = append(a.toasts, "Recording removed from history. File kept.") })
			return nil
		})
	}
}

func (a *application) recordingHistoryDialog(c *ui.Context) {
	ui.Modal(c, &a.clearHistoryOpen, func() {
		ui.Text(c, "Clear recording history?").FontSize(22).FontWeight(600)
		ui.Text(c, "Remove all completed recordings from this list?\nFiles stay on disk. Active recordings stay in the list.").Width(400)
		ui.Row(c).Justify(ui.End).Gap(10).Children(func() {
			if ui.Button(c, "Cancel").Clicked() {
				a.clearHistoryOpen = false
			}
			if ui.PrimaryButton(c, "Clear history").Disabled(a.busy || a.preview).Clicked() {
				a.work(func() error {
					n, err := a.store.ClearRecordingHistory()
					if err != nil {
						return err
					}
					message := fmt.Sprintf("%d recordings removed from history. Files kept.", n)
					if n == 1 {
						message = "1 recording removed from history. File kept."
					}
					a.update(func() { a.toasts = append(a.toasts, message) })
					return nil
				})
				a.clearHistoryOpen = false
			}
		})
	})
}
