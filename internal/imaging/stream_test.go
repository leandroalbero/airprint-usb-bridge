package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// PackBitsTo must write exactly PackBits' output, whatever the size: the PostScript driver
// depends on it for byte-identical documents.
func TestPackBitsToMatchesPackBits(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for _, n := range []int{0, 1, 2, 3, 127, 128, 129, 64 << 10, 64<<10 + 1, 300000} {
		src := make([]byte, n)
		for i := 0; i < n; {
			v, run := byte(r.Intn(4)*85), 1+r.Intn(300)
			for ; run > 0 && i < n; run-- {
				src[i] = v
				if r.Intn(9) == 0 {
					src[i] = byte(r.Intn(256))
				}
				i++
			}
		}
		var buf bytes.Buffer
		if err := PackBitsTo(&buf, src); err != nil {
			t.Fatal(err)
		}
		if want := PackBits(nil, src); !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("n=%d: %d bytes, want %d", n, buf.Len(), len(want))
		}
	}
}

func noisePage(r *rand.Rand, w, h, dpi int) raster.Page {
	p := raster.Page{Width: w, Height: h, XDPI: dpi, YDPI: dpi, Gray: make([]byte, w*h)}
	for i := range p.Gray {
		p.Gray[i] = byte(r.Intn(256))
	}
	return p
}

// Placing in place must give the same sheet as placing on a new one, for pages a little larger
// or smaller than the sheet, turned or not, with or without spare capacity.
func TestPlaceInPlaceMatches(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	media := driver.Media{Name: "x", Width: 2540, Length: 3810, Margins: driver.Margins{Top: 100, Bottom: 50, Left: 30, Right: 80}}
	sheetW, sheetH := SheetDots(media, 100, 100)
	for _, c := range []struct{ w, h, spare, orient int }{
		{sheetW, sheetH, 0, 0},
		{sheetW - 1, sheetH, 0, 0},
		{sheetW - 1, sheetH, sheetH, 0},
		{sheetW - 2, sheetH - 3, 1000, 0},
		{sheetW + 1, sheetH + 2, 0, 0},
		{sheetW + 2, sheetH, 0, 0},
		{sheetW, sheetH + 3, 0, 6},
		{sheetH, sheetW, 0, 0},
		{sheetH - 1, sheetW + 1, 0, 5},
		{sheetW - 1, sheetH + 1, 1000, 0},
		{sheetW / 2, sheetH / 2, 1 << 16, 0},
	} {
		for _, scaling := range []string{"", "none", "fill"} {
			name := fmt.Sprintf("%dx%d+%d/%d/%s", c.w, c.h, c.spare, c.orient, scaling)
			src := noisePage(r, c.w, c.h, 100)
			o := PlaceOptions{XDPI: 100, YDPI: 100, Scaling: scaling, Orientation: c.orient}
			want, err := Place(src, media, o)
			if err != nil {
				t.Fatal(err)
			}
			mine := src
			mine.Gray = append(make([]byte, 0, len(src.Gray)+c.spare), src.Gray...)
			o.InPlace = true
			got, err := Place(mine, media, o)
			if err != nil {
				t.Fatal(err)
			}
			if got.Width != want.Width || got.Height != want.Height || got.XDPI != want.XDPI || !bytes.Equal(got.Gray, want.Gray) {
				t.Errorf("%s: in-place sheet differs", name)
			}
		}
	}
}

// CheckPlace must refuse exactly what Place refuses.
func TestCheckPlaceAgreesWithPlace(t *testing.T) {
	a4 := driver.Media{Name: "a4", Width: 21000, Length: 29700}
	small := driver.NewBudget(2480 * 3508)
	cases := []struct {
		p raster.Page
		m driver.Media
		o PlaceOptions
	}{
		{raster.Page{Width: 10, Height: 10, XDPI: 300, YDPI: 300, Gray: make([]byte, 5)}, a4, PlaceOptions{XDPI: 300, YDPI: 300}},
		{raster.Page{Width: 0, Height: 10, XDPI: 300, YDPI: 300}, a4, PlaceOptions{XDPI: 300, YDPI: 300}},
		{raster.Page{Width: 10, Height: 10, XDPI: 300, YDPI: 300, Gray: make([]byte, 100)}, a4, PlaceOptions{XDPI: 0, YDPI: 300}},
		{raster.Page{Width: 10, Height: 10, XDPI: 300, YDPI: 300, Gray: make([]byte, 100)}, a4, PlaceOptions{XDPI: 300, YDPI: 300, Scaling: "stretch"}},
		{raster.Page{Width: 10, Height: 10, XDPI: 300, YDPI: 300, Gray: make([]byte, 100)}, a4, PlaceOptions{XDPI: 600, YDPI: 600, Budget: small}},
		{raster.Page{Width: 10, Height: 10, XDPI: 300, YDPI: 300, Gray: make([]byte, 100)}, a4, PlaceOptions{XDPI: 300, YDPI: 300, Budget: small}},
		{raster.Page{Width: 2000, Height: 10, XDPI: 1, YDPI: 1, Gray: make([]byte, 20000)}, a4, PlaceOptions{XDPI: 300, YDPI: 300, Scaling: "fill", Budget: small}},
	}
	for i, c := range cases {
		_, perr := Place(c.p, c.m, c.o)
		cerr := CheckPlace(c.p, c.m, c.o)
		header := c.p
		header.Gray = nil
		herr := CheckPlace(header, c.m, c.o)
		same := func(a, b error) bool {
			// A sheet over the budget is reported per page by CheckPlace, per job by Place.
			if errors.Is(a, driver.ErrJobTooLarge) {
				return errors.Is(b, driver.ErrJobTooLarge) && errors.Is(b, driver.ErrDocument)
			}
			return fmt.Sprint(a) == fmt.Sprint(b)
		}
		if !same(perr, cerr) {
			t.Errorf("case %d: Place %v, CheckPlace %v", i, perr, cerr)
		}
		if len(c.p.Gray) == c.p.Width*c.p.Height && !same(perr, herr) {
			t.Errorf("case %d: Place %v, CheckPlace without pixels %v", i, perr, herr)
		}
		if perr == nil {
			small.Release(int64(2480 * 3508))
		}
	}
}
