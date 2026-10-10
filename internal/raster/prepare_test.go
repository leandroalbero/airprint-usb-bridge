package raster

import (
	"errors"
	"testing"

	"github.com/leandroalbero/ql-airprint/internal/raster/testutil"
)

func grayPage(w, h, dpi int, g []byte) Page {
	return Page{Width: w, Height: h, XDPI: dpi, YDPI: dpi, Gray: g}
}

func TestPrepareErrors(t *testing.T) {
	ok := grayPage(732, 2, 300, whiteGray(732, 2))
	cases := []struct {
		name string
		p    Page
		o    func(*Options)
		want error
	}{
		{"dpi", grayPage(732, 2, 200, whiteGray(732, 2)), nil, ErrResolution},
		{"narrow", grayPage(700, 2, 300, whiteGray(700, 2)), nil, ErrWidth},
		{"wide", grayPage(741, 2, 300, whiteGray(741, 2)), nil, ErrWidth},
		{"wide at 600", grayPage(1484, 2, 600, whiteGray(1484, 2)), nil, ErrWidth},
		{"dither", ok, func(o *Options) { o.Dither = "maybe" }, nil},
		{"pins", ok, func(o *Options) { o.Pins = 0 }, nil},
		{"short gray", Page{Width: 732, Height: 2, XDPI: 300, Gray: make([]byte, 10)}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := baseOptions()
			if c.o != nil {
				c.o(&o)
			}
			_, err := Prepare(c.p, o)
			if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestPrepareWidthBoundsAt600(t *testing.T) {
	for _, w := range []int{1448, 1449, 1480, 1481} {
		if _, err := Prepare(grayPage(w, 4, 600, whiteGray(w, 4)), baseOptions()); err != nil {
			t.Errorf("width %d: %v", w, err)
		}
	}
	if _, err := Prepare(grayPage(1447, 4, 600, whiteGray(1447, 4)), baseOptions()); !errors.Is(err, ErrWidth) {
		t.Errorf("1447: %v", err)
	}
}

func TestPrepareEmptyDitherMeansAuto(t *testing.T) {
	p := grayPage(732, 100, 300, gradientGray(1, 732, 100))
	o := baseOptions()
	o.Dither = ""
	l, err := Prepare(p, o)
	if err != nil || !l.Dithered {
		t.Fatalf("dithered=%v err=%v", l.Dithered, err)
	}
}

func TestPreparePhaseKeepsEdgesSharp(t *testing.T) {
	for _, parity := range []int{0, 1} {
		g := textGray(5, 1464, 400, 40, 2, parity, false)
		gray, w, h := halve(g, 1464, 400, true)
		if w != 732 || h != 200 {
			t.Fatalf("parity %d: halved to %dx%d", parity, w, h)
		}
		for i, v := range gray {
			if v != 0 && v != 255 {
				t.Fatalf("parity %d: grey %d at x=%d y=%d", parity, v, i%w, i/w)
			}
		}
	}
}

func TestPrepareTrim(t *testing.T) {
	g := whiteGray(732, 200)
	fillRect(g, 732, 100, 60, 500, 80, 0)
	p := grayPage(732, 200, 300, g)
	for _, c := range []struct{ trim, rows int }{{-1, 80}, {0, 200}, {24, 152}, {60, 80}, {1000, 80}} {
		o := baseOptions()
		o.TrimMax = c.trim
		l, err := Prepare(p, o)
		if err != nil {
			t.Fatal(err)
		}
		if l.Bitmap.H != c.rows || l.Bitmap.W != 696 || l.Blank || l.HighRes {
			t.Errorf("trim %d: %dx%d blank=%v", c.trim, l.Bitmap.W, l.Bitmap.H, l.Blank)
		}
	}
}

func TestPrepareHighResDoublesTrim(t *testing.T) {
	g := whiteGray(1464, 400)
	fillRect(g, 1464, 200, 120, 1000, 160, 0)
	o := baseOptions()
	o.HighRes = true
	l, err := Prepare(grayPage(1464, 400, 600, g), o)
	if err != nil {
		t.Fatal(err)
	}
	if !l.HighRes || l.Bitmap.H != 400-2*48 {
		t.Fatalf("high-res=%v rows=%d", l.HighRes, l.Bitmap.H)
	}
	o.HighRes = false
	l, err = Prepare(grayPage(1464, 400, 600, g), o)
	if err != nil {
		t.Fatal(err)
	}
	if l.HighRes || l.Bitmap.H != 200-2*24 {
		t.Fatalf("high-res=%v rows=%d", l.HighRes, l.Bitmap.H)
	}
}

func TestPrepareBlank(t *testing.T) {
	for _, o := range []Options{baseOptions(), {Pins: 696, WidthMin: 724, WidthMax: 740, TrimMax: -1}} {
		l, err := Prepare(grayPage(732, 100, 300, whiteGray(732, 100)), o)
		if err != nil || !l.Blank {
			t.Fatalf("blank=%v err=%v", l.Blank, err)
		}
	}
	// One 600 dpi row halves to no rows at all.
	l, err := Prepare(grayPage(1464, 1, 600, make([]byte, 1464)), baseOptions())
	if err != nil || !l.Blank || l.Bitmap.H != 0 {
		t.Fatalf("blank=%v err=%v", l.Blank, err)
	}
}

func TestPrepareDroppedAndCentring(t *testing.T) {
	g := whiteGray(740, 10)
	fillRect(g, 740, 0, 0, 22, 10, 0)
	l, err := Prepare(grayPage(740, 10, 300, g), baseOptions())
	if err != nil {
		t.Fatal(err)
	}
	if l.Dropped != 22*10 {
		t.Fatalf("dropped %d", l.Dropped)
	}
	if l.Bitmap.At(0, 0) {
		t.Fatal("column 0 should be white after dropping the 22 left columns")
	}
}

func TestFloorDiv(t *testing.T) {
	for _, c := range [][3]int{{-5, 2, -3}, {-6, 2, -3}, {-1, 2, -1}, {5, 2, 2}, {0, 2, 0}, {36, 2, 18}} {
		if got := floorDiv(c[0], c[1]); got != c[2] {
			t.Errorf("floorDiv(%d,%d)=%d want %d", c[0], c[1], got, c[2])
		}
	}
}

func TestEndToEndSyntheticURF(t *testing.T) {
	g := whiteGray(732, 100)
	fillRect(g, 732, 18, 30, 696, 40, 0)
	pages, err := Decode(testutil.EncodeURF(testutil.GrayPage(732, 100, 300, g)))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Prepare(pages[0], baseOptions())
	if err != nil {
		t.Fatal(err)
	}
	if l.Bitmap.H != 52 || l.Dropped != 0 {
		t.Fatalf("rows=%d dropped=%d", l.Bitmap.H, l.Dropped)
	}
	for y := 6; y < 46; y++ {
		for x := 0; x < 696; x++ {
			if !l.Bitmap.At(x, y) {
				t.Fatalf("white dot at %d,%d", x, y)
			}
		}
	}
}
