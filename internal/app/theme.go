package app

import (
	"bigo-live-recorder/internal/assets"
	"github.com/egoist/mygo/ui"
)

func palette(dark bool) *ui.Theme {
	t := ui.LightTheme()
	if dark {
		t = ui.DarkTheme()
		t.Background = ui.Hex("#14171d")
		t.Surface = ui.Hex("#1a1e25")
		t.SurfaceHover = ui.Hex("#252b35")
		t.SurfacePressed = ui.Hex("#263f76")
		t.Border = ui.Hex("#343942")
		t.Text = ui.Hex("#f0f3f9")
		t.TextMuted = ui.Hex("#929ba9")
		t.Accent = ui.Hex("#4f82f3")
		t.AccentHover = ui.Hex("#6494ff")
		t.AccentPressed = ui.Hex("#3c6edb")
		t.Danger = ui.Hex("#ff626c")
		t.Success = ui.Hex("#8bd36d")
		t.Warning = ui.Hex("#f0c55a")
	} else {
		t.Background = ui.Hex("#eef1f5")
		t.Surface = ui.Hex("#f9fafc")
		t.SurfaceHover = ui.Hex("#e4e9f1")
		t.SurfacePressed = ui.Hex("#d5e1fb")
		t.Border = ui.Hex("#cfd5de")
		t.Text = ui.Hex("#1b2130")
		t.TextMuted = ui.Hex("#677182")
		t.Accent = ui.Hex("#336bdc")
	}
	t.Radius = 6
	t.Spacing = 4
	t.FontSize = 13
	return t
}
func icon(name string) *ui.SVG {
	b, err := assets.Icons.ReadFile("icons/" + name + ".svg")
	if err != nil {
		return nil
	}
	return ui.MustParseSVG(b)
}

var navIcons = map[string]*ui.SVG{"streamers": icon("list-check"), "recordings": icon("folder"), "logs": icon("file-text"), "settings": icon("settings")}

var dotsIcon = icon("dots")
var connectionIcon = icon("connection")
