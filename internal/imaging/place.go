package imaging

import (
	"fmt"
	"math"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// PlaceOptions say how a page goes onto a sheet.
type PlaceOptions struct {
	XDPI, YDPI int // output resolution
	// Orientation is the job's orientation-requested. Clients send raster pages already laid
	// out, so a page is only turned when its shape disagrees with the sheet (landscape page,
	// portrait sheet or the reverse); 5 (reverse-landscape) turns it clockwise, anything else
	// counter-clockwise. 6 (reverse-portrait) turns a page that needs no quarter turn by 180°.
	Orientation int
	// Scaling is the job's print-scaling: "none", "fit" (into the printable area), "fill" (cover
	// the sheet, cropping), "auto-fit" (fit when larger than the printable area, else none) or
	// "auto"/"" (none when the page is the sheet size within 3%, else auto-fit).
	Scaling string
	// KeepMargins leaves the unprintable margins as they come; by default they are cleared to
	// white so nothing is sent where the printer cannot print.
	KeepMargins bool
	// Budget, when non-nil, is charged for the sheet before it is allocated (normally the job's
	// JobInfo.Budget). Place then fails with driver.ErrJobTooLarge instead of running out of
	// memory on a tiny low-resolution page that scales up to a huge sheet.
	Budget *driver.Budget
	// InPlace lets Place draw the sheet over p.Gray (p.RGB) and return it, when the page goes
	// on the sheet 1:1 and the buffer's capacity holds the sheet; the caller must not use p
	// afterwards.
	InPlace bool
	// Color makes CheckPlace count three bytes per dot, for a page that will be decoded as RGB
	// (the check pass sees pages without pixels). Place goes by the page it is given.
	Color bool
	// Reuse, when set, is where Place draws an RGB sheet it cannot place in place: the buffer
	// is used when large enough and kept there otherwise. A driver streaming pages keeps one
	// per job, so turned or scaled colour pages do not each leave a 100 MB sheet to the
	// collector. The previous sheet drawn there is overwritten.
	Reuse *[]byte
}

// Place scales and positions p on a sheet of media's size at the output resolution and returns
// the whole sheet, white where the page does not reach. A media.Length of 0 (a roll of unknown
// length) makes the sheet as long as the placed page.
//
// When the scale is 1 and the resolutions match, pixels are copied exactly; otherwise each output
// pixel is the mean of the source pixels it covers (or the nearest one when enlarging).
func Place(p raster.Page, media driver.Media, o PlaceOptions) (raster.Page, error) {
	if p.RGB != nil {
		return placeRGB(p, media, o)
	}
	if len(p.Gray) != p.Width*p.Height {
		return raster.Page{}, malformed(p)
	}
	pl, err := plan(p, media, o)
	if err != nil {
		return raster.Page{}, err
	}
	if err := o.Budget.Charge(int64(pl.outW) * int64(pl.outH)); err != nil {
		return raster.Page{}, err
	}
	owned := o.InPlace
	if pl.quarters != 0 {
		p = Rotate(p, pl.quarters)
		owned = true
	}
	out, ok := raster.Page{}, false
	if owned && pl.pw == p.Width && pl.ph == p.Height {
		out, ok = shiftInPlace(p, pl)
		out.XDPI, out.YDPI = o.XDPI, o.YDPI
	}
	if !ok {
		out = raster.Page{Width: pl.outW, Height: pl.outH, XDPI: o.XDPI, YDPI: o.YDPI, Gray: make([]byte, pl.outW*pl.outH)}
		for i := range out.Gray {
			out.Gray[i] = 255
		}
		if pl.pw > 0 && pl.ph > 0 {
			blit(&out, resample(p, pl.pw, pl.ph), pl.ox, pl.oy)
		}
	}
	if !o.KeepMargins {
		clearMargins(&out, media.Margins, media.Length == 0)
	}
	return out, nil
}

// shiftInPlace places p at 1:1 over its own pixels, when the sheet fits in p.Gray's capacity and
// every row moves the same way (all forward, or all back), so no row is overwritten before it
// has moved. It gives what blitting p on a white sheet gives.
func shiftInPlace(p raster.Page, pl placement) (raster.Page, bool) {
	w, h, outW, outH, ox, oy := p.Width, p.Height, pl.outW, pl.outH, pl.ox, pl.oy
	forward := outW >= w && ox >= 0 && oy >= 0
	back := outW <= w && ox <= 0 && oy <= 0
	if (!forward && !back) || cap(p.Gray) < outW*outH {
		return raster.Page{}, false
	}
	buf := p.Gray[:max(w*h, outW*outH)]
	x0, x1 := max(0, -ox), min(w, outW-ox)
	white := func(b []byte) {
		for i := range b {
			b[i] = 255
		}
	}
	move := func(y int) {
		dy := y + oy
		if dy < 0 || dy >= outH {
			return
		}
		row := buf[dy*outW : (dy+1)*outW]
		if x0 >= x1 {
			white(row)
			return
		}
		copy(row[ox+x0:ox+x1], buf[y*w+x0:y*w+x1])
		white(row[:ox+x0])
		white(row[ox+x1:])
	}
	if forward {
		for y := h - 1; y >= 0; y-- {
			move(y)
		}
		white(buf[:min(oy, outH)*outW])
	} else {
		for y := 0; y < h; y++ {
			move(y)
		}
	}
	white(buf[max(0, min(oy+h, outH))*outW : outW*outH])
	return raster.Page{Width: outW, Height: outH, Gray: buf[:outW*outH]}, true
}

// CheckPlace reports the error Place would return for a page of p's size and resolution, without
// placing it; p.Gray may be nil. The sheet is checked against the whole Budget, not charged:
// a driver that streams pages releases each sheet before placing the next.
func CheckPlace(p raster.Page, media driver.Media, o PlaceOptions) error {
	if (p.Gray != nil && len(p.Gray) != p.Width*p.Height) || (p.RGB != nil && len(p.RGB) != 3*p.Width*p.Height) {
		return malformed(p)
	}
	pl, err := plan(p, media, o)
	if err != nil {
		return err
	}
	return o.Budget.Check(int64(pl.outW) * int64(pl.outH) * int64(channels(p, o)))
}

// channels is the bytes per dot of the sheet p becomes.
func channels(p raster.Page, o PlaceOptions) int {
	if p.RGB != nil || o.Color {
		return 3
	}
	return 1
}

func malformed(p raster.Page) error {
	if p.RGB != nil {
		return fmt.Errorf("imaging: malformed page %dx%d @%dx%d with %d RGB bytes", p.Width, p.Height, p.XDPI, p.YDPI, len(p.RGB))
	}
	return fmt.Errorf("imaging: malformed page %dx%d @%dx%d with %d gray bytes", p.Width, p.Height, p.XDPI, p.YDPI, len(p.Gray))
}

// placement is where Place puts a page: turned by quarters, scaled to pw x ph and drawn at
// (ox, oy) on an outW x outH sheet.
type placement struct {
	quarters       int
	pw, ph, ox, oy int
	outW, outH     int
}

func plan(p raster.Page, media driver.Media, o PlaceOptions) (placement, error) {
	var pl placement
	if p.Width <= 0 || p.Height <= 0 || p.XDPI <= 0 || p.YDPI <= 0 {
		return pl, malformed(p)
	}
	if o.XDPI <= 0 || o.YDPI <= 0 || media.Width <= 0 || media.Length < 0 {
		return pl, fmt.Errorf("imaging: bad target %dx%d dpi on %dx%d", o.XDPI, o.YDPI, media.Width, media.Length)
	}

	pageLandscape := float64(p.Width)/float64(p.XDPI) > float64(p.Height)/float64(p.YDPI)
	sheetLandscape := media.Length > 0 && media.Width > media.Length
	switch {
	case media.Length > 0 && pageLandscape != sheetLandscape:
		if o.Orientation == 5 {
			pl.quarters = 3
		} else {
			pl.quarters = 1
		}
	case o.Orientation == 6:
		pl.quarters = 2
	}
	w, h, xdpi, ydpi := p.Width, p.Height, p.XDPI, p.YDPI
	if pl.quarters%2 == 1 {
		w, h, xdpi, ydpi = h, w, ydpi, xdpi
	}

	srcW := float64(w) / float64(xdpi) * 2540 // hundredths of a millimetre
	srcH := float64(h) / float64(ydpi) * 2540
	sheetW, sheetH := float64(media.Width), float64(media.Length)
	m := media.Margins
	printW, printH := sheetW-float64(m.Left+m.Right), sheetH-float64(m.Top+m.Bottom)
	if media.Length == 0 {
		sheetH, printH = math.Inf(1), math.Inf(1)
	}

	scale := 1.0
	mode := o.Scaling
	if mode == "" || mode == "auto" {
		mode = "auto-fit"
		if media.Length > 0 && within(srcW, sheetW, 0.03) && within(srcH, sheetH, 0.03) {
			mode = "none"
		}
	}
	switch mode {
	case "none":
	case "fit":
		scale = math.Min(printW/srcW, printH/srcH)
	case "auto-fit":
		if srcW > printW+1 || srcH > printH+1 {
			scale = math.Min(printW/srcW, printH/srcH)
		}
	case "fill":
		scale = math.Max(sheetW/srcW, sheetH/srcH)
		if media.Length == 0 {
			scale = sheetW / srcW
		}
	default:
		return pl, fmt.Errorf("imaging: print-scaling %q", o.Scaling)
	}

	pl.outW, pl.outH = SheetDots(media, o.XDPI, o.YDPI)
	placedW := srcW * scale
	placedH := srcH * scale
	var offX, offY float64 // where the page's top-left lands, hundredths of a millimetre
	if media.Length == 0 {
		pl.outH = max(dots(placedH, o.YDPI), 1)
	}
	if mode == "fit" || (mode == "auto-fit" && scale != 1) {
		offX = float64(m.Left) + (printW-placedW)/2
		offY = float64(m.Top) + (printH-placedH)/2
	} else {
		offX = (sheetW - placedW) / 2
		offY = (sheetH - placedH) / 2
	}
	if media.Length == 0 {
		offY = 0
	}

	pl.ox, pl.oy = int(math.Round(offX*float64(o.XDPI)/2540)), int(math.Round(offY*float64(o.YDPI)/2540))
	pl.pw, pl.ph = int(math.Round(placedW*float64(o.XDPI)/2540)), int(math.Round(placedH*float64(o.YDPI)/2540))
	if scale == 1 && xdpi == o.XDPI && ydpi == o.YDPI {
		pl.pw, pl.ph = w, h
	}
	if pl.pw > 0 && pl.ph > 0 && (pl.pw != w || pl.ph != h) {
		// resample's buffers are transient, so they are checked but not charged; a page
		// placed at "none" or "fill" may be far larger than the sheet it is cropped to.
		if err := o.Budget.Check(int64(pl.pw) * int64(max(pl.ph, h)) * int64(channels(p, o))); err != nil {
			return pl, err
		}
	}
	return pl, nil
}

// SheetDots is the size of the sheet Place returns for media at xdpi by ydpi. A roll's height
// (media.Length 0) depends on the page and is returned as 0.
func SheetDots(media driver.Media, xdpi, ydpi int) (w, h int) {
	return dots(float64(media.Width), xdpi), dots(float64(media.Length), ydpi)
}

func within(a, b, frac float64) bool { return math.Abs(a-b) <= b*frac }

func dots(hmm float64, dpi int) int { return int(math.Round(hmm * float64(dpi) / 2540)) }

// resample scales p to w x h: a box filter when shrinking, nearest neighbour when enlarging.
func resample(p raster.Page, w, h int) raster.Page {
	if w == p.Width && h == p.Height {
		return p
	}
	xs := spans(p.Width, w)
	ys := spans(p.Height, h)
	// Columns first, into a p.Height x w intermediate.
	mid := make([]byte, p.Height*w)
	for y := 0; y < p.Height; y++ {
		row := p.Gray[y*p.Width : (y+1)*p.Width]
		for x, s := range xs {
			sum := 0
			for _, v := range row[s[0]:s[1]] {
				sum += int(v)
			}
			mid[y*w+x] = byte((sum + (s[1]-s[0])/2) / (s[1] - s[0]))
		}
	}
	out := raster.Page{Width: w, Height: h, XDPI: p.XDPI, YDPI: p.YDPI, Gray: make([]byte, w*h)}
	for y, s := range ys {
		for x := 0; x < w; x++ {
			sum := 0
			for sy := s[0]; sy < s[1]; sy++ {
				sum += int(mid[sy*w+x])
			}
			out.Gray[y*w+x] = byte((sum + (s[1]-s[0])/2) / (s[1] - s[0]))
		}
	}
	return out
}

// spans maps each of n output samples to the [start, end) source samples it covers.
func spans(src, n int) [][2]int {
	out := make([][2]int, n)
	f := float64(src) / float64(n)
	for i := range out {
		a := int(math.Floor(float64(i) * f))
		b := int(math.Floor(float64(i+1) * f))
		if f <= 1 || b <= a {
			a = min(int(math.Floor((float64(i)+0.5)*f)), src-1)
			b = a + 1
		}
		out[i] = [2]int{a, min(b, src)}
	}
	return out
}

func blit(dst *raster.Page, src raster.Page, ox, oy int) {
	for y := 0; y < src.Height; y++ {
		dy := y + oy
		if dy < 0 || dy >= dst.Height {
			continue
		}
		x0, x1 := max(0, -ox), min(src.Width, dst.Width-ox)
		if x0 >= x1 {
			continue
		}
		copy(dst.Gray[dy*dst.Width+ox+x0:dy*dst.Width+ox+x1], src.Gray[y*src.Width+x0:y*src.Width+x1])
	}
}

func clearMargins(p *raster.Page, m driver.Margins, sidesOnly bool) {
	left, right := dots(float64(m.Left), p.XDPI), dots(float64(m.Right), p.XDPI)
	top, bottom := dots(float64(m.Top), p.YDPI), dots(float64(m.Bottom), p.YDPI)
	if sidesOnly {
		top, bottom = 0, 0
	}
	for y := 0; y < p.Height; y++ {
		row := p.Gray[y*p.Width : (y+1)*p.Width]
		if y < top || y >= p.Height-bottom {
			for x := range row {
				row[x] = 255
			}
			continue
		}
		for x := 0; x < min(left, p.Width); x++ {
			row[x] = 255
		}
		for x := max(p.Width-right, 0); x < p.Width; x++ {
			row[x] = 255
		}
	}
}

// Rotate turns p by quarter turns counter-clockwise (1 = 90°, 2 = 180°, 3 = 90° clockwise).
func Rotate(p raster.Page, quarters int) raster.Page {
	quarters = ((quarters % 4) + 4) % 4
	if quarters == 0 {
		return p
	}
	w, h := p.Width, p.Height
	out := raster.Page{Width: w, Height: h, XDPI: p.XDPI, YDPI: p.YDPI, Gray: make([]byte, w*h)}
	if quarters != 2 {
		out.Width, out.Height, out.XDPI, out.YDPI = h, w, p.YDPI, p.XDPI
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := p.Gray[y*w+x]
			switch quarters {
			case 1: // (x, y) -> (y, w-1-x)
				out.Gray[(w-1-x)*h+y] = v
			case 2:
				out.Gray[(h-1-y)*w+(w-1-x)] = v
			case 3: // (x, y) -> (h-1-y, x)
				out.Gray[x*h+(h-1-y)] = v
			}
		}
	}
	return out
}
