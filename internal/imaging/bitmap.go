// Package imaging holds the raster helpers drivers share: placing a decoded page on a sheet at
// the printer's resolution, gray to 1-bit conversion (threshold and Floyd–Steinberg), a packed
// bitmap, and PackBits. The Brother QL pipeline keeps its own, Python-parity code in
// internal/raster and does not use this package.
package imaging

import (
	"image"
	"image/color"
	"math/bits"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// Bitmap is a packed 1-bit image: rows of Stride bytes, most significant bit first, 1 = black
// (ink). Bits past W in the last byte of a row are always 0.
type Bitmap struct {
	W, H   int
	Stride int
	Pix    []byte
}

func NewBitmap(w, h int) *Bitmap {
	stride := (w + 7) / 8
	return &Bitmap{W: w, H: h, Stride: stride, Pix: make([]byte, stride*h)}
}

// Row returns row y's packed bytes; changing them changes the bitmap.
func (b *Bitmap) Row(y int) []byte { return b.Pix[y*b.Stride : (y+1)*b.Stride] }

func (b *Bitmap) At(x, y int) bool {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return false
	}
	return b.Pix[y*b.Stride+x/8]&(0x80>>(x%8)) != 0
}

func (b *Bitmap) Set(x, y int, black bool) {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return
	}
	i, m := y*b.Stride+x/8, byte(0x80>>(x%8))
	if black {
		b.Pix[i] |= m
	} else {
		b.Pix[i] &^= m
	}
}

// RowBlank reports whether row y has no ink.
func (b *Bitmap) RowBlank(y int) bool {
	for _, v := range b.Row(y) {
		if v != 0 {
			return false
		}
	}
	return true
}

// Blank reports whether the bitmap has no ink.
func (b *Bitmap) Blank() bool {
	for _, v := range b.Pix {
		if v != 0 {
			return false
		}
	}
	return true
}

// InkRows returns the first row with ink and one past the last; ok is false for a blank bitmap.
func (b *Bitmap) InkRows() (top, bottom int, ok bool) {
	top = 0
	for top < b.H && b.RowBlank(top) {
		top++
	}
	if top == b.H {
		return 0, 0, false
	}
	bottom = b.H
	for b.RowBlank(bottom - 1) {
		bottom--
	}
	return top, bottom, true
}

// Rows returns a copy of rows [top, bottom).
func (b *Bitmap) Rows(top, bottom int) *Bitmap {
	top, bottom = max(top, 0), min(bottom, b.H)
	out := NewBitmap(b.W, max(bottom-top, 0))
	copy(out.Pix, b.Pix[top*b.Stride:max(bottom, top)*b.Stride])
	return out
}

// Ink counts black dots.
func (b *Bitmap) Ink() int {
	n := 0
	for _, v := range b.Pix {
		n += bits.OnesCount8(v)
	}
	return n
}

// Invert flips every dot, keeping the padding bits 0. PostScript's 1-bit image, for one, takes
// 1 as white.
func (b *Bitmap) Invert() {
	for i := range b.Pix {
		b.Pix[i] = ^b.Pix[i]
	}
	if r := b.W % 8; r != 0 {
		mask := byte(0xff << (8 - r))
		for y := 0; y < b.H; y++ {
			b.Pix[y*b.Stride+b.Stride-1] &= mask
		}
	}
}

// Image is a black-on-white view for debug PNGs.
func (b *Bitmap) Image() image.Image {
	img := image.NewPaletted(image.Rect(0, 0, b.W, b.H), color.Palette{color.White, color.Black})
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			if b.At(x, y) {
				img.Pix[y*img.Stride+x] = 1
			}
		}
	}
	return img
}

// GrayImage is a view of a page for debug PNGs.
func GrayImage(p raster.Page) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, p.Width, p.Height))
	for y := 0; y < p.Height; y++ {
		copy(img.Pix[y*img.Stride:y*img.Stride+p.Width], p.Gray[y*p.Width:(y+1)*p.Width])
	}
	return img
}

// FromImage turns an image into a gray page at dpi: (r+g+b)/3, white where transparent.
func FromImage(img image.Image, dpi int) raster.Page {
	r := img.Bounds()
	p := raster.Page{Width: r.Dx(), Height: r.Dy(), XDPI: dpi, YDPI: dpi, Gray: make([]byte, r.Dx()*r.Dy())}
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			cr, cg, cb, ca := img.At(r.Min.X+x, r.Min.Y+y).RGBA()
			// RGBA is alpha-premultiplied; composite on white.
			w := 0xffff - ca
			g := (cr + w + cg + w + cb + w) / 3
			p.Gray[y*p.Width+x] = byte(g >> 8)
		}
	}
	return p
}
