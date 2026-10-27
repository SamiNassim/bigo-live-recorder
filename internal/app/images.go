package app

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"math"

	"github.com/egoist/mygo/ui"
)

const logoPixels = 256
const avatarPixels = 128

func decodeThumbnail(data []byte, limit int) (*ui.Bitmap, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 2048 || cfg.Height > 2048 {
		return nil, errors.New("image dimensions exceed the thumbnail limit")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return ui.NewBitmap(resizeThumbnail(img, limit)), nil
}

// resizeThumbnail averages source pixel coverage in premultiplied RGBA, keeping
// transparent edges smooth without retaining a full-size decoded image.
func resizeThumbnail(src image.Image, limit int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if max(w, h) <= limit || limit <= 0 {
		return src
	}
	dw, dh := max(1, w*limit/max(w, h)), max(1, h*limit/max(w, h))
	pixels, ok := src.(image.RGBA64Image)
	if !ok {
		copy := image.NewRGBA(b)
		draw.Draw(copy, b, src, b.Min, draw.Src)
		pixels = copy
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sx, sy := float64(w)/float64(dw), float64(h)/float64(dh)
	for y := range dh {
		y0, y1 := float64(y)*sy, float64(y+1)*sy
		for x := range dw {
			x0, x1 := float64(x)*sx, float64(x+1)*sx
			var r, g, blue, a float64
			for iy := int(y0); iy < min(h, int(math.Ceil(y1))); iy++ {
				wy := min(y1, float64(iy+1)) - max(y0, float64(iy))
				for ix := int(x0); ix < min(w, int(math.Ceil(x1))); ix++ {
					weight := wy * (min(x1, float64(ix+1)) - max(x0, float64(ix)))
					c := pixels.RGBA64At(b.Min.X+ix, b.Min.Y+iy)
					r += float64(c.R) * weight
					g += float64(c.G) * weight
					blue += float64(c.B) * weight
					a += float64(c.A) * weight
				}
			}
			area := (x1 - x0) * (y1 - y0) * 257
			i := out.PixOffset(x, y)
			for channel, value := range [4]float64{r, g, blue, a} {
				out.Pix[i+channel] = uint8(min(255, math.Round(value/area)))
			}
		}
	}
	return out
}
