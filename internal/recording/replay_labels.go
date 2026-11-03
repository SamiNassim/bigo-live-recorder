package recording

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Labels use the same compiled Go image/font libraries as the interface. The
// viewing copy requires neither FFmpeg's optional drawtext filter nor a font
// installation on the recording machine.
func replayLabel(path, id, name string, width int) error {
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 18, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return err
	}
	defer face.Close()
	canvas := image.NewRGBA(image.Rect(0, 0, width, 48))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{10, 12, 16, 245}), image.Point{}, draw.Src)
	if id == "" {
		id = "Unidentified guest"
	}
	if name == id {
		name = ""
	}
	d := font.Drawer{Dst: canvas, Src: image.White, Face: face}
	for i, line := range []string{id, name} {
		runes := []rune(line)
		for len(runes) > 0 && d.MeasureString(string(runes)).Ceil() > width-24 {
			runes = runes[:len(runes)-1]
		}
		if len(runes) < len([]rune(line)) && len(runes) > 0 {
			runes[len(runes)-1] = '…'
		}
		d.Dot = fixed.P(12, 20+i*22)
		d.DrawString(string(runes))
	}
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
