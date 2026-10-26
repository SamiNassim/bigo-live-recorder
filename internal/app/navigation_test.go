package app

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestSidebarSelectionDoesNotDimWhenContentTakesFocus(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		a := accountFixture(t)
		a.cfg.Theme = theme
		tt := ui.NewTester(a.view, 1050, 650)
		if err := tt.Click("Streamers"); err != nil {
			t.Fatal(err)
		}
		label, ok := tt.Find("Streamers")
		if !ok {
			t.Fatal("sidebar item missing")
		}
		x, y := int(label.X+2), int(label.Y-5)
		before := tt.Image().RGBAAt(x, y)
		if err := tt.Click("Search streamers"); err != nil {
			t.Fatal(err)
		}
		after := tt.Image().RGBAAt(x, y)
		if a.page != "streamers" || before != after {
			t.Fatalf("selected page dimmed after focus moved (%s): before=%v after=%v", theme, before, after)
		}
	}
}

func TestScrollFadesIndicateBothAvailableDirections(t *testing.T) {
	var scroll ui.ScrollState
	background := ui.Hex("#101010")
	tt := ui.NewTester(func(c *ui.Context) {
		ui.Scroll(c).Fill().TrackScroll(&scroll).DrawOver(func(p *ui.Painter, r ui.Rect) { drawScrollFades(p, r, &scroll, background) }).Children(func() {
			ui.Box(c).Height(1000).Shrink(0).FillWidth().Background(ui.Hex("#ff0000"))
		})
	}, 200, 160)
	check := func(top, bottom bool) {
		t.Helper()
		frame := tt.Image()
		if got := frame.RGBAAt(20, 2).R < 150; got != top {
			t.Fatal("wrong top fade", got, top, scroll.Y)
		}
		if got := frame.RGBAAt(20, 157).R < 150; got != bottom {
			t.Fatal("wrong bottom fade", got, bottom, scroll.Y)
		}
	}
	check(false, true)
	scroll.Y = 300
	tt.Frame()
	check(true, true)
	scroll.Y = scroll.MaxY
	tt.Frame()
	check(true, false)
}
