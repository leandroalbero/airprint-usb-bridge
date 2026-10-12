package imaging

import (
	"image"
	"image/color"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// placeRGB is Place for an RGB page. It never holds more than the page and the sheet: a turned
// or scaled page is read through a rotated view and resampled row by row straight into the sheet,
// with the same arithmetic as the gray path.
func placeRGB(p raster.Page, media driver.Media, o PlaceOptions) (raster.Page, error) {
	if len(p.RGB) != 3*p.Width*p.Height {
		return raster.Page{}, malformed(p)
	}
	pl, err := plan(p, media, o)
	if err != nil {
		return raster.Page{}, err
	}
	if err := o.Budget.Charge(3 * int64(pl.outW) * int64(pl.outH)); err != nil {
		return raster.Page{}, err
	}
	var out raster.Page
	ok := false
	if o.InPlace && pl.quarters == 0 && pl.pw == p.Width && pl.ph == p.Height {
		out, ok = shiftInPlaceRGB(p, pl)
	}
	if !ok {
		n := 3 * pl.outW * pl.outH
		var buf []byte
		if o.Reuse != nil && cap(*o.Reuse) >= n {
			buf = (*o.Reuse)[:n]
		} else {
			if o.Reuse != nil {
				*o.Reuse = nil
			}
			buf = make([]byte, n)
			if o.Reuse != nil {
				*o.Reuse = buf
			}
		}
		out = raster.Page{Width: pl.outW, Height: pl.outH, RGB: buf}
		fill(out.RGB, 255)
		if pl.pw > 0 && pl.ph > 0 {
			drawRGB(&out, p, pl)
		}
	}
	out.XDPI, out.YDPI = o.XDPI, o.YDPI
	if !o.KeepMargins {
		clearMarginsRGB(&out, media.Margins, media.Length == 0)
	}
	return out, nil
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}

// shiftInPlaceRGB is shiftInPlace for three bytes per dot.
func shiftInPlaceRGB(p raster.Page, pl placement) (raster.Page, bool) {
	w, h, outW, outH, ox, oy := p.Width, p.Height, pl.outW, pl.outH, pl.ox, pl.oy
	forward := outW >= w && ox >= 0 && oy >= 0
	back := outW <= w && ox <= 0 && oy <= 0
	if (!forward && !back) || cap(p.RGB) < 3*outW*outH {
		return raster.Page{}, false
	}
	buf := p.RGB[:3*max(w*h, outW*outH)]
	x0, x1 := max(0, -ox), min(w, outW-ox)
	move := func(y int) {
		dy := y + oy
		if dy < 0 || dy >= outH {
			return
		}
		row := buf[3*dy*outW : 3*(dy+1)*outW]
		if x0 >= x1 {
			fill(row, 255)
			return
		}
		copy(row[3*(ox+x0):3*(ox+x1)], buf[3*(y*w+x0):3*(y*w+x1)])
		fill(row[:3*(ox+x0)], 255)
		fill(row[3*(ox+x1):], 255)
	}
	if forward {
		for y := h - 1; y >= 0; y-- {
			move(y)
		}
		fill(buf[:3*min(oy, outH)*outW], 255)
	} else {
		for y := 0; y < h; y++ {
			move(y)
		}
	}
	fill(buf[3*max(0, min(oy+h, outH))*outW:3*outW*outH], 255)
	return raster.Page{Width: outW, Height: outH, RGB: buf[:3*outW*outH]}, true
}

// rotatedRows reads the rows of p turned by quarters counter-clockwise, as Rotate would lay them
// out, without building the turned page.
type rotatedRows struct {
	p        raster.Page
	quarters int
	w, h     int // size of the turned page
	buf      []byte
}

func newRotatedRows(p raster.Page, quarters int) *rotatedRows {
	r := &rotatedRows{p: p, quarters: quarters, w: p.Width, h: p.Height}
	if quarters%2 == 1 {
		r.w, r.h = p.Height, p.Width
	}
	if quarters != 0 {
		r.buf = make([]byte, 3*r.w)
	}
	return r
}

// row returns row y of the turned page; it is valid until the next call.
func (r *rotatedRows) row(y int) []byte {
	p, w := r.p, r.p.Width
	if r.quarters == 0 {
		return p.RGB[3*y*w : 3*(y+1)*w]
	}
	for x := 0; x < r.w; x++ {
		var sx, sy int
		switch r.quarters {
		case 1:
			sx, sy = w-1-y, x
		case 2:
			sx, sy = w-1-x, p.Height-1-y
		case 3:
			sx, sy = y, p.Height-1-x
		}
		copy(r.buf[3*x:3*x+3], p.RGB[3*(sy*w+sx):3*(sy*w+sx)+3])
	}
	return r.buf
}

// drawRGB draws p, turned and scaled as pl says, on the white sheet out. Each output sample is
// the rounded mean of the column means of the source rows it covers, as resample computes it.
func drawRGB(out *raster.Page, p raster.Page, pl placement) {
	src := newRotatedRows(p, pl.quarters)
	pw, ph := pl.pw, pl.ph
	x0, x1 := max(0, -pl.ox), min(pw, out.Width-pl.ox)
	if x0 >= x1 {
		return
	}
	put := func(y int, row []byte) {
		dy := y + pl.oy
		off := 3 * (dy*out.Width + pl.ox)
		copy(out.RGB[off+3*x0:off+3*x1], row[3*x0:3*x1])
	}
	if pw == src.w && ph == src.h {
		for y := 0; y < ph; y++ {
			if dy := y + pl.oy; dy >= 0 && dy < out.Height {
				put(y, src.row(y))
			}
		}
		return
	}
	xs := spans(src.w, pw)
	ys := spans(src.h, ph)
	mid := make([]byte, 3*pw)
	midRow := -1
	sum := make([]int, 3*pw)
	row := make([]byte, 3*pw)
	for y, s := range ys {
		if dy := y + pl.oy; dy < 0 || dy >= out.Height {
			continue
		}
		clear(sum)
		for sy := s[0]; sy < s[1]; sy++ {
			if sy != midRow {
				in := src.row(sy)
				for x, sp := range xs {
					n := sp[1] - sp[0]
					for c := 0; c < 3; c++ {
						t := 0
						for i := sp[0]; i < sp[1]; i++ {
							t += int(in[3*i+c])
						}
						mid[3*x+c] = byte((t + n/2) / n)
					}
				}
				midRow = sy
			}
			for i, v := range mid {
				sum[i] += int(v)
			}
		}
		n := s[1] - s[0]
		for i, t := range sum {
			row[i] = byte((t + n/2) / n)
		}
		put(y, row)
	}
}

func clearMarginsRGB(p *raster.Page, m driver.Margins, sidesOnly bool) {
	left, right := dots(float64(m.Left), p.XDPI), dots(float64(m.Right), p.XDPI)
	top, bottom := dots(float64(m.Top), p.YDPI), dots(float64(m.Bottom), p.YDPI)
	if sidesOnly {
		top, bottom = 0, 0
	}
	w := p.Width
	for y := 0; y < p.Height; y++ {
		row := p.RGB[3*y*w : 3*(y+1)*w]
		if y < top || y >= p.Height-bottom {
			fill(row, 255)
			continue
		}
		fill(row[:3*min(left, w)], 255)
		fill(row[3*max(w-right, 0):], 255)
	}
}

// LooksLikePhotoRGB is LooksLikePhoto for an RGB page: a sample counts as mid-tone when any of
// its channels is.
func LooksLikePhotoRGB(p raster.Page) bool {
	step := max(1, p.Height/200)
	total, mid := 0, 0
	for y := 0; y < p.Height; y += step {
		for x := 0; x < p.Width; x += 4 {
			o := 3 * (y*p.Width + x)
			total++
			for _, v := range p.RGB[o : o+3] {
				if v >= 48 && v <= 207 {
					mid++
					break
				}
			}
		}
	}
	return total > 0 && float64(mid)/float64(total) > 0.05
}

// RGBImage is a view of an RGB page for debug PNGs.
func RGBImage(p raster.Page) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, p.Width, p.Height))
	for y := 0; y < p.Height; y++ {
		for x := 0; x < p.Width; x++ {
			o, d := 3*(y*p.Width+x), y*img.Stride+4*x
			copy(img.Pix[d:d+3], p.RGB[o:o+3])
			img.Pix[d+3] = 0xff
		}
	}
	return img
}

// PageImage is GrayImage or RGBImage, whichever the page holds.
func PageImage(p raster.Page) image.Image {
	if p.RGB != nil {
		return RGBImage(p)
	}
	return GrayImage(p)
}

// FromImageRGB turns an image into an RGB page at dpi, white where transparent.
func FromImageRGB(img image.Image, dpi int) raster.Page {
	r := img.Bounds()
	p := raster.Page{Width: r.Dx(), Height: r.Dy(), XDPI: dpi, YDPI: dpi, RGB: make([]byte, 3*r.Dx()*r.Dy())}
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			c := color.NRGBAModel.Convert(img.At(r.Min.X+x, r.Min.Y+y)).(color.NRGBA)
			o := 3 * (y*p.Width + x)
			for i, v := range []uint8{c.R, c.G, c.B} {
				// Composite on white.
				p.RGB[o+i] = byte((int(v)*int(c.A) + 255*(255-int(c.A)) + 127) / 255)
			}
		}
	}
	return p
}

// Diffuser is Floyd-Steinberg error diffusion of one channel, one row at a time, to 2^bits
// levels. Near 0 (<= 15) and near 255 (>= 240) are clamped first, as FloydSteinberg does for
// gray; at one bit the result is FloydSteinberg's, level 1 being its white.
type Diffuser struct {
	cur, next []int32
	clean     bool // no error is carried into the next row
}

// NewDiffuser diffuses rows of w samples.
func NewDiffuser(w int) *Diffuser {
	return &Diffuser{cur: make([]int32, w+2), next: make([]int32, w+2), clean: true}
}

// Row quantizes the next row of samples in to levels from 0 to 2^bits-1 in out.
func (d *Diffuser) Row(in []byte, bits int, out []byte) {
	top := int32(1)<<bits - 1
	step := 255 * 16 / top // exact for 1, 2 and 4 bits
	if d.clean && len(in) > 0 && (in[0] == 0 || in[0] == 255) && uniform(in) {
		// A blank or solid row with no error coming in quantizes exactly and passes none on.
		l := byte(0)
		if in[0] == 255 {
			l = byte(top)
		}
		for x := range out[:len(in)] {
			out[x] = l
		}
		return
	}
	clear(d.next)
	cur, next := d.cur[:len(in)+2], d.next[:len(in)+2]
	// Errors are kept in sixteenths to stay in integers.
	for x, s := range in {
		g := int32(s)
		if g >= 240 {
			g = 255
		} else if g <= 15 {
			g = 0
		}
		v := g*16 + cur[x+1]
		var q int32
		if bits == 1 {
			// FloydSteinberg's threshold, so one bit diffuses exactly as gray does.
			if v >= 128*16 {
				q = 1
			}
		} else {
			q = min(max((v*top+255*8)/(255*16), 0), top)
		}
		e := v - q*step
		out[x] = byte(q)
		cur[x+2] += e * 7 / 16
		next[x] += e * 3 / 16
		next[x+1] += e * 5 / 16
		next[x+2] += e / 16
	}
	d.cur, d.next = d.next, d.cur
	d.clean = true
	for _, e := range d.cur[1 : len(in)+1] {
		if e != 0 {
			d.clean = false
			break
		}
	}
}

func uniform(b []byte) bool {
	for _, v := range b {
		if v != b[0] {
			return false
		}
	}
	return true
}

// GrayFromRGB turns an RGB page gray in place, (R+G+B)/3 as the decoder does; a gray page is
// returned as it is. Drivers use it for an RGB page in a job that does not print in colour.
func GrayFromRGB(p raster.Page) raster.Page {
	if p.RGB == nil {
		return p
	}
	n := p.Width * p.Height
	g := p.RGB[:n]
	for i := 0; i < n; i++ {
		g[i] = byte((int(p.RGB[3*i]) + int(p.RGB[3*i+1]) + int(p.RGB[3*i+2])) / 3)
	}
	p.Gray, p.RGB = g, nil
	return p
}
