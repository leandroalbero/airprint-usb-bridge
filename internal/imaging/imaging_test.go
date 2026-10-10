package imaging

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"math/rand"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func TestPackBitsAppleExample(t *testing.T) {
	// Apple Technical Note TN1023.
	unpacked := []byte{0xAA, 0xAA, 0xAA, 0x80, 0x00, 0x2A, 0xAA, 0xAA, 0xAA, 0xAA, 0x80, 0x00, 0x2A, 0x22,
		0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}
	packed := []byte{0xFE, 0xAA, 0x02, 0x80, 0x00, 0x2A, 0xFD, 0xAA, 0x03, 0x80, 0x00, 0x2A, 0x22, 0xF7, 0xAA}
	if got := PackBits(nil, unpacked); !bytes.Equal(got, packed) {
		t.Fatalf("PackBits = % X\nwant       % X", got, packed)
	}
	if got, err := UnpackBits(packed, len(unpacked)); err != nil || !bytes.Equal(got, unpacked) {
		t.Fatalf("UnpackBits = % X, %v", got, err)
	}
}

func TestPackBitsRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 600; n += 7 {
		src := make([]byte, n)
		for i := range src {
			if r.Intn(3) == 0 {
				src[i] = byte(r.Intn(256))
			} else if i > 0 {
				src[i] = src[i-1]
			}
		}
		p := PackBits([]byte{0x55}, src)
		if p[0] != 0x55 {
			t.Fatal("dst prefix lost")
		}
		got, err := UnpackBits(p[1:], 0)
		if err != nil || !bytes.Equal(got, src) {
			t.Fatalf("n=%d round trip: %v", n, err)
		}
		if len(p)-1 > n+(n+127)/128 {
			t.Fatalf("n=%d packed to %d bytes", n, len(p)-1)
		}
	}
	long := bytes.Repeat([]byte{7}, 300)
	if got := PackBits(nil, long); !bytes.Equal(got, []byte{0x81, 7, 0x81, 7, 0xD5, 7}) {
		t.Fatalf("long run % X", got)
	}
	if _, err := UnpackBits([]byte{0x05, 1, 2}, 0); !errors.Is(err, ErrPackBits) {
		t.Fatalf("short literal: %v", err)
	}
	if _, err := UnpackBits([]byte{0xFE}, 0); !errors.Is(err, ErrPackBits) {
		t.Fatalf("short repeat: %v", err)
	}
	if got, err := UnpackBits([]byte{0x80, 0x00, 9}, 0); err != nil || !bytes.Equal(got, []byte{9}) {
		t.Fatalf("no-op header: % X %v", got, err)
	}
}

func page(w, h, dpi int, f func(x, y int) byte) raster.Page {
	p := raster.Page{Width: w, Height: h, XDPI: dpi, YDPI: dpi, Gray: make([]byte, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p.Gray[y*w+x] = f(x, y)
		}
	}
	return p
}

func TestBitmap(t *testing.T) {
	b := NewBitmap(10, 4)
	if b.Stride != 2 || !b.Blank() {
		t.Fatalf("%+v", b)
	}
	b.Set(9, 2, true)
	b.Set(0, 1, true)
	b.Set(10, 0, true)
	if !b.At(9, 2) || b.At(8, 2) || b.Row(2)[1] != 0x40 || b.Ink() != 2 {
		t.Fatalf("pix % X", b.Pix)
	}
	if top, bottom, ok := b.InkRows(); !ok || top != 1 || bottom != 3 {
		t.Fatalf("ink rows %d %d %v", top, bottom, ok)
	}
	if r := b.Rows(1, 3); r.H != 2 || !r.At(0, 0) || !r.At(9, 1) {
		t.Fatalf("rows %+v", r)
	}
	b.Invert()
	if b.At(9, 2) || !b.At(8, 2) || b.Row(0)[1] != 0xC0 || b.Ink() != 38 {
		t.Fatalf("invert % X", b.Pix)
	}
	img := b.Image()
	if img.Bounds().Dx() != 10 || img.At(9, 2) != color.White {
		t.Fatal("image")
	}
}

func TestBinarize(t *testing.T) {
	text := page(64, 32, 300, func(x, y int) byte {
		if x < 32 {
			return 0
		}
		return 255
	})
	b, dithered, err := Binarize(text, "")
	if err != nil || dithered || b.Ink() != 32*32 || !b.At(31, 5) || b.At(32, 5) {
		t.Fatalf("text: dithered %v ink %d %v", dithered, b.Ink(), err)
	}
	grey := page(200, 200, 300, func(x, y int) byte { return 128 })
	b, dithered, err = Binarize(grey, DitherAuto)
	if err != nil || !dithered {
		t.Fatalf("grey not dithered: %v", err)
	}
	if ink := b.Ink(); ink < 200*200*45/100 || ink > 200*200*55/100 {
		t.Fatalf("50%% grey dithered to %d of %d dots", ink, 200*200)
	}
	if b, _, _ := Binarize(grey, DitherOff); b.Ink() != 0 {
		t.Fatal("128 thresholds to white")
	}
	clamp := page(16, 16, 300, func(x, y int) byte { return 245 })
	if b, _, _ := Binarize(clamp, DitherOn); b.Ink() != 0 {
		t.Fatal("near-white dithered to dots")
	}
	if _, _, err := Binarize(text, "maybe"); err == nil {
		t.Fatal("bad mode accepted")
	}
	if _, _, err := Binarize(raster.Page{Width: 2, Height: 2}, ""); err == nil {
		t.Fatal("malformed page accepted")
	}
}

func a4(dpi int) driver.Media {
	return driver.Media{Name: driver.MediaA4, Width: 21000, Length: 29700, Margins: driver.Margins{Top: 423, Bottom: 423, Left: 423, Right: 423}}
}

func TestPlaceExactCopy(t *testing.T) {
	// A4 at 300 dpi is 2480x3508.
	p := page(2480, 3508, 300, func(x, y int) byte { return byte((x + 3*y) % 251) })
	out, err := Place(p, a4(300), PlaceOptions{XDPI: 300, YDPI: 300, KeepMargins: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Width != 2480 || out.Height != 3508 || !bytes.Equal(out.Gray, p.Gray) {
		t.Fatalf("not an exact copy: %dx%d", out.Width, out.Height)
	}
	out, _ = Place(p, a4(300), PlaceOptions{XDPI: 300, YDPI: 300})
	if out.Gray[0] != 255 || out.Gray[100*2480+100] != p.Gray[100*2480+100] {
		t.Fatal("margins not cleared, or content changed")
	}
}

func TestPlaceDownscale(t *testing.T) {
	p := page(4960, 7016, 600, func(x, y int) byte {
		if (x/2+y/2)%2 == 0 {
			return 0
		}
		return 200
	})
	out, err := Place(p, a4(600), PlaceOptions{XDPI: 300, YDPI: 300, KeepMargins: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Width != 2480 || out.Height != 3508 || out.XDPI != 300 {
		t.Fatalf("%dx%d @%d", out.Width, out.Height, out.XDPI)
	}
	if out.Gray[0] != 0 || out.Gray[1] != 200 || out.Gray[2480] != 200 {
		t.Fatalf("2x2 blocks not averaged: %d %d %d", out.Gray[0], out.Gray[1], out.Gray[2480])
	}
}

func TestPlaceFitAndRotate(t *testing.T) {
	// A landscape Letter page (11x8.5 in at 100 dpi) on portrait A4 is turned, then fitted.
	p := page(1100, 850, 100, func(x, y int) byte {
		if x < 10 && y < 10 {
			return 0
		}
		return 255
	})
	out, err := Place(p, a4(300), PlaceOptions{XDPI: 300, YDPI: 300, Scaling: "fit"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Width != 2480 || out.Height != 3508 {
		t.Fatalf("%dx%d", out.Width, out.Height)
	}
	// Counter-clockwise: the top-left mark ends at the bottom-left, inside the margins.
	ink := func(x0, y0, x1, y1 int) bool {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if out.Gray[y*out.Width+x] < 128 {
					return true
				}
			}
		}
		return false
	}
	if !ink(0, 3508-400, 400, 3508) || ink(0, 0, 400, 400) {
		t.Fatal("mark not at the bottom-left")
	}
	if ink(0, 0, 49, 3508) || ink(0, 3508-49, 2480, 3508) {
		t.Fatal("ink inside the margins")
	}
	cw, _ := Place(p, a4(300), PlaceOptions{XDPI: 300, YDPI: 300, Scaling: "fit", Orientation: 5})
	out = cw
	if !ink(2480-400, 0, 2480, 400) {
		t.Fatal("clockwise mark not at the top-right")
	}
}

func TestPlaceRollAndNone(t *testing.T) {
	roll := driver.Media{Width: 5400, Margins: driver.Margins{Left: 100, Right: 100}, Roll: true}
	p := page(1000, 300, 300, func(x, y int) byte { return 0 })
	out, err := Place(p, roll, PlaceOptions{XDPI: 300, YDPI: 300})
	if err != nil {
		t.Fatal(err)
	}
	// 54 mm at 300 dpi is 638 dots; the 1000-dot page is fitted into 52 mm (614 dots).
	if out.Width != 638 || out.Height != 184 {
		t.Fatalf("%dx%d", out.Width, out.Height)
	}
	small := page(100, 50, 300, func(x, y int) byte { return 0 })
	out, _ = Place(small, a4(300), PlaceOptions{XDPI: 300, YDPI: 300, Scaling: "none"})
	if out.Gray[(3508/2)*2480+2480/2] != 0 || out.Gray[(3508/2)*2480+2480/2+60] != 255 {
		t.Fatal("small page not centred at 1:1")
	}
	if _, err := Place(small, a4(300), PlaceOptions{XDPI: 300, YDPI: 300, Scaling: "stretch"}); err == nil {
		t.Fatal("bad scaling accepted")
	}
	fill, _ := Place(small, a4(300), PlaceOptions{XDPI: 300, YDPI: 300, Scaling: "fill", KeepMargins: true})
	if fill.Gray[0] != 0 || fill.Gray[len(fill.Gray)-1] != 0 {
		t.Fatal("fill left white corners")
	}
}

func TestRotate(t *testing.T) {
	p := page(3, 2, 300, func(x, y int) byte { return byte(10*y + x) })
	p.YDPI = 600
	r := Rotate(p, 1)
	// Counter-clockwise: the top row becomes the left column, read bottom to top.
	if r.Width != 2 || r.Height != 3 || r.XDPI != 600 || r.YDPI != 300 || !bytes.Equal(r.Gray, []byte{2, 12, 1, 11, 0, 10}) {
		t.Fatalf("%+v", r)
	}
	if back := Rotate(Rotate(r, 2), 1); !bytes.Equal(back.Gray, p.Gray) || back.Width != 3 {
		t.Fatalf("four quarter turns: %+v", back)
	}
	if Rotate(p, 4).Width != 3 {
		t.Fatal("full turn")
	}
}

func TestFromImage(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	img.Set(1, 0, color.NRGBA{A: 0})
	p := FromImage(img, 203)
	if p.XDPI != 203 || p.Gray[0] != 85 || p.Gray[1] != 255 {
		t.Fatalf("%+v", p)
	}
	if g := GrayImage(p); g.GrayAt(0, 0).Y != 85 {
		t.Fatal("GrayImage")
	}
}
