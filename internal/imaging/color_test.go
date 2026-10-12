package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// rgbOf interleaves three gray pages of one size into an RGB page.
func rgbOf(r, g, b raster.Page) raster.Page {
	p := raster.Page{Width: r.Width, Height: r.Height, XDPI: r.XDPI, YDPI: r.YDPI, RGB: make([]byte, 3*len(r.Gray))}
	for i := range r.Gray {
		p.RGB[3*i], p.RGB[3*i+1], p.RGB[3*i+2] = r.Gray[i], g.Gray[i], b.Gray[i]
	}
	return p
}

func channel(p raster.Page, c int) []byte {
	out := make([]byte, p.Width*p.Height)
	for i := range out {
		out[i] = p.RGB[3*i+c]
	}
	return out
}

// Each channel of an RGB page must be placed exactly as a gray page of that channel: turned,
// scaled, cropped, margins cleared, in place or not.
func TestPlaceRGBMatchesGray(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	media := driver.Media{Name: "x", Width: 2540, Length: 3810, Margins: driver.Margins{Top: 100, Bottom: 50, Left: 30, Right: 80}}
	roll := driver.Media{Name: "roll", Width: 2540, Margins: driver.Margins{Left: 100, Right: 60}}
	sheetW, sheetH := SheetDots(media, 100, 100)
	type tc struct {
		w, h, dpi, spare, orient int
		m                        driver.Media
	}
	cases := []tc{
		{sheetW, sheetH, 100, 0, 0, media},
		{sheetW - 1, sheetH, 100, sheetH * 3, 0, media},
		{sheetW + 2, sheetH + 1, 100, 0, 0, media},
		{sheetW, sheetH + 3, 100, 0, 6, media},
		{sheetH, sheetW, 100, 0, 0, media},
		{sheetH - 1, sheetW + 1, 100, 0, 5, media},
		{sheetW / 2, sheetH / 2, 100, 1 << 16, 0, media},
		{sheetW * 2, sheetH * 2, 200, 0, 0, media},
		{sheetW * 3, sheetH, 100, 0, 0, media},
		{70, 40, 37, 0, 4, media},
		{90, 130, 100, 0, 0, roll},
		{130, 90, 60, 0, 0, roll},
	}
	for _, c := range cases {
		for _, scaling := range []string{"", "none", "fit", "fill", "auto-fit"} {
			name := fmt.Sprintf("%dx%d@%d+%d/%d/%s/%s", c.w, c.h, c.dpi, c.spare, c.orient, scaling, c.m.Name)
			ch := [3]raster.Page{noisePage(r, c.w, c.h, c.dpi), noisePage(r, c.w, c.h, c.dpi), noisePage(r, c.w, c.h, c.dpi)}
			o := PlaceOptions{XDPI: 100, YDPI: 100, Scaling: scaling, Orientation: c.orient}
			var want [3]raster.Page
			for i := range ch {
				var err error
				if want[i], err = Place(ch[i], c.m, o); err != nil {
					t.Fatalf("%s: gray: %v", name, err)
				}
			}
			for _, inPlace := range []bool{false, true} {
				src := rgbOf(ch[0], ch[1], ch[2])
				src.RGB = append(make([]byte, 0, len(src.RGB)+3*c.spare), src.RGB...)
				o.InPlace = inPlace
				budget := driver.NewBudget(driver.MaxSheetBytes)
				o.Budget = budget
				got, err := Place(src, c.m, o)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if got.Gray != nil || got.Width != want[0].Width || got.Height != want[0].Height || got.XDPI != 100 || got.YDPI != 100 {
					t.Fatalf("%s in place %v: sheet %dx%d @%dx%d, want %dx%d", name, inPlace, got.Width, got.Height, got.XDPI, got.YDPI, want[0].Width, want[0].Height)
				}
				for i := range want {
					if !bytes.Equal(channel(got, i), want[i].Gray) {
						t.Errorf("%s in place %v: channel %d differs from the gray placement", name, inPlace, i)
					}
				}
				budget.Release(3 * int64(got.Width) * int64(got.Height))
				if err := budget.Charge(driver.MaxSheetBytes); err != nil {
					t.Errorf("%s: RGB sheet not charged as 3 bytes a dot: %v", name, err)
				}
			}
		}
	}
}

func TestCheckPlaceColorCountsThreeBytes(t *testing.T) {
	a4 := driver.Media{Name: "a4", Width: 21000, Length: 29700}
	w, h := SheetDots(a4, 300, 300)
	budget := driver.NewBudget(int64(w*h) * 2)
	p := raster.Page{Width: 2480, Height: 3508, XDPI: 300, YDPI: 300}
	if err := CheckPlace(p, a4, PlaceOptions{XDPI: 300, YDPI: 300, Budget: budget}); err != nil {
		t.Errorf("gray sheet within the budget: %v", err)
	}
	if err := CheckPlace(p, a4, PlaceOptions{XDPI: 300, YDPI: 300, Budget: budget, Color: true}); !errors.Is(err, driver.ErrJobTooLarge) {
		t.Errorf("RGB sheet over the budget: %v", err)
	}
	p.RGB = make([]byte, 3*p.Width*p.Height)
	if _, err := Place(p, a4, PlaceOptions{XDPI: 300, YDPI: 300, Budget: budget}); !errors.Is(err, driver.ErrJobTooLarge) {
		t.Errorf("Place of an RGB page over the budget: %v", err)
	}
	p.RGB = p.RGB[:10]
	if err := CheckPlace(p, a4, PlaceOptions{XDPI: 300, YDPI: 300}); err == nil {
		t.Error("malformed RGB page accepted")
	}
}

func TestDiffuserOneBitIsFloydSteinberg(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	p := noisePage(r, 97, 41, 300)
	for i := range p.Gray {
		p.Gray[i] = byte((i*7)%256)/2 + p.Gray[i]/2
	}
	// Blank and solid rows, some right after content (error still carried), some after more
	// blank rows (the fast path).
	for _, y := range []int{10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40} {
		v := byte(255)
		if y >= 30 {
			v = 0
		}
		for x := 0; x < p.Width; x++ {
			p.Gray[y*p.Width+x] = v
		}
	}
	want := FloydSteinberg(p)
	d := NewDiffuser(p.Width)
	out := make([]byte, p.Width)
	for y := 0; y < p.Height; y++ {
		d.Row(p.Gray[y*p.Width:(y+1)*p.Width], 1, out)
		for x, l := range out {
			if black := l == 0; black != want.At(x, y) {
				t.Fatalf("(%d,%d): level %d, FloydSteinberg black %v", x, y, l, want.At(x, y))
			}
		}
	}
}

func TestDiffuserLevelsKeepTheMean(t *testing.T) {
	for _, bits := range []int{1, 2, 4} {
		top := 1<<bits - 1
		for _, v := range []byte{30, 64, 128, 200} {
			d := NewDiffuser(200)
			in, out := bytes.Repeat([]byte{v}, 200), make([]byte, 200)
			sum, n := 0, 0
			for y := 0; y < 100; y++ {
				d.Row(in, bits, out)
				for _, l := range out {
					if int(l) > top {
						t.Fatalf("level %d over %d", l, top)
					}
					sum += int(l)
					n++
				}
			}
			mean := float64(sum) / float64(n) * 255 / float64(top)
			if d := mean - float64(v); d > 3 || d < -3 {
				t.Errorf("%d bits, value %d: mean %.1f", bits, v, mean)
			}
		}
	}
}

func TestRGBHelpers(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	img.Set(0, 0, color.NRGBA{255, 0, 0, 255})
	img.Set(1, 0, color.NRGBA{0, 0, 255, 0})
	img.Set(2, 0, color.NRGBA{0, 0, 0, 128})
	p := FromImageRGB(img, 300)
	if want := []byte{255, 0, 0, 255, 255, 255, 127, 127, 127}; !bytes.Equal(p.RGB, want) {
		t.Errorf("FromImageRGB = %v, want %v", p.RGB, want)
	}
	back := RGBImage(p)
	if c := back.NRGBAAt(0, 0); c != (color.NRGBA{255, 0, 0, 255}) {
		t.Errorf("RGBImage(0,0) = %v", c)
	}
	g := GrayFromRGB(p)
	if g.RGB != nil || !bytes.Equal(g.Gray, []byte{85, 255, 127}) {
		t.Errorf("GrayFromRGB = %v", g.Gray)
	}
	if LooksLikePhotoRGB(raster.Page{Width: 8, Height: 8, RGB: bytes.Repeat([]byte{255, 0, 0}, 64)}) {
		t.Error("solid red looks like a photo")
	}
	if !LooksLikePhotoRGB(raster.Page{Width: 8, Height: 8, RGB: bytes.Repeat([]byte{255, 120, 0}, 64)}) {
		t.Error("orange does not look like a photo")
	}
}

// A turned page is drawn in the Reuse buffer, which is kept for the next one.
func TestPlaceRGBReuse(t *testing.T) {
	media := driver.Media{Name: "x", Width: 2540, Length: 3810}
	w, h := SheetDots(media, 100, 100)
	var reuse []byte
	turned := raster.Page{Width: h, Height: w, XDPI: 100, YDPI: 100, RGB: make([]byte, 3*w*h)}
	a, err := Place(turned, media, PlaceOptions{XDPI: 100, YDPI: 100, InPlace: true, Reuse: &reuse})
	if err != nil {
		t.Fatal(err)
	}
	if len(reuse) != 3*w*h || &reuse[0] != &a.RGB[0] {
		t.Fatalf("sheet not drawn in the reuse buffer (len %d)", len(reuse))
	}
	turned.RGB = make([]byte, 3*w*h)
	b, err := Place(turned, media, PlaceOptions{XDPI: 100, YDPI: 100, InPlace: true, Reuse: &reuse})
	if err != nil || &b.RGB[0] != &a.RGB[0] {
		t.Fatalf("second sheet not in the same buffer: %v", err)
	}
	// A page placed in place does not touch the reuse buffer.
	flat := raster.Page{Width: w, Height: h, XDPI: 100, YDPI: 100, RGB: make([]byte, 3*w*h)}
	c, err := Place(flat, media, PlaceOptions{XDPI: 100, YDPI: 100, InPlace: true, Reuse: &reuse})
	if err != nil || &c.RGB[0] != &flat.RGB[0] || &reuse[0] != &a.RGB[0] {
		t.Fatalf("in-place page: %v", err)
	}
}
