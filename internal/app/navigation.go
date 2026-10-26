package app

import "github.com/egoist/mygo/ui"

// Keep native sidebar navigation, focus rings and accessibility, but do not
// dim the selected page when keyboard focus moves into its content.
func navigationItem(c *ui.Context, id, label string, selected bool) {
	t := c.Theme()
	color := t.Accent
	item := ui.SidebarItem(c, id, nil, label).Padding(13, 12, 13, 36)
	if selected {
		item.Background(t.Accent).TextColor(t.AccentText)
		color = t.AccentText
	}
	item.Draw(func(p *ui.Painter, r ui.Rect) {
		p.Icon(navIcons[id], ui.Rect{X: r.X + 12, Y: r.Y + (r.H-16)/2, W: 16, H: 16}, color)
	})
}

func drawScrollFades(p *ui.Painter, r ui.Rect, scroll *ui.ScrollState, background ui.Color) {
	height := min(float32(32), r.H/2)
	if height <= 0 {
		return
	}
	width := max(0, r.W-12)
	if scroll.Y > 1 {
		p.FillGradient(ui.Rect{X: r.X, Y: r.Y, W: width, H: height}, ui.LinearGradient{From: background.Alpha(min(1, scroll.Y/height)), To: background.Alpha(0), Angle: 180}, 0)
	}
	if remaining := scroll.MaxY - scroll.Y; remaining > 1 {
		p.FillGradient(ui.Rect{X: r.X, Y: r.Y + r.H - height, W: width, H: height}, ui.LinearGradient{From: background.Alpha(0), To: background.Alpha(min(1, remaining/height)), Angle: 180}, 0)
	}
}
