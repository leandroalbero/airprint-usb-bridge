package cups

import (
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// encodePage appends the uncompressed v3 pixel data of one page to dst. p is the page already
// cut to cupsWidth x cupsHeight; bm is its 1-bit form (needed only when cupsBitsPerColor is 1).
// Input is gray, so colour spaces with a black channel print gray as black ink only.
func encodePage(dst []byte, h *Header, f format, p raster.Page, bm *imaging.Bitmap) []byte {
	w, hgt := p.Width, p.Height
	bpc := int(h.CupsBitsPerColor)

	if f.colors == 1 && bpc == 1 {
		for y := 0; y < hgt; y++ {
			row := bm.Row(y)
			if f.additive {
				for _, b := range row {
					dst = append(dst, ^b)
				}
			} else {
				dst = append(dst, row...)
			}
		}
		return dst
	}
	if f.colors == 1 && bpc == 8 {
		for y := 0; y < hgt; y++ {
			row := p.Gray[y*w : (y+1)*w]
			if f.additive {
				dst = append(dst, row...)
			} else {
				for _, g := range row {
					dst = append(dst, 255-g)
				}
			}
		}
		return dst
	}

	sample := func(c, x, y int) uint32 {
		if bpc == 1 {
			black := bm.At(x, y)
			switch {
			case f.additive:
				black = !black
			case f.blackFirst >= 0 && c != f.blackFirst:
				black = false
			}
			if black {
				return 1
			}
			return 0
		}
		v := uint32(p.Gray[y*w+x])
		switch {
		case f.additive:
		case f.blackFirst >= 0 && c != f.blackFirst:
			v = 0
		default:
			v = 255 - v
		}
		if bpc == 16 {
			return v * 257
		}
		return v >> (8 - bpc)
	}

	var bw bitWriter
	switch h.CupsColorOrder {
	case OrderChunked:
		for y := 0; y < hgt; y++ {
			bw.start(dst, f.bytesPerRow)
			for x := 0; x < w; x++ {
				for c := 0; c < f.colors; c++ {
					bw.put(sample(c, x, y), bpc)
				}
			}
			dst = bw.end()
		}
	case OrderBanded:
		for y := 0; y < hgt; y++ {
			for c := 0; c < f.colors; c++ {
				bw.start(dst, f.bytesPerRow/f.colors)
				for x := 0; x < w; x++ {
					bw.put(sample(c, x, y), bpc)
				}
				dst = bw.end()
			}
		}
	case OrderPlanar:
		for c := 0; c < f.colors; c++ {
			for y := 0; y < hgt; y++ {
				bw.start(dst, f.bytesPerRow)
				for x := 0; x < w; x++ {
					bw.put(sample(c, x, y), bpc)
				}
				dst = bw.end()
			}
		}
	}
	return dst
}

// bitWriter packs samples MSB first into a row of fixed length appended to a buffer; 16-bit
// samples are written in byteOrder, as libcups expects them in the stream's order.
type bitWriter struct {
	buf      []byte
	rowStart int
	n        int
	acc      uint32
	nacc     int
}

func (b *bitWriter) start(dst []byte, n int) {
	b.buf, b.rowStart, b.n, b.acc, b.nacc = dst, len(dst), n, 0, 0
}

func (b *bitWriter) put(v uint32, bits int) {
	if bits == 16 {
		var two [2]byte
		byteOrder.PutUint16(two[:], uint16(v))
		b.buf = append(b.buf, two[:]...)
		return
	}
	if bits == 8 {
		b.buf = append(b.buf, byte(v))
		return
	}
	b.acc = b.acc<<bits | v&(1<<bits-1)
	b.nacc += bits
	if b.nacc == 8 {
		b.buf = append(b.buf, byte(b.acc))
		b.acc, b.nacc = 0, 0
	}
}

func (b *bitWriter) end() []byte {
	if b.nacc > 0 {
		b.buf = append(b.buf, byte(b.acc<<(8-b.nacc)))
		b.acc, b.nacc = 0, 0
	}
	for len(b.buf)-b.rowStart < b.n {
		b.buf = append(b.buf, 0)
	}
	return b.buf
}

// crop cuts the w x h window at (x0, y0) out of p, white where the window leaves p.
func crop(p raster.Page, x0, y0, w, h int) raster.Page {
	out := raster.Page{Width: w, Height: h, XDPI: p.XDPI, YDPI: p.YDPI, Gray: make([]byte, w*h)}
	for i := range out.Gray {
		out.Gray[i] = 255
	}
	for y := 0; y < h; y++ {
		sy := y0 + y
		if sy < 0 || sy >= p.Height {
			continue
		}
		lo, hi := max(x0, 0), min(x0+w, p.Width)
		if lo >= hi {
			continue
		}
		copy(out.Gray[y*w+lo-x0:], p.Gray[sy*p.Width+lo:sy*p.Width+hi])
	}
	return out
}

// cropInPlace is crop reusing p's pixels when the window lies inside p; p is then spent.
func cropInPlace(p raster.Page, x0, y0, w, h int) raster.Page {
	if x0 < 0 || y0 < 0 || x0+w > p.Width || y0+h > p.Height {
		return crop(p, x0, y0, w, h)
	}
	// Each row moves to an earlier or equal index, and never over a row still to be moved.
	for y := 0; y < h; y++ {
		copy(p.Gray[y*w:(y+1)*w], p.Gray[(y0+y)*p.Width+x0:(y0+y)*p.Width+x0+w])
	}
	return raster.Page{Width: w, Height: h, XDPI: p.XDPI, YDPI: p.YDPI, Gray: p.Gray[:w*h]}
}

// flip mirrors p in place across the vertical axis (x), the horizontal axis (y), or both (a
// half turn), as CUPS drivers with *cupsBackSide expect of duplex back pages.
func flip(p raster.Page, x, y bool) {
	w, h := p.Width, p.Height
	if x {
		for r := 0; r < h; r++ {
			row := p.Gray[r*w : (r+1)*w]
			for i, j := 0, w-1; i < j; i, j = i+1, j-1 {
				row[i], row[j] = row[j], row[i]
			}
		}
	}
	if y {
		tmp := make([]byte, w)
		for i, j := 0, h-1; i < j; i, j = i+1, j-1 {
			a, b := p.Gray[i*w:(i+1)*w], p.Gray[j*w:(j+1)*w]
			copy(tmp, a)
			copy(a, b)
			copy(b, tmp)
		}
	}
}
