package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"

	"github.com/leandroalbero/ql-airprint/internal/ql"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func isPNG(b []byte) bool { return bytes.HasPrefix(b, pngMagic) }

// writePNG stores a label as a 1-bit PNG (black on white).
func writePNG(path string, b *ql.Bitmap) error {
	img := image.NewPaletted(image.Rect(0, 0, b.W, b.H), color.Palette{color.White, color.Black})
	for y := 0; y < b.H; y++ {
		row := b.Bits[y*b.W : (y+1)*b.W]
		for x, black := range row {
			if black {
				img.Pix[y*img.Stride+x] = 1
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// readPNG thresholds a PNG to 1 bit: black when (r+g+b)/3 < 0x8000.
func readPNG(data []byte) (*ql.Bitmap, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("png: %w", err)
	}
	r := img.Bounds()
	b := ql.NewBitmap(r.Dx(), r.Dy())
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			cr, cg, cb, _ := img.At(r.Min.X+x, r.Min.Y+y).RGBA()
			if (cr+cg+cb)/3 < 0x8000 {
				b.Bits[y*b.W+x] = true
			}
		}
	}
	return b, nil
}
