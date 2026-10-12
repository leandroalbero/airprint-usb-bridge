package raster

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// expectRGB is what an RGB decode of p must give, written out from the spec rather than from
// toRGB: gray is R=G=B, RGB is taken as is, CMYK is 255-min(255, C+K) per channel.
func expectRGB(p testutil.Page, subtractive bool) []byte {
	bypp := p.BPP / 8
	out := make([]byte, 0, 3*p.Width*p.Height)
	for i := 0; i < p.Width*p.Height; i++ {
		px := p.Pix[i*bypp : (i+1)*bypp]
		switch {
		case bypp == 1 && subtractive:
			out = append(out, 255-px[0], 255-px[0], 255-px[0])
		case bypp == 1:
			out = append(out, px[0], px[0], px[0])
		case bypp == 4 && subtractive:
			for c := 0; c < 3; c++ {
				out = append(out, byte(255-min(255, int(px[c])+int(px[3]))))
			}
		case subtractive:
			out = append(out, 255-px[0], 255-px[1], 255-px[2])
		default:
			out = append(out, px[0], px[1], px[2])
		}
	}
	return out
}

func TestReaderRGB(t *testing.T) {
	const w, h = 37, 23
	gray := gradientGray(3, w, h)
	cases := []struct {
		name string
		doc  []byte
		page testutil.Page
		sub  bool
	}{
		{"urf W8", nil, testutil.GrayPage(w, h, 300, gray), false},
		{"urf sRGB24", nil, colourPage(5, w, h, 300, 24, testutil.URFRGB, false, gray), false},
		{"urf CMYK32", nil, colourPage(7, w, h, 300, 32, testutil.URFCMYK, true, gray), true},
		{"pwg sgray", nil, testutil.Page{Width: w, Height: h, XDPI: 600, BPP: 8, ColorSpace: testutil.PWGGray, Pix: gray}, false},
		{"pwg black", nil, testutil.Page{Width: w, Height: h, XDPI: 600, BPP: 8, ColorSpace: testutil.PWGBlack, Pix: invert(gray)}, true},
		{"pwg srgb", nil, colourPage(9, w, h, 600, 24, testutil.PWGRGB, false, gray), false},
		{"pwg cmyk", nil, colourPage(11, w, h, 600, 32, testutil.PWGCMYK, true, gray), true},
	}
	for _, c := range cases {
		doc := testutil.EncodeURF(c.page)
		if c.name[:3] == "pwg" {
			doc = testutil.EncodePWG(c.page, c.page)
		}
		r := NewReader(bytes.NewReader(doc), Limits{RGB: true})
		p, err := r.Next()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.Gray != nil || !p.IsRGB() {
			t.Fatalf("%s: RGB decode gave a gray page", c.name)
		}
		if want := expectRGB(c.page, c.sub); !bytes.Equal(p.RGB, want) {
			t.Errorf("%s: RGB pixels differ", c.name)
		}
		if got, want := r.Colored(), c.page.BPP > 8; got != want {
			t.Errorf("%s: Colored = %v, want %v", c.name, got, want)
		}
		// The same document read as gray is what it always was.
		g, err := NewReader(bytes.NewReader(doc), Limits{}).Next()
		if err != nil || g.RGB != nil || !bytes.Equal(g.Gray, expectGray(c.page, c.sub)) {
			t.Errorf("%s: gray decode changed (%v)", c.name, err)
		}
	}
}

func TestReaderRGBBudgetCountsThreeBytes(t *testing.T) {
	const w, h = 40, 30
	page := testutil.GrayPage(w, h, 300, whiteGray(w, h))
	doc := testutil.EncodeURF(page)
	if _, err := NewReader(bytes.NewReader(doc), Limits{MaxGrayBytes: w * h}).Next(); err != nil {
		t.Fatalf("gray page within its budget: %v", err)
	}
	if _, err := NewReader(bytes.NewReader(doc), Limits{MaxGrayBytes: 3*w*h - 1, RGB: true}).Next(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("RGB page over a %d byte budget: %v, want ErrTooLarge", 3*w*h-1, err)
	}
	r := NewReader(bytes.NewReader(doc), Limits{MaxGrayBytes: 3 * w * h, RGB: true})
	if _, err := r.Skip(); err != nil {
		t.Errorf("RGB page at exactly its budget: %v", err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("after the only page: %v", err)
	}
}

// TestReaderRGBReusesBuffer checks the streaming reader keeps one RGB buffer with spare room
// for InPlace placement (pages over the 1 MiB first allocation), as it does for gray.
func TestReaderRGBReusesBuffer(t *testing.T) {
	const w, h = 700, 600
	page := colourPage(1, w, h, 300, 24, testutil.URFRGB, false, gradientGray(1, w, h))
	r := NewReader(bytes.NewReader(testutil.EncodeURF(page, page)), Limits{RGB: true})
	a, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if cap(a.RGB) < 3*w*h+3*w*h/64 {
		t.Errorf("RGB buffer cap %d, want spare room past %d", cap(a.RGB), 3*w*h)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if &a.RGB[0] != &b.RGB[0] {
		t.Error("second page got a new buffer")
	}
}

// TestColoredByContent: a colour format whose pixels are all gray is not colour; one pixel off
// gray anywhere (in a literal run, a repeated pixel or a repeated line, on any page) is, and so
// is a CMYK pixel with K.
func TestColoredByContent(t *testing.T) {
	const w, h = 40, 9
	page := func(bpp, cs int, set func(px []byte, x, y int)) testutil.Page {
		bypp := bpp / 8
		pix := make([]byte, w*h*bypp)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				set(pix[(y*w+x)*bypp:(y*w+x+1)*bypp], x, y)
			}
		}
		return testutil.Page{Width: w, Height: h, XDPI: 300, BPP: bpp, ColorSpace: cs, Pix: pix}
	}
	neutral := func(px []byte, x, y int) {
		v := byte(x * 6)
		if y >= 3 && y < 6 {
			v = 128 // a run, and three equal lines
		}
		if x >= 30 {
			v = 255 // paper, sent as the end-of-line fill
		}
		px[0], px[1], px[2] = v, v, v
	}
	offAt := func(x0, y0 int) func(px []byte, x, y int) {
		return func(px []byte, x, y int) {
			neutral(px, x, y)
			if x == x0 && y == y0 {
				px[1]++
			}
		}
	}
	cmyk := func(px []byte, x, _ int) { px[0], px[1], px[2], px[3] = byte(x), byte(x), byte(x), 0 }
	cmykK := func(px []byte, x, y int) {
		cmyk(px, x, y)
		if x == 5 && y == 4 {
			px[3] = 200
		}
	}
	cases := []struct {
		name  string
		pages []testutil.Page
		want  bool
	}{
		{"neutral sRGB", []testutil.Page{page(24, testutil.URFRGB, neutral)}, false},
		{"neutral CMYK", []testutil.Page{page(32, testutil.URFCMYK, cmyk)}, false},
		// The gray decode would drop K.
		{"CMYK with K", []testutil.Page{page(32, testutil.URFCMYK, cmykK)}, true},
		{"literal run", []testutil.Page{page(24, testutil.URFRGB, offAt(17, 1))}, true},
		{"repeated pixel", []testutil.Page{page(24, testutil.URFRGB, func(px []byte, x, y int) {
			neutral(px, x, y)
			if x >= 10 && x < 20 && y == 7 {
				px[0], px[1], px[2] = 200, 100, 50
			}
		})}, true},
		{"repeated line", []testutil.Page{page(24, testutil.URFRGB, func(px []byte, x, y int) {
			neutral(px, x, y)
			if y >= 3 && y < 6 {
				px[2] = 0
			}
		})}, true},
		{"last page", []testutil.Page{page(24, testutil.URFRGB, neutral), page(8, testutil.URFGray, func(px []byte, x, _ int) { px[0] = byte(x) }),
			page(24, testutil.URFRGB, offAt(39, 8))}, true},
	}
	for _, c := range cases {
		doc := testutil.EncodeURF(c.pages...)
		for _, mode := range []string{"skip", "gray", "rgb"} {
			r := NewReader(bytes.NewReader(doc), Limits{RGB: mode == "rgb"})
			for {
				var err error
				if mode == "skip" {
					_, err = r.Skip()
				} else {
					_, err = r.Next()
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("%s %s: %v", c.name, mode, err)
				}
			}
			if r.Colored() != c.want {
				t.Errorf("%s %s: Colored = %v, want %v", c.name, mode, r.Colored(), c.want)
			}
		}
	}
}
