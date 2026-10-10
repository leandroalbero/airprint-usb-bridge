package raster

import (
	"errors"
	"runtime"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

var rollLimits = Limits{MaxGrayBytes: 64 << 20, WidthMin: 724, WidthMax: 740}

// A few hundred bytes of repeat-line, fill-to-end codes expand to ~500 MiB; a
// wrong-width page must be refused from its header alone.
func TestDecodeLimitedRefusesBombByHeader(t *testing.T) {
	data := append(urfHeader(1), testutil.URFPageHeader(testutil.Page{Width: 20000, Height: 26000, XDPI: 300, BPP: 8})...)
	for y := 0; y < 26000; y += 256 {
		data = append(data, 0xff, 0x80)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := DecodeLimited(data, rollLimits)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrWidth) {
		t.Fatalf("got %v, want ErrWidth", err)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > 1<<20 {
		t.Errorf("allocated %d bytes refusing the page", d)
	}
}

func TestDecodeLimitedGrayBudget(t *testing.T) {
	p := testutil.GrayPage(732, 100, 300, whiteGray(732, 100))
	data := testutil.EncodeURF(p, p)
	lim := rollLimits
	lim.MaxGrayBytes = 2*732*100 - 1
	if _, err := DecodeLimited(data, lim); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	lim.MaxGrayBytes++
	if _, err := DecodeLimited(data, lim); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeLimitedResolution(t *testing.T) {
	data := testutil.EncodeURF(testutil.GrayPage(732, 4, 150, whiteGray(732, 4)))
	if _, err := DecodeLimited(data, rollLimits); !errors.Is(err, ErrResolution) {
		t.Fatalf("got %v, want ErrResolution", err)
	}
}

// The header check must never refuse a page Prepare accepts, whatever pairing
// phase the 600 dpi halving picks.
func TestDecodeLimitedAgreesWithPrepare(t *testing.T) {
	opts := Options{Pins: 696, WidthMin: 724, WidthMax: 740, TrimMax: -1}
	check := func(w, dpi int, g []byte) {
		t.Helper()
		data := testutil.EncodeURF(testutil.GrayPage(w, 4, dpi, g))
		pages, err := Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		_, perr := Prepare(pages[0], opts)
		_, lerr := DecodeLimited(data, rollLimits)
		if perr == nil && lerr != nil {
			t.Errorf("%d dots at %d dpi: Prepare accepts, DecodeLimited says %v", w, dpi, lerr)
		}
		if errors.Is(perr, ErrWidth) && lerr == nil && dpi == 300 {
			t.Errorf("%d dots at 300 dpi: DecodeLimited accepts a width Prepare refuses", w)
		}
	}
	for w := 720; w <= 744; w++ {
		check(w, 300, whiteGray(w, 4))
	}
	for w := 1440; w <= 1490; w++ {
		check(w, 600, whiteGray(w, 4))
		// Black column pairs starting at odd x make the halving pick phase 1.
		g := whiteGray(w, 4)
		for y := 0; y < 4; y++ {
			for x := 0; x < w; x++ {
				if x%4 == 1 || x%4 == 2 {
					g[y*w+x] = 0
				}
			}
		}
		check(w, 600, g)
	}
	for _, w := range []int{1446, 1482} {
		data := testutil.EncodeURF(testutil.GrayPage(w, 4, 600, whiteGray(w, 4)))
		if _, err := DecodeLimited(data, rollLimits); !errors.Is(err, ErrWidth) {
			t.Errorf("%d dots at 600 dpi: got %v, want ErrWidth", w, err)
		}
	}
}
