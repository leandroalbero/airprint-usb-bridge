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
