// Command icon-alpha removes accidental alpha haze from an existing app icon.
// It changes only alpha: the largest connected icon silhouette and its
// antialiased edge remain, while every original RGB value is preserved.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
)

func main() {
	output := flag.String("out", "", "output PNG path")
	flag.Parse()
	if flag.NArg() != 1 || *output == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/icon-alpha -out clean.png source.png")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if config.Width != config.Height || config.Width < 32 || config.Width > 4096 {
		return errors.New("input must be a square PNG between 32 and 4096 pixels")
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	clean, err := cleanAlpha(im)
	if err != nil {
		return err
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, clean); err != nil {
		return err
	}
	return os.WriteFile(target, encoded.Bytes(), 0644)
}

func cleanAlpha(source image.Image) (*image.NRGBA, error) {
	b := source.Bounds()
	w, h := b.Dx(), b.Dy()
	pixels := make([]color.NRGBA, w*h)
	for y := range h {
		for x := range w {
			pixels[y*w+x] = color.NRGBAModel.Convert(source.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
		}
	}
	// A few disconnected nontransparent pixels must not become part of the
	// icon's silhouette merely because a PNG encoder retained their alpha.
	visited := make([]bool, len(pixels))
	var queue, largest []int
	for start, pixel := range pixels {
		if visited[start] || pixel.A < 128 {
			continue
		}
		queue = append(queue[:0], start)
		visited[start] = true
		for head := 0; head < len(queue); head++ {
			at := queue[head]
			x, y := at%w, at/w
			for yy := max(y-1, 0); yy <= min(y+1, h-1); yy++ {
				for xx := max(x-1, 0); xx <= min(x+1, w-1); xx++ {
					next := yy*w + xx
					if !visited[next] && pixels[next].A >= 128 {
						visited[next] = true
						queue = append(queue, next)
					}
				}
			}
		}
		if len(queue) > len(largest) {
			largest = append(largest[:0], queue...)
		}
	}
	if len(largest) == 0 {
		return nil, errors.New("input has no opaque icon silhouette")
	}
	mask := make([]bool, len(pixels))
	for _, at := range largest {
		mask[at] = true
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			at := y*w + x
			pixel := pixels[at]
			inside, adjacent := mask[at], false
			for yy := y - 1; yy <= y+1; yy++ {
				for xx := x - 1; xx <= x+1; xx++ {
					neighbor := xx >= 0 && xx < w && yy >= 0 && yy < h && mask[yy*w+xx]
					inside = inside && neighbor
					adjacent = adjacent || neighbor
				}
			}
			switch {
			case inside || mask[at] && pixel.A >= 240:
				pixel.A = 255
			case adjacent:
				// Preserve partial coverage only at the actual silhouette edge.
			default:
				pixel.A = 0
			}
			out.SetNRGBA(x, y, pixel)
		}
	}
	return out, nil
}
