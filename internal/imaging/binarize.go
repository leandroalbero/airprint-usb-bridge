package imaging

import (
	"fmt"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

const (
	DitherAuto = "auto"
	DitherOn   = "on"
	DitherOff  = "off"
)

// Threshold makes a dot black where gray < level (128 is the usual level).
func Threshold(p raster.Page, level byte) *Bitmap {
	b := NewBitmap(p.Width, p.Height)
	for y := 0; y < p.Height; y++ {
		row := p.Gray[y*p.Width : (y+1)*p.Width]
		out := b.Row(y)
		for x, g := range row {
			if g < level {
				out[x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	return b
}

// FloydSteinberg diffuses the error of a 128 threshold to the right and the row below. Near-white
// (>= 240) and near-black (<= 15) are clamped first so paper stays clean and solids stay solid.
func FloydSteinberg(p raster.Page) *Bitmap {
	w, h := p.Width, p.Height
	b := NewBitmap(w, h)
	cur := make([]int32, w+2)
	next := make([]int32, w+2)
	for y := 0; y < h; y++ {
		clear(next)
		row := p.Gray[y*w : (y+1)*w]
		out := b.Row(y)
		// Errors are kept in sixteenths to stay in integers.
		for x := 0; x < w; x++ {
			g := int32(row[x])
			if g >= 240 {
				g = 255
			} else if g <= 15 {
				g = 0
			}
			v := g*16 + cur[x+1]
			var e int32
			if v < 128*16 {
				out[x/8] |= 0x80 >> (x % 8)
				e = v
			} else {
				e = v - 255*16
			}
			cur[x+2] += e * 7 / 16
			next[x] += e * 3 / 16
			next[x+1] += e * 5 / 16
			next[x+2] += e / 16
		}
		cur, next = next, cur
	}
	return b
}

// LooksLikePhoto samples the page and reports whether more than 5% of it is mid-grey (48–207):
// text and barcodes stay sharp under a threshold, photos need dithering.
func LooksLikePhoto(p raster.Page) bool {
	step := max(1, p.Height/200)
	total, mid := 0, 0
	for y := 0; y < p.Height; y += step {
		for x := 0; x < p.Width; x += 4 {
			v := p.Gray[y*p.Width+x]
			total++
			if v >= 48 && v <= 207 {
				mid++
			}
		}
	}
	return total > 0 && float64(mid)/float64(total) > 0.05
}

// Binarize converts a page with mode "auto" (dither photos, threshold the rest), "on" (always
// dither) or "off" (threshold at 128); "" means auto. dithered reports which was used.
func Binarize(p raster.Page, mode string) (b *Bitmap, dithered bool, err error) {
	if p.Width <= 0 || p.Height <= 0 || len(p.Gray) != p.Width*p.Height {
		return nil, false, fmt.Errorf("imaging: malformed page %dx%d with %d gray bytes", p.Width, p.Height, len(p.Gray))
	}
	switch mode {
	case "", DitherAuto:
		dithered = LooksLikePhoto(p)
	case DitherOn:
		dithered = true
	case DitherOff:
	default:
		return nil, false, fmt.Errorf("imaging: dither mode %q (want auto, on or off)", mode)
	}
	if dithered {
		return FloydSteinberg(p), true, nil
	}
	return Threshold(p, 128), false, nil
}
