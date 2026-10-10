package raster

import (
	"errors"
	"fmt"

	"github.com/leandroalbero/ql-airprint/internal/ql"
)

var (
	ErrResolution = errors.New("raster: unsupported resolution")
	ErrWidth      = errors.New("raster: page width does not match the roll")
)

type Options struct {
	Pins     int    // 696 for 62 mm
	WidthMin int    // 724, at 300 dpi
	WidthMax int    // 740
	TrimMax  int    // per end, rows at 300 dpi; <0 = unlimited
	Dither   string // "auto" | "on" | "off"; "" means auto
	HighRes  bool   // keep 600 dpi along the feed when the page is 600 dpi
}

type Label struct {
	Bitmap   *ql.Bitmap // Pins wide
	HighRes  bool       // true when rows are 600 dpi along the feed
	Dropped  int        // black dots cropped off the sides
	Dithered bool
	Blank    bool // nothing left after trim; caller skips it
}

// Prepare converts one decoded page into a label bitmap. Only XDPI decides the
// 300/600 handling, because the reference reads a single resolution per page.
func Prepare(p Page, o Options) (Label, error) {
	if o.Pins <= 0 {
		return Label{}, fmt.Errorf("raster: pins must be positive, got %d", o.Pins)
	}
	mode := o.Dither
	switch mode {
	case "":
		mode = "auto"
	case "auto", "on", "off":
	default:
		return Label{}, fmt.Errorf("raster: unknown dither mode %q", o.Dither)
	}
	if p.Width <= 0 || p.Height <= 0 || len(p.Gray) != p.Width*p.Height {
		return Label{}, fmt.Errorf("raster: malformed page %dx%d with %d gray bytes", p.Width, p.Height, len(p.Gray))
	}
	if p.XDPI != 300 && p.XDPI != 600 {
		return Label{}, fmt.Errorf("%w: %d dpi, expected 300 or 600", ErrResolution, p.XDPI)
	}

	gray, w, h := p.Gray, p.Width, p.Height
	highRes := false
	if p.XDPI == 600 {
		gray, w, h = halve(gray, w, h, !o.HighRes)
		highRes = o.HighRes
	}
	if w < o.WidthMin || w > o.WidthMax {
		return Label{}, fmt.Errorf("%w: %d dots wide at 300 dpi, expected %d-%d", ErrWidth, w, o.WidthMin, o.WidthMax)
	}

	var bits []byte
	dithered := mode == "on" || (mode == "auto" && looksLikePhoto(gray, w, h))
	if dithered {
		bits = floydSteinberg(gray, w, h)
	} else {
		bits = threshold(gray)
	}

	trim := o.TrimMax
	if highRes && trim > 0 {
		trim *= 2
	}
	bm, dropped, blank := labelBitmap(bits, w, h, o.Pins, trim)
	return Label{Bitmap: bm, HighRes: highRes, Dropped: dropped, Dithered: dithered, Blank: blank}, nil
}

// pairPhase picks the 600→300 pairing offset that splits the fewest black/white
// edges; pairing across an edge turns it grey and fattens strokes by a dot.
// at(l, i) is sample i of sampled line l; each line is n samples long.
func pairPhase(lines int, n int, at func(line, i int) byte) int {
	var mixed [2]int
	for phase := 0; phase < 2; phase++ {
		for l := 0; l < lines; l++ {
			for i := phase; i < n-1; i += 2 {
				if (at(l, i) < 128) != (at(l, i+1) < 128) {
					mixed[phase]++
				}
			}
		}
	}
	if mixed[0] <= mixed[1] {
		return 0
	}
	return 1
}

// paddedAt indexes a line with one white sample prepended when phase is 1 and
// white beyond the end, so odd edges pair with paper.
func paddedAt(get func(int) byte, n, phase, k int) int {
	k -= phase
	if k < 0 || k >= n {
		return 255
	}
	return int(get(k))
}

func halve(gray []byte, w, h int, rowsToo bool) ([]byte, int, int) {
	rstep := max(1, h/64)
	sampled := (h + rstep - 1) / rstep
	cphase := pairPhase(sampled, w, func(l, i int) byte { return gray[l*rstep*w+i] })

	nw := (w + cphase) / 2
	cols := make([]byte, nw*h)
	for y := 0; y < h; y++ {
		line := gray[y*w : (y+1)*w]
		get := func(k int) byte { return line[k] }
		out := cols[y*nw : (y+1)*nw]
		for i := range out {
			out[i] = byte((paddedAt(get, w, cphase, 2*i) + paddedAt(get, w, cphase, 2*i+1)) / 2)
		}
	}
	if !rowsToo {
		return cols, nw, h
	}

	cstep := max(1, nw/64)
	csampled := (nw + cstep - 1) / cstep
	rphase := pairPhase(csampled, h, func(l, i int) byte { return cols[i*nw+l*cstep] })

	nh := (h + rphase) / 2
	out := make([]byte, nw*nh)
	for x := 0; x < nw; x++ {
		get := func(k int) byte { return cols[k*nw+x] }
		for i := 0; i < nh; i++ {
			out[i*nw+x] = byte((paddedAt(get, h, rphase, 2*i) + paddedAt(get, h, rphase, 2*i+1)) / 2)
		}
	}
	return out, nw, nh
}

// looksLikePhoto keeps text and QR codes pixel-exact while photos, which are
// mostly mid-tones, get error diffusion instead of a blotchy threshold.
func looksLikePhoto(gray []byte, w, h int) bool {
	step := max(1, h/200)
	total, mid := 0, 0
	for y := 0; y < h; y += step {
		for x := 0; x < w; x += 4 {
			v := gray[y*w+x]
			total++
			if v >= 48 && v <= 207 {
				mid++
			}
		}
	}
	return total > 0 && float64(mid)/float64(total) > 0.05
}

func threshold(gray []byte) []byte {
	bits := make([]byte, len(gray))
	for i, v := range gray {
		if v < 128 {
			bits[i] = 1
		}
	}
	return bits
}

// floydSteinberg must round exactly like the reference's Python floats; the
// explicit float64 conversions stop the compiler fusing multiply-adds.
func floydSteinberg(gray []byte, w, h int) []byte {
	bits := make([]byte, w*h)
	errRow := make([]float64, w+2)
	nxt := make([]float64, w+2)
	for y := 0; y < h; y++ {
		clear(nxt)
		carry := 0.0
		for x := 0; x < w; x++ {
			g := int(gray[y*w+x])
			if g >= 240 {
				g = 255
			} else if g <= 15 {
				g = 0
			}
			v := float64(float64(g)+errRow[x+1]) + carry
			black := v < 128
			var e float64
			if black {
				bits[y*w+x] = 1
				e = v
			} else {
				e = v - 255
			}
			carry = float64(e*7) / 16
			nxt[x] += float64(e*3) / 16
			nxt[x+1] += float64(e*5) / 16
			nxt[x+2] += e / 16
		}
		errRow, nxt = nxt, errRow
	}
	return bits
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// labelBitmap centre-crops (or pads) to pins and trims white rows from both
// ends, at most trim per end; trim < 0 is unlimited.
func labelBitmap(bits []byte, w, h, pins, trim int) (*ql.Bitmap, int, bool) {
	off := floorDiv(w-pins, 2)
	dropped := 0
	if off >= 0 {
		for y := 0; y < h; y++ {
			row := bits[y*w : (y+1)*w]
			for x, v := range row {
				if x < off || x >= off+pins {
					dropped += int(v)
				}
			}
		}
	}
	src := func(y, x int) byte {
		sx := x + off
		if sx < 0 || sx >= w {
			return 0
		}
		return bits[y*w+sx]
	}
	blankRow := func(y int) bool {
		for x := 0; x < pins; x++ {
			if src(y, x) != 0 {
				return false
			}
		}
		return true
	}
	if trim < 0 {
		trim = h
	}
	top := 0
	for top < min(trim, h) && blankRow(top) {
		top++
	}
	bottom := h
	for h-bottom < trim && bottom > top && blankRow(bottom-1) {
		bottom--
	}
	bm := ql.NewBitmap(pins, bottom-top)
	blank := true
	for y := top; y < bottom; y++ {
		for x := 0; x < pins; x++ {
			if src(y, x) != 0 {
				bm.Bits[(y-top)*pins+x] = true
				blank = false
			}
		}
	}
	return bm, dropped, blank
}
