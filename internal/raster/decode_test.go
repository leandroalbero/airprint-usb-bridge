package raster

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"

	"github.com/leandroalbero/ql-airprint/internal/raster/testutil"
)

func expectGray(p testutil.Page, subtractive bool) []byte {
	g := make([]byte, p.Width*p.Height)
	bypp := p.BPP / 8
	for y := 0; y < p.Height; y++ {
		stride := p.Width * bypp
		toGray(g[y*p.Width:(y+1)*p.Width], p.Pix[y*stride:(y+1)*stride], bypp, subtractive)
	}
	return g
}

func TestDecodeRoundTrip(t *testing.T) {
	text := textGray(1, 732, 120, 10, 1, 0, true)
	photo := gradientGray(2, 732, 90)
	cases := []struct {
		name        string
		page        testutil.Page
		pwg         bool
		subtractive bool
	}{
		{"urf gray", testutil.GrayPage(732, 120, 300, text), false, false},
		{"urf rgb24", colourPage(3, 732, 90, 300, 24, testutil.URFRGB, false, photo), false, false},
		{"urf rgb32", colourPage(4, 732, 90, 600, 32, testutil.URFRGB, false, photo), false, false},
		{"urf cmyk32", colourPage(5, 732, 120, 300, 32, testutil.URFCMYK, true, text), false, true},
		{"pwg gray", testutil.Page{Width: 732, Height: 120, XDPI: 300, BPP: 8, ColorSpace: testutil.PWGGray, Pix: text}, true, false},
		{"pwg black", testutil.Page{Width: 732, Height: 120, XDPI: 300, BPP: 8, ColorSpace: testutil.PWGBlack, Pix: invert(text)}, true, true},
		{"pwg rgb24", colourPage(6, 732, 90, 300, 24, testutil.PWGRGB, false, photo), true, false},
		{"pwg cmyk32", colourPage(7, 732, 120, 300, 32, testutil.PWGCMYK, true, text), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var data []byte
			if c.pwg {
				data = testutil.EncodePWG(c.page, c.page)
			} else {
				data = testutil.EncodeURF(c.page, c.page)
			}
			pages, err := Decode(data)
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != 2 {
				t.Fatalf("got %d pages", len(pages))
			}
			want := expectGray(c.page, c.subtractive)
			for _, p := range pages {
				if p.Width != c.page.Width || p.Height != c.page.Height || p.XDPI != c.page.XDPI || p.YDPI != c.page.XDPI {
					t.Fatalf("header %+v", p)
				}
				if !bytes.Equal(p.Gray, want) {
					t.Fatal("gray mismatch")
				}
			}
			if !c.subtractive && c.page.BPP == 8 && !bytes.Equal(pages[0].Gray, c.page.Pix) {
				t.Fatal("8-bit additive gray must pass through unchanged")
			}
		})
	}
}

func TestDecodePWGYDPI(t *testing.T) {
	p := testutil.GrayPage(4, 2, 300, whiteGray(4, 2))
	p.YDPI = 600
	pages, err := Decode(testutil.EncodePWG(p))
	if err != nil {
		t.Fatal(err)
	}
	if pages[0].XDPI != 300 || pages[0].YDPI != 600 {
		t.Fatalf("got %dx%d", pages[0].XDPI, pages[0].YDPI)
	}
}

func urfHeader(count uint32) []byte {
	return append([]byte("UNIRAST\x00"), byte(count>>24), byte(count>>16), byte(count>>8), byte(count))
}

func TestDecodeRunOverrunAndFill(t *testing.T) {
	data := urfHeader(0)
	data = append(data, testutil.URFPageHeader(testutil.Page{Width: 5, Height: 4, XDPI: 300, BPP: 8})...)
	// Line 1 (x2): a 4-pixel literal then a 3-pixel run overshooting by 2.
	data = append(data, 1, 253, 10, 20, 30, 40, 2, 50)
	// Line 3: a 7-pixel literal overshooting by 2, whose excess is consumed.
	data = append(data, 0, 250, 1, 2, 3, 4, 5, 6, 7)
	// Line 4: one pixel then fill.
	data = append(data, 0, 0, 9, 128)
	pages, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		10, 20, 30, 40, 50,
		10, 20, 30, 40, 50,
		1, 2, 3, 4, 5,
		9, 255, 255, 255, 255,
	}
	if !bytes.Equal(pages[0].Gray, want) {
		t.Fatalf("got %v", pages[0].Gray)
	}
}

func TestDecodeLineRepeatClampedToHeight(t *testing.T) {
	data := urfHeader(1)
	data = append(data, testutil.URFPageHeader(testutil.Page{Width: 2, Height: 3, XDPI: 300, BPP: 8})...)
	data = append(data, 255, 1, 7)
	pages, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pages[0].Gray, []byte{7, 7, 7, 7, 7, 7}) {
		t.Fatalf("got %v", pages[0].Gray)
	}
}

func TestDecodeSubtractiveFill(t *testing.T) {
	data := urfHeader(1)
	data = append(data, testutil.URFPageHeader(testutil.Page{Width: 2, Height: 1, XDPI: 300, BPP: 32, ColorSpace: testutil.URFCMYK})...)
	data = append(data, 0, 128)
	pages, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pages[0].Gray, []byte{255, 255}) {
		t.Fatalf("got %v", pages[0].Gray)
	}
}

func TestDecodeURFCountZeroReadsAll(t *testing.T) {
	p := testutil.GrayPage(732, 3, 300, whiteGray(732, 3))
	pages, err := Decode(testutil.EncodeURFCount(0, p, p, p))
	if err != nil || len(pages) != 3 {
		t.Fatalf("pages=%d err=%v", len(pages), err)
	}
}

func TestDecodeURFCountStopsEarly(t *testing.T) {
	p := testutil.GrayPage(732, 3, 300, whiteGray(732, 3))
	pages, err := Decode(testutil.EncodeURFCount(2, p, p, p))
	if err != nil || len(pages) != 2 {
		t.Fatalf("pages=%d err=%v", len(pages), err)
	}
}

func TestDecodeErrors(t *testing.T) {
	good := testutil.GrayPage(732, 40, 300, textGray(1, 732, 40, 2, 1, 0, true))
	badBPP := good
	badBPP.BPP = 16
	zero := good
	zero.Width = 0
	wide := testutil.Page{Width: MaxWidth + 1, Height: 1, XDPI: 300, BPP: 8}
	tall := testutil.Page{Width: 1, Height: MaxHeight + 1, XDPI: 300, BPP: 8}
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrUnsupported},
		{"pdf", []byte("%PDF-1.7\n"), ErrUnsupported},
		{"urf no count", []byte("UNIRAST\x00\x00"), ErrTruncated},
		{"urf no pages", urfHeader(0), ErrNoPages},
		{"pwg no pages", []byte("RaS2"), ErrNoPages},
		{"urf missing pages", testutil.EncodeURFCount(3, good, good), ErrTruncated},
		{"bpp", append(urfHeader(1), testutil.URFPageHeader(badBPP)...), ErrUnsupported},
		{"zero width", append(urfHeader(1), testutil.URFPageHeader(zero)...), ErrUnsupported},
		{"too wide", append(urfHeader(1), testutil.URFPageHeader(wide)...), ErrTooLarge},
		{"too tall", append([]byte("RaS2"), testutil.PWGPageHeader(tall)...), ErrTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Decode(c.data); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestDecodeGrayBudget(t *testing.T) {
	p := testutil.GrayPage(100, 100, 300, whiteGray(100, 100))
	data := testutil.EncodeURF(p, p, p)
	if _, err := decode(data, Limits{MaxGrayBytes: 3 * 100 * 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := decode(data, Limits{MaxGrayBytes: 3*100*100 - 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	// A lying header must not allocate its declared size up front.
	huge := append(urfHeader(1), testutil.URFPageHeader(testutil.Page{Width: MaxWidth, Height: MaxHeight / 8, XDPI: 300, BPP: 8})...)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Decode(huge); !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %v", err)
	}
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 16<<20 {
		t.Fatalf("truncated %dx%d header allocated %d bytes", MaxWidth, MaxHeight/8, n)
	}
}

func TestDecodeEveryTruncation(t *testing.T) {
	text := textGray(9, 732, 30, 2, 1, 0, true)
	for _, data := range [][]byte{
		testutil.EncodeURF(testutil.GrayPage(732, 30, 300, text)),
		testutil.EncodePWG(colourPage(1, 732, 30, 300, 24, testutil.PWGRGB, false, text)),
	} {
		for n := 0; n < len(data); n++ {
			pages, err := Decode(data[:n])
			if err == nil {
				t.Fatalf("prefix %d of %d decoded %d pages without error", n, len(data), len(pages))
			}
		}
	}
}

func TestDecodeIgnoresShortTrailer(t *testing.T) {
	p := testutil.GrayPage(732, 3, 300, whiteGray(732, 3))
	pwg := append(testutil.EncodePWG(p), make([]byte, 1795)...)
	if pages, err := Decode(pwg); err != nil || len(pages) != 1 {
		t.Fatalf("pwg pages=%d err=%v", len(pages), err)
	}
	urf := append(testutil.EncodeURF(p), 1, 2, 3)
	if pages, err := Decode(urf); err != nil || len(pages) != 1 {
		t.Fatalf("urf pages=%d err=%v", len(pages), err)
	}
}

func TestPWGHeaderLayout(t *testing.T) {
	h := testutil.PWGPageHeader(testutil.Page{Width: 732, Height: 10, XDPI: 300, BPP: 24, ColorSpace: testutil.PWGRGB})
	if len(h) != pwgHeaderLen {
		t.Fatalf("len %d", len(h))
	}
	if got := binary.BigEndian.Uint32(h[392:]); got != 732*3 {
		t.Fatalf("bytes per line %d", got)
	}
}
