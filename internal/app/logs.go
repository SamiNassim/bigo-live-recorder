package app

import (
	"bigo-live-recorder/internal/logging"
	"github.com/egoist/mygo/ui"
)

func logMenu(m *ui.Menu, entry logging.Entry, services ui.Services) {
	if m.Item("Copy message").Chosen() {
		services.WriteClipboard(entry.Message)
	}
	if m.Item("Copy diagnostic details").Disabled(entry.Details == "").Chosen() {
		services.WriteClipboard(entry.Message + "\n\n" + entry.Details)
	}
}
