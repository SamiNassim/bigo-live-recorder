package assets

import (
	"bytes"
	"image/color"
	"image/png"
	"testing"
)

// A PNG can advertise alpha while carrying a faint rectangular matte. macOS
// can then present it as artwork inside an additional system icon tile.
func TestIconHasCleanTransparency(t *testing.T) {
	im, err := png.Decode(bytes.NewReader(Icon))
	if err != nil {
		t.Fatal(err)
	}
	b := im.Bounds()
	if b.Dx() != b.Dy() {
		t.Fatalf("icon is not square: %v", b)
	}
	margin := b.Dx() / 40
	var face, opaque int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(im.At(x, y)).(color.NRGBA)
			if (x < margin || y < margin || x >= b.Max.X-margin || y >= b.Max.Y-margin) && c.A != 0 {
				t.Fatalf("stray alpha in transparent outer margin at (%d,%d): %d", x, y, c.A)
			}
			if x > b.Dx()/4 && x < 3*b.Dx()/4 && y > b.Dy()/4 && y < 3*b.Dy()/4 {
				face++
				if c.A == 255 {
					opaque++
				}
			}
		}
	}
	if opaque != face {
		t.Fatalf("icon face must be opaque, not a translucent matte: %d/%d pixels opaque", opaque, face)
	}
}
