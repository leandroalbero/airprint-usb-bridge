// Package raster turns URF and PWG raster documents into label bitmaps for the
// QL encoder.
package raster

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	ErrUnsupported = errors.New("raster: unsupported document")
	ErrTruncated   = errors.New("raster: truncated document")
	ErrTooLarge    = errors.New("raster: page too large")
	ErrNoPages     = errors.New("raster: no pages")
)

const (
	MaxWidth     = 20000
	MaxHeight    = 200000
	MaxGrayBytes = 512 << 20

	urfMagic     = "UNIRAST\x00"
	pwgMagic     = "RaS2"
	urfHeaderLen = 32
	pwgHeaderLen = 1796
)

// Page is one decoded page in 8-bit gray.
type Page struct {
	Width, Height int
	XDPI, YDPI    int
	Gray          []byte // Width*Height, 0 = black, 255 = white
}

type pageHeader struct {
	bpp, width, height, xdpi, ydpi int
	subtractive                    bool
}

// Decode returns every page of a URF or PWG raster stream. It rejects unknown formats,
// truncated data and absurd sizes (cap: 20000x200000 px, and 512 MiB of decoded gray) with errors.
func Decode(data []byte) ([]Page, error) {
	return decode(data, Limits{})
}

// Limits tightens DecodeLimited. Zero fields keep Decode's caps and skip the checks.
type Limits struct {
	MaxGrayBytes int // decoded gray over all pages; <= 0 means MaxGrayBytes
	// WidthMin and WidthMax are the page widths Prepare accepts, in dots at
	// 300 dpi. When set, every page header must also say 300 or 600 dpi.
	WidthMin, WidthMax int
	// MaxPages caps the page count; <= 0 means no cap. Pages of a few pixels cost little
	// gray, so the gray cap alone lets a document hold millions of them.
	MaxPages int
}

// DecodeLimited is Decode with caps sized for the printer. A page that Prepare
// would refuse for its width or resolution fails with ErrWidth or ErrResolution
// from its header, before a highly compressed body is expanded.
func DecodeLimited(data []byte, lim Limits) ([]Page, error) {
	return decode(data, lim)
}

func (lim Limits) check(h pageHeader, n int) error {
	if lim.WidthMin <= 0 && lim.WidthMax <= 0 {
		return nil
	}
	lo, hi := lim.WidthMin, lim.WidthMax
	switch h.xdpi {
	case 300:
	case 600:
		// Halving yields (w+phase)/2 columns with phase 0 or 1.
		lo, hi = 2*lo-1, 2*hi+1
	default:
		return fmt.Errorf("page %d: %w: %d dpi, expected 300 or 600", n, ErrResolution, h.xdpi)
	}
	if (lim.WidthMin > 0 && h.width < lo) || (lim.WidthMax > 0 && h.width > hi) {
		return fmt.Errorf("page %d: %w: %d dots wide at %d dpi", n, ErrWidth, h.width, h.xdpi)
	}
	return nil
}

func decode(data []byte, lim Limits) ([]Page, error) {
	maxGray := lim.MaxGrayBytes
	if maxGray <= 0 || maxGray > MaxGrayBytes {
		maxGray = MaxGrayBytes
	}
	var pages []Page
	total := 0
	add := func(h pageHeader, pos int) (int, error) {
		if lim.MaxPages > 0 && len(pages) >= lim.MaxPages {
			return 0, fmt.Errorf("%w: more than %d pages", ErrTooLarge, lim.MaxPages)
		}
		if err := checkHeader(h, len(pages)+1); err != nil {
			return 0, err
		}
		if err := lim.check(h, len(pages)+1); err != nil {
			return 0, err
		}
		if h.height > (maxGray-total)/h.width {
			return 0, fmt.Errorf("%w: page %d needs %d bytes of gray, over the %d byte budget", ErrTooLarge, len(pages)+1, h.width*h.height, maxGray)
		}
		p, next, err := decodePage(data, pos, h)
		if err != nil {
			return 0, fmt.Errorf("page %d: %w", len(pages)+1, err)
		}
		total += h.width * h.height
		pages = append(pages, p)
		return next, nil
	}

	switch {
	case bytes.HasPrefix(data, []byte(urfMagic)):
		if len(data) < 12 {
			return nil, ErrTruncated
		}
		count := binary.BigEndian.Uint32(data[8:12])
		pos := 12
		for pos+urfHeaderLen <= len(data) && (count == 0 || uint32(len(pages)) < count) {
			h := data[pos : pos+urfHeaderLen]
			dpi := int(binary.BigEndian.Uint32(h[20:24]))
			ph := pageHeader{
				bpp:         int(h[0]),
				width:       int(binary.BigEndian.Uint32(h[12:16])),
				height:      int(binary.BigEndian.Uint32(h[16:20])),
				xdpi:        dpi,
				ydpi:        dpi,
				subtractive: h[1] == 6,
			}
			next, err := add(ph, pos+urfHeaderLen)
			if err != nil {
				return nil, err
			}
			pos = next
		}
		if count != 0 && uint32(len(pages)) < count {
			return nil, fmt.Errorf("%w: header announces %d pages, found %d", ErrTruncated, count, len(pages))
		}
	case bytes.HasPrefix(data, []byte(pwgMagic)):
		pos := 4
		for pos+pwgHeaderLen <= len(data) {
			h := data[pos : pos+pwgHeaderLen]
			cs := binary.BigEndian.Uint32(h[400:404])
			ph := pageHeader{
				bpp:         int(binary.BigEndian.Uint32(h[388:392])),
				width:       int(binary.BigEndian.Uint32(h[372:376])),
				height:      int(binary.BigEndian.Uint32(h[376:380])),
				xdpi:        int(binary.BigEndian.Uint32(h[276:280])),
				ydpi:        int(binary.BigEndian.Uint32(h[280:284])),
				subtractive: cs == 3 || cs == 6,
			}
			next, err := add(ph, pos+pwgHeaderLen)
			if err != nil {
				return nil, err
			}
			pos = next
		}
	default:
		return nil, ErrUnsupported
	}
	if len(pages) == 0 {
		return nil, ErrNoPages
	}
	return pages, nil
}

func checkHeader(h pageHeader, n int) error {
	if h.bpp != 8 && h.bpp != 24 && h.bpp != 32 {
		return fmt.Errorf("%w: page %d has %d bits per pixel", ErrUnsupported, n, h.bpp)
	}
	if h.width <= 0 || h.height <= 0 {
		return fmt.Errorf("%w: page %d is %dx%d", ErrUnsupported, n, h.width, h.height)
	}
	if h.width > MaxWidth || h.height > MaxHeight {
		return fmt.Errorf("%w: page %d is %dx%d, cap %dx%d", ErrTooLarge, n, h.width, h.height, MaxWidth, MaxHeight)
	}
	return nil
}

func decodePage(data []byte, pos int, h pageHeader) (Page, int, error) {
	bypp := h.bpp / 8
	need := h.width * bypp
	raw := make([]byte, need)
	row := make([]byte, h.width)
	var fill byte = 0xff
	if h.subtractive {
		fill = 0x00
	}
	size := h.width * h.height
	gray := make([]byte, 0, min(size, 1<<20))
	for y := 0; y < h.height; {
		if pos >= len(data) {
			return Page{}, 0, ErrTruncated
		}
		rep := int(data[pos]) + 1
		pos++
		n := 0
		for n < need {
			if pos >= len(data) {
				return Page{}, 0, ErrTruncated
			}
			b := data[pos]
			pos++
			switch {
			case b == 128:
				for ; n < need; n++ {
					raw[n] = fill
				}
			case b > 128:
				// Runs may overshoot the line; the excess is consumed and dropped.
				k := (257 - int(b)) * bypp
				if pos+k > len(data) {
					return Page{}, 0, ErrTruncated
				}
				n += copy(raw[n:], data[pos:pos+k])
				pos += k
			default:
				if pos+bypp > len(data) {
					return Page{}, 0, ErrTruncated
				}
				px := data[pos : pos+bypp]
				pos += bypp
				for i := 0; i <= int(b) && n < need; i++ {
					n += copy(raw[n:], px)
				}
			}
		}
		toGray(row, raw, bypp, h.subtractive)
		rep = min(rep, h.height-y)
		if len(gray)+rep*h.width > cap(gray) {
			grown := make([]byte, len(gray), min(size, max(2*cap(gray), len(gray)+rep*h.width)))
			copy(grown, gray)
			gray = grown
		}
		for i := 0; i < rep; i++ {
			gray = append(gray, row...)
		}
		y += rep
	}
	return Page{Width: h.width, Height: h.height, XDPI: h.xdpi, YDPI: h.ydpi, Gray: gray}, pos, nil
}

// toGray averages the first three channels only, so the K of CMYK and the
// fourth byte of 32-bit additive pixels are ignored, as the reference does.
func toGray(dst, raw []byte, bypp int, subtractive bool) {
	for x := range dst {
		var v byte
		if bypp == 1 {
			v = raw[x]
		} else {
			o := x * bypp
			v = byte((int(raw[o]) + int(raw[o+1]) + int(raw[o+2])) / 3)
		}
		if subtractive {
			v = 255 - v
		}
		dst[x] = v
	}
}
