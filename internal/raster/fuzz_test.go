package raster

import (
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

func FuzzDecode(f *testing.F) {
	text := textGray(1, 64, 16, 1, 1, 0, true)
	f.Add(testutil.EncodeURF(testutil.GrayPage(64, 16, 300, text)))
	f.Add(testutil.EncodeURFCount(0, testutil.GrayPage(3, 2, 600, []byte{0, 255, 9, 128, 7, 255})))
	f.Add(testutil.EncodeURF(colourPage(2, 64, 16, 600, 32, testutil.URFCMYK, true, text)))
	f.Add(testutil.EncodePWG(colourPage(3, 64, 16, 300, 24, testutil.PWGRGB, false, text)))
	f.Add(testutil.EncodePWG(testutil.Page{Width: 2, Height: 1, XDPI: 600, BPP: 8, ColorSpace: testutil.PWGBlack, Pix: []byte{1, 2}}))
	f.Add([]byte("UNIRAST\x00"))
	f.Add([]byte("RaS2"))

	lenient := []Options{
		{Pins: 8, WidthMin: 0, WidthMax: 1 << 30, TrimMax: 2, Dither: "auto"},
		{Pins: 64, WidthMin: 0, WidthMax: 1 << 30, TrimMax: -1, Dither: "on", HighRes: true},
		{Pins: 1, WidthMin: 0, WidthMax: 1 << 30, TrimMax: 0, Dither: "off"},
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		pages, err := decode(data, Limits{MaxGrayBytes: 1 << 20})
		if err != nil {
			return
		}
		for _, p := range pages {
			if len(p.Gray) != p.Width*p.Height {
				t.Fatalf("%dx%d page with %d gray bytes", p.Width, p.Height, len(p.Gray))
			}
			for _, dpi := range []int{300, 600} {
				p.XDPI = dpi
				for _, o := range lenient {
					l, err := Prepare(p, o)
					if err != nil {
						t.Fatal(err)
					}
					if l.Bitmap.W != o.Pins || len(l.Bitmap.Bits) != l.Bitmap.W*l.Bitmap.H {
						t.Fatalf("bitmap %dx%d with %d bits", l.Bitmap.W, l.Bitmap.H, len(l.Bitmap.Bits))
					}
				}
			}
		}
	})
}
