package main

import (
	"image"
	"image/color"
	"testing"
)

func TestCleanAlphaPreservesColorsAndEdge(t *testing.T) {
	im := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 3; y < 13; y++ {
		for x := 3; x < 13; x++ {
			im.SetNRGBA(x, y, color.NRGBA{R: 31, G: 87, B: 211, A: 252})
		}
	}
	im.SetNRGBA(2, 8, color.NRGBA{R: 31, G: 87, B: 211, A: 60})
	im.SetNRGBA(0, 0, color.NRGBA{R: 1, A: 1})
	im.SetNRGBA(15, 15, color.NRGBA{R: 10, A: 255})
	got, err := cleanAlpha(im)
	if err != nil {
		t.Fatal(err)
	}
	for y := range 16 {
		for x := range 16 {
			before, after := im.NRGBAAt(x, y), got.NRGBAAt(x, y)
			if before.R != after.R || before.G != after.G || before.B != after.B {
				t.Fatalf("RGB changed at (%d,%d)", x, y)
			}
		}
	}
	if got.NRGBAAt(8, 8).A != 255 || got.NRGBAAt(2, 8).A != 60 || got.NRGBAAt(0, 0).A != 0 || got.NRGBAAt(15, 15).A != 0 {
		t.Fatal("failed to separate opaque face, antialiased edge and stray pixels")
	}
}
