package recording

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReplayLabelsPreserveColorEmoji(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("system color emoji font availability depends on the Linux installation")
	}
	cases := []struct{ name, text string }{{"joined skin tone", "👩🏽‍💻"}, {"pictograph", "🌈"}}
	if runtime.GOOS == "darwin" {
		cases = append(cases, struct{ name, text string }{"regional indicator flag", "🇫🇷"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "label.png")
			if err := replayLabel(path, "guest.example", tc.text, 300); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			img, err := png.Decode(f)
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds() != image.Rect(0, 0, 300, 48) {
				t.Fatal("label dimensions changed", img.Bounds())
			}
			colored := 0
			for y := 24; y < 47; y++ {
				for x := 12; x < 288; x++ {
					r, g, b, a := img.At(x, y).RGBA()
					if a > 0x8000 && max(r, g, b)-min(r, g, b) > 0x3000 {
						colored++
					}
				}
			}
			if colored < 20 {
				t.Fatal("emoji was missing, monochrome, or clipped", colored)
			}
		})
	}
}
