// Package raster turns URF and PWG raster documents into label bitmaps for the
// QL encoder.
package raster

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// Page is one decoded page: 8-bit gray, or 8-bit sRGB when it was decoded with Limits.RGB.
type Page struct {
	Width, Height int
	XDPI, YDPI    int
	Gray          []byte // Width*Height, 0 = black, 255 = white; nil on an RGB page
	RGB           []byte // Width*Height*3, R G B per pixel, 255 = full intensity; nil on a gray page
}

// IsRGB reports whether the page holds RGB pixels.
func (p Page) IsRGB() bool { return p.RGB != nil }

// Pix is the page's pixels, Gray or RGB, and their bytes per pixel.
func (p Page) Pix() ([]byte, int) {
	if p.RGB != nil {
		return p.RGB, 3
	}
	return p.Gray, 1
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

// Limits tightens DecodeLimited and Reader. Zero fields keep Decode's caps and skip the checks.
type Limits struct {
	// MaxGrayBytes is the decoded gray over all pages for DecodeLimited, and per page for a
	// Reader; <= 0 means MaxGrayBytes.
	MaxGrayBytes int
	// WidthMin and WidthMax are the page widths Prepare accepts, in dots at
	// 300 dpi. When set, every page header must also say 300 or 600 dpi.
	WidthMin, WidthMax int
	// MaxPages caps the page count; <= 0 means no cap. Pages of a few pixels cost little
	// gray, so the gray cap alone lets a document hold millions of them.
	MaxPages int
	// RGB decodes every page to Page.RGB instead of Page.Gray, and MaxGrayBytes then counts
	// three bytes per pixel. Gray pages become R=G=B, CMYK is inverted naively.
	RGB bool
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

// DecodeFrom is DecodeLimited reading the document from r, so it is never held in memory next
// to its decoded pages.
func DecodeFrom(r io.Reader, lim Limits) ([]Page, error) {
	return decodeFrom(r, lim)
}

func decode(data []byte, lim Limits) ([]Page, error) {
	return decodeFrom(bytes.NewReader(data), lim)
}

func decodeFrom(src io.Reader, lim Limits) ([]Page, error) {
	r := newReader(src, lim, true)
	var pages []Page
	for {
		p, err := r.Next()
		if err == io.EOF {
			return pages, nil
		}
		if err != nil {
			return nil, err
		}
		pages = append(pages, p)
	}
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

// toRGB converts one row of raw pixels to sRGB. Every RGB flavour is taken as sRGB; CMYK is
// R = 255 - min(255, C+K) and so on, with no colour management.
func toRGB(dst, raw []byte, bypp int, subtractive bool) {
	n := len(dst) / 3
	switch {
	case bypp == 1:
		for x := 0; x < n; x++ {
			v := raw[x]
			if subtractive {
				v = 255 - v
			}
			dst[3*x], dst[3*x+1], dst[3*x+2] = v, v, v
		}
	case subtractive && bypp == 4:
		for x := 0; x < n; x++ {
			o := 4 * x
			k := int(raw[o+3])
			for c := 0; c < 3; c++ {
				dst[3*x+c] = byte(255 - min(255, int(raw[o+c])+k))
			}
		}
	default:
		for x := 0; x < n; x++ {
			o := x * bypp
			r, g, b := raw[o], raw[o+1], raw[o+2]
			if subtractive {
				r, g, b = 255-r, 255-g, 255-b
			}
			dst[3*x], dst[3*x+1], dst[3*x+2] = r, g, b
		}
	}
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
