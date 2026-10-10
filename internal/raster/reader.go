package raster

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Reader decodes a URF or PWG raster stream one page at a time, so a document is never held
// in memory: only the page being converted is. Limits apply as in DecodeLimited, except that
// MaxGrayBytes caps each page instead of the whole document.
type Reader struct {
	r   *bufio.Reader
	lim Limits
	// keep gives every page its own buffer and charges MaxGrayBytes over all pages (Decode);
	// otherwise Next reuses one buffer.
	keep    bool
	maxGray int
	total   int

	started bool
	urf     bool
	count   uint32 // pages a URF header announces; 0 = unknown
	n       int
	err     error

	hdr      []byte
	raw, row []byte
	px       [4]byte
	gray     []byte
}

// NewReader reads the document from r.
func NewReader(r io.Reader, lim Limits) *Reader { return newReader(r, lim, false) }

func newReader(r io.Reader, lim Limits, keep bool) *Reader {
	maxGray := lim.MaxGrayBytes
	if maxGray <= 0 || maxGray > MaxGrayBytes {
		maxGray = MaxGrayBytes
	}
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), lim: lim, keep: keep, maxGray: maxGray}
}

// Pages is the number of pages read or skipped so far.
func (d *Reader) Pages() int { return d.n }

// Next decodes the next page. Its Gray is reused by the following call to Next, so a caller
// that keeps a page must copy it; the caller may overwrite it. After the last page Next returns
// io.EOF, or ErrNoPages when the document has none. Errors are sticky.
func (d *Reader) Next() (Page, error) { return d.next(false) }

// Skip reads past the next page, checking its header and data exactly as Next does, without
// building its pixels: the returned Page has a nil Gray.
func (d *Reader) Skip() (Page, error) { return d.next(true) }

func (d *Reader) next(skip bool) (Page, error) {
	if d.err != nil {
		return Page{}, d.err
	}
	p, err := d.page(skip)
	if err != nil {
		d.err = err
		return Page{}, err
	}
	return p, nil
}

func (d *Reader) start() error {
	magic, err := d.r.Peek(len(urfMagic))
	switch {
	case bytes.HasPrefix(magic, []byte(urfMagic)):
		d.urf = true
		var b [12]byte
		if _, err := io.ReadFull(d.r, b[:]); err != nil {
			return truncated(err)
		}
		d.count = binary.BigEndian.Uint32(b[8:12])
		d.hdr = make([]byte, urfHeaderLen)
	case bytes.HasPrefix(magic, []byte(pwgMagic)):
		if _, err := d.r.Discard(len(pwgMagic)); err != nil {
			return err
		}
		d.hdr = make([]byte, pwgHeaderLen)
	case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull):
		return err
	default:
		return ErrUnsupported
	}
	return nil
}

// end is what reading past the last page returns.
func (d *Reader) end() error {
	if d.urf && d.count != 0 && uint32(d.n) < d.count {
		return fmt.Errorf("%w: header announces %d pages, found %d", ErrTruncated, d.count, d.n)
	}
	if d.n == 0 {
		return ErrNoPages
	}
	return io.EOF
}

func (d *Reader) page(skip bool) (Page, error) {
	if !d.started {
		d.started = true
		if err := d.start(); err != nil {
			return Page{}, err
		}
	}
	if d.urf && d.count != 0 && uint32(d.n) >= d.count {
		return Page{}, d.end()
	}
	// A trailer shorter than a page header is ignored, as it always was.
	if _, err := io.ReadFull(d.r, d.hdr); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Page{}, d.end()
		}
		return Page{}, err
	}
	var h pageHeader
	if d.urf {
		b := d.hdr
		dpi := int(binary.BigEndian.Uint32(b[20:24]))
		h = pageHeader{
			bpp:         int(b[0]),
			width:       int(binary.BigEndian.Uint32(b[12:16])),
			height:      int(binary.BigEndian.Uint32(b[16:20])),
			xdpi:        dpi,
			ydpi:        dpi,
			subtractive: b[1] == 6,
		}
	} else {
		b := d.hdr
		cs := binary.BigEndian.Uint32(b[400:404])
		h = pageHeader{
			bpp:         int(binary.BigEndian.Uint32(b[388:392])),
			width:       int(binary.BigEndian.Uint32(b[372:376])),
			height:      int(binary.BigEndian.Uint32(b[376:380])),
			xdpi:        int(binary.BigEndian.Uint32(b[276:280])),
			ydpi:        int(binary.BigEndian.Uint32(b[280:284])),
			subtractive: cs == 3 || cs == 6,
		}
	}
	n := d.n + 1
	if d.lim.MaxPages > 0 && d.n >= d.lim.MaxPages {
		return Page{}, fmt.Errorf("%w: more than %d pages", ErrTooLarge, d.lim.MaxPages)
	}
	if err := checkHeader(h, n); err != nil {
		return Page{}, err
	}
	if err := d.lim.check(h, n); err != nil {
		return Page{}, err
	}
	used := 0
	if d.keep {
		used = d.total
	}
	if h.height > (d.maxGray-used)/h.width {
		return Page{}, fmt.Errorf("%w: page %d needs %d bytes of gray, over the %d byte budget", ErrTooLarge, n, h.width*h.height, d.maxGray)
	}
	gray, err := d.body(h, skip)
	if err != nil {
		return Page{}, fmt.Errorf("page %d: %w", n, err)
	}
	d.total += h.width * h.height
	d.n = n
	return Page{Width: h.width, Height: h.height, XDPI: h.xdpi, YDPI: h.ydpi, Gray: gray}, nil
}

func truncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return err
}

// body decodes the rows of one page: a line-repeat byte, then runs of pixels until the line is
// full. With skip it only consumes and checks the data.
func (d *Reader) body(h pageHeader, skip bool) ([]byte, error) {
	bypp := h.bpp / 8
	need := h.width * bypp
	if cap(d.raw) < need {
		d.raw = make([]byte, need)
	}
	if cap(d.row) < h.width {
		d.row = make([]byte, h.width)
	}
	raw, row, px := d.raw[:need], d.row[:h.width], d.px[:bypp]
	var fill byte = 0xff
	if h.subtractive {
		fill = 0x00
	}
	size := h.width * h.height
	limit := size
	if !d.keep {
		// Spare room lets a driver place a page on a sheet a few dots larger without a
		// second page-sized buffer (imaging.PlaceOptions.InPlace).
		limit += size / 64
	}
	var gray []byte
	if !skip {
		if !d.keep && cap(d.gray) >= size {
			gray = d.gray[:0]
		} else {
			// Grown with the data, so a header that lies about its size cannot make a
			// short document allocate it.
			gray = make([]byte, 0, min(size, 1<<20))
		}
	}
	for y := 0; y < h.height; {
		c, err := d.r.ReadByte()
		if err != nil {
			return nil, truncated(err)
		}
		rep := min(int(c)+1, h.height-y)
		n := 0
		for n < need {
			b, err := d.r.ReadByte()
			if err != nil {
				return nil, truncated(err)
			}
			switch {
			case b == 128:
				if !skip {
					for i := n; i < need; i++ {
						raw[i] = fill
					}
				}
				n = need
			case b > 128:
				// Runs may overshoot the line; the excess is consumed and dropped.
				k := (257 - int(b)) * bypp
				m := min(k, need-n)
				if skip {
					m = 0
				}
				if _, err := io.ReadFull(d.r, raw[n:n+m]); err != nil {
					return nil, truncated(err)
				}
				if _, err := d.r.Discard(k - m); err != nil {
					return nil, truncated(err)
				}
				n += min(k, need-n)
			default:
				if _, err := io.ReadFull(d.r, px); err != nil {
					return nil, truncated(err)
				}
				if skip {
					n += min((int(b)+1)*bypp, need-n)
					continue
				}
				for i := 0; i <= int(b) && n < need; i++ {
					n += copy(raw[n:], px)
				}
			}
		}
		y += rep
		if skip {
			continue
		}
		toGray(row, raw, bypp, h.subtractive)
		if len(gray)+rep*h.width > cap(gray) {
			grown := make([]byte, len(gray), min(limit, max(2*cap(gray), len(gray)+rep*h.width)))
			copy(grown, gray)
			gray = grown
		}
		for i := 0; i < rep; i++ {
			gray = append(gray, row...)
		}
	}
	if !skip && !d.keep {
		d.gray = gray
	}
	return gray, nil
}
