package recording

import (
	"image/png"
	"os"

	"github.com/egoist/mygo/ui"
)

// Render labels with the interface's native text stack. System font fallback
// preserves emoji, joined sequences and scripts beyond the default font, without
// depending on FFmpeg's optional drawtext filter or bundling another font.
func replayLabel(path, id, name string, width int) error {
	if id == "" {
		id = "Unidentified guest"
	}
	if name == id {
		name = ""
	}
	canvas := ui.Render(func(c *ui.Context) {
		ui.Column(c).Width(float32(width)).Height(48).Padding(2, 12).Gap(0).Clip().Background(ui.RGBA(10, 12, 16, 245)).Children(func() {
			for _, line := range []string{id, name} {
				ui.Text(c, line).FontSize(18).FixedLineHeight(22).Height(22).Width(float32(max(0, width-24))).SingleLine().TextColor(ui.RGB(255, 255, 255))
			}
		})
	}, width, 48, 1)
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	err = png.Encode(out, canvas)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
