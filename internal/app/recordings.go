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
