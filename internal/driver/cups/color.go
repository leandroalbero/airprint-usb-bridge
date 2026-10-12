package cups

import (
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// component is what one channel of a raster format carries, computed from an sRGB pixel.
type component uint8

const (
	compGray    component = iota // (R+G+B)/3, 255 = white
	compInkGray                  // 255 - gray: K of a one-channel black space
	compR
	compG
	compB
	compC // 255 - R, no black generation (CMY, YMC)
	compM
	compY
	compCK // 255 - R - K, with K = min(255-R, 255-G, 255-B) (CMYK family)
	compMK
	compYK
	compK
)

// components lists the channels of a colour space in stream order.
func components(space uint32) []component {
	switch space {
	case CSpaceW, CSpaceSW:
		return []component{compGray}
	case CSpaceK:
		return []component{compInkGray}
	case CSpaceRGB, CSpaceSRGB, CSpaceAdobeRGB:
		return []component{compR, compG, compB}
	case CSpaceCMY:
		return []component{compC, compM, compY}
	case CSpaceYMC:
		return []component{compY, compM, compC}
	case CSpaceCMYK:
		return []component{compCK, compMK, compYK, compK}
	case CSpaceYMCK:
		return []component{compYK, compMK, compCK, compK}
	case CSpaceKCMY:
		return []component{compK, compCK, compMK, compYK}
	}
	return nil
}

// values computes component c of a row of RGB pixels into v.
func (c component) values(rgb, v []byte) {
	switch c {
	case compGray, compInkGray:
		for x := range v {
			g := byte((int(rgb[3*x]) + int(rgb[3*x+1]) + int(rgb[3*x+2])) / 3)
			if c == compInkGray {
				g = 255 - g
			}
			v[x] = g
		}
	case compR, compG, compB:
		i := int(c - compR)
		for x := range v {
			v[x] = rgb[3*x+i]
		}
	case compC, compM, compY:
		i := int(c - compC)
		for x := range v {
			v[x] = 255 - rgb[3*x+i]
		}
	case compCK, compMK, compYK:
		i := int(c - compCK)
		for x := range v {
			o := 3 * x
			k := 255 - max(rgb[o], rgb[o+1], rgb[o+2])
			v[x] = 255 - rgb[o+i] - k
		}
	case compK:
		for x := range v {
			o := 3 * x
			v[x] = 255 - max(rgb[o], rgb[o+1], rgb[o+2])
		}
	}
}

// colorPage encodes one placed sheet into the raster format of h, a line at a time, so the
// page's raster is never held whole. The page is the w x h window at (x0, y0) of the sheet,
// white where the window leaves it. Sheet pixels may be RGB or gray.
type colorPage struct {
	h     *Header
	f     format
	sheet raster.Page
	pix   []byte
	ch    int
	x0    int
	y0    int
	w     int
	ht    int
	comps []component
	bpc   int

	dither []*imaging.Diffuser // per channel, when dithering
	line   int                 // next line in stream order
	rgb    []byte              // the current source row as RGB
	vals   [][]byte            // per channel 8-bit values of the current row
	levels [][]byte            // per channel samples of the current row, below 8 bits
	bw     bitWriter
}

func newColorPage(h *Header, f format, sheet raster.Page, x0, y0 int, dithered bool) *colorPage {
	pix, ch := sheet.Pix()
	e := &colorPage{
		h: h, f: f, sheet: sheet, pix: pix, ch: ch, x0: x0, y0: y0,
		w: int(h.CupsWidth), ht: int(h.CupsHeight), comps: components(h.CupsColorSpace), bpc: int(h.CupsBitsPerColor),
	}
	e.rgb = make([]byte, 3*e.w)
	e.vals = make([][]byte, len(e.comps))
	e.levels = make([][]byte, len(e.comps))
	for i := range e.vals {
		e.vals[i] = make([]byte, e.w)
		e.levels[i] = make([]byte, e.w)
	}
	if dithered && e.bpc < 8 {
		e.dither = make([]*imaging.Diffuser, len(e.comps))
		for i := range e.dither {
			e.dither[i] = imaging.NewDiffuser(e.w)
		}
	}
	return e
}

// done reports whether every line has been encoded.
func (e *colorPage) done() bool { return e.line >= e.f.rows }

// source fills e.rgb with row y of the page window.
func (e *colorPage) source(y int) {
	fill := func(b []byte) {
		for i := range b {
			b[i] = 255
		}
	}
	sy := e.y0 + y
	if sy < 0 || sy >= e.sheet.Height {
		fill(e.rgb)
		return
	}
	lo, hi := max(e.x0, 0), min(e.x0+e.w, e.sheet.Width)
	if lo >= hi {
		fill(e.rgb)
		return
	}
	fill(e.rgb[:3*(lo-e.x0)])
	fill(e.rgb[3*(hi-e.x0):])
	row := e.pix[e.ch*(sy*e.sheet.Width+lo) : e.ch*(sy*e.sheet.Width+hi)]
	dst := e.rgb[3*(lo-e.x0) : 3*(hi-e.x0)]
	if e.ch == 3 {
		copy(dst, row)
		return
	}
	for i, v := range row {
		dst[3*i], dst[3*i+1], dst[3*i+2] = v, v, v
	}
}

// quantize computes channel c of the current source row: 8-bit values in e.vals[c] and, below
// 8 bits, samples in e.levels[c], dithered or cut to their top bits as the gray path does.
func (e *colorPage) quantize(c int) {
	vals := e.vals[c]
	e.comps[c].values(e.rgb, vals)
	if e.bpc >= 8 {
		return
	}
	out := e.levels[c]
	if e.dither != nil {
		e.dither[c].Row(vals, e.bpc, out)
		return
	}
	shift := 8 - e.bpc
	for x, v := range vals {
		out[x] = v >> shift
	}
}

// putRow appends channel c of the current row, packed, as one band or plane line.
func (e *colorPage) putRow(dst []byte, c int) []byte {
	switch e.bpc {
	case 8:
		return append(dst, e.vals[c]...)
	case 16:
		for _, v := range e.vals[c] {
			dst = append16(dst, uint16(v)*257)
		}
		return dst
	}
	per := 8 / e.bpc
	l := e.levels[c]
	for x := 0; x < e.w; x += per {
		var b byte
		for k := 0; k < per; k++ {
			b <<= e.bpc
			if x+k < e.w {
				b |= l[x+k]
			}
		}
		dst = append(dst, b)
	}
	return dst
}

// next appends lines to dst until it holds at least min bytes or the page is done.
func (e *colorPage) next(dst []byte, min int) []byte {
	colors := len(e.comps)
	for !e.done() && len(dst) < min {
		switch e.h.CupsColorOrder {
		case OrderChunked:
			e.source(e.line)
			for c := 0; c < colors; c++ {
				e.quantize(c)
			}
			switch e.bpc {
			case 8:
				for x := 0; x < e.w; x++ {
					for c := 0; c < colors; c++ {
						dst = append(dst, e.vals[c][x])
					}
				}
			case 16:
				for x := 0; x < e.w; x++ {
					for c := 0; c < colors; c++ {
						dst = append16(dst, uint16(e.vals[c][x])*257)
					}
				}
			default:
				e.bw.start(dst, e.f.bytesPerRow)
				for x := 0; x < e.w; x++ {
					for c := 0; c < colors; c++ {
						e.bw.put(uint32(e.levels[c][x]), e.bpc)
					}
				}
				dst = e.bw.end()
			}
		case OrderBanded:
			e.source(e.line)
			for c := 0; c < colors; c++ {
				e.quantize(c)
				dst = e.putRow(dst, c)
			}
		case OrderPlanar:
			c, y := e.line/e.ht, e.line%e.ht
			e.source(y)
			e.quantize(c)
			dst = e.putRow(dst, c)
		}
		e.line++
	}
	return dst
}

// append16 appends a 16-bit sample in the stream's byte order.
func append16(dst []byte, v uint16) []byte {
	var two [2]byte
	byteOrder.PutUint16(two[:], v)
	return append(dst, two[:]...)
}

// flipRGB is flip for an RGB page.
func flipRGB(p raster.Page, x, y bool) {
	w, h := p.Width, p.Height
	if x {
		for r := 0; r < h; r++ {
			row := p.RGB[3*r*w : 3*(r+1)*w]
			for i, j := 0, w-1; i < j; i, j = i+1, j-1 {
				a, b := row[3*i:3*i+3], row[3*j:3*j+3]
				a[0], a[1], a[2], b[0], b[1], b[2] = b[0], b[1], b[2], a[0], a[1], a[2]
			}
		}
	}
	if y {
		tmp := make([]byte, 3*w)
		for i, j := 0, h-1; i < j; i, j = i+1, j-1 {
			a, b := p.RGB[3*i*w:3*(i+1)*w], p.RGB[3*j*w:3*(j+1)*w]
			copy(tmp, a)
			copy(a, b)
			copy(b, tmp)
		}
	}
}
