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
	reserve int // bytes the page buffer is first made for, see Reserve

	started bool
	urf     bool
	count   uint32 // pages a URF header announces; 0 = unknown
	n       int
	err     error
	colored bool

	hdr      []byte
	raw, row []byte
	rgb      []byte // a row in RGB, to look for colour
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

// Colored reports whether a page read or skipped so far has a pixel that is not gray: R, G and B
// not all equal. A colour format whose pixels are all gray is not colour.
func (d *Reader) Colored() bool { return d.colored }

// channels is the bytes per decoded pixel.
func (d *Reader) channels() int {
	if d.lim.RGB {
		return 3
	}
	return 1
}

// Reserve makes the page buffer, when Next first needs one, large enough for pages of n bytes
// (decoded, as Gray or RGB), so pages that grow reuse it instead of each leaving a smaller
// buffer behind for the collector. It allocates n bytes whatever the document holds, so it is
// for a document already read through, whose largest page is known to be n bytes.
func (d *Reader) Reserve(n int) { d.reserve = min(n, d.maxGray) }

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
	ch := d.channels()
	if h.height > (d.maxGray-used)/(h.width*ch) {
		what := "gray"
		if ch == 3 {
			what = "RGB"
		}
		return Page{}, fmt.Errorf("%w: page %d needs %d bytes of %s, over the %d byte budget", ErrTooLarge, n, h.width*h.height*ch, what, d.maxGray)
	}
	pix, err := d.body(h, skip)
	if err != nil {
		return Page{}, fmt.Errorf("page %d: %w", n, err)
	}
	d.total += h.width * h.height * ch
	d.n = n
	p := Page{Width: h.width, Height: h.height, XDPI: h.xdpi, YDPI: h.ydpi}
	if ch == 3 {
		p.RGB = pix
	} else {
		p.Gray = pix
	}
	return p, nil
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
	ch := d.channels()
	rowLen := h.width * ch
	if cap(d.row) < rowLen {
		d.row = make([]byte, rowLen)
	}
	raw, row, px := d.raw[:need], d.row[:rowLen], d.px[:bypp]
	check := h.bpp > 8 && !d.colored
	if check && cap(d.rgb) < 3*h.width {
		d.rgb = make([]byte, 3*h.width)
	}
	var fill byte = 0xff
	if h.subtractive {
		fill = 0x00
	}
	size := rowLen * h.height
	limit := size
	if !d.keep {
		// Spare room lets a driver place a page on a sheet a few dots larger without a
		// second page-sized buffer (imaging.PlaceOptions.InPlace).
		limit += size / 64
	}
	var gray []byte
	if !skip {
		switch {
		case !d.keep && cap(d.gray) >= size:
			gray = d.gray[:0]
		case !d.keep && d.reserve >= size:
			d.gray = nil
			gray = make([]byte, 0, d.reserve+d.reserve/64)
		default:
			d.gray = nil
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
		// Skip still needs the pixels while it looks for colour.
		fillRaw := !skip || check
		n := 0
		for n < need {
			b, err := d.r.ReadByte()
			if err != nil {
				return nil, truncated(err)
			}
			switch {
			case b == 128:
				if fillRaw {
					for i := n; i < need; i++ {
						raw[i] = fill
					}
				}
				n = need
			case b > 128:
				// Runs may overshoot the line; the excess is consumed and dropped.
				k := (257 - int(b)) * bypp
				m := min(k, need-n)
				if !fillRaw {
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
				if !fillRaw {
					n += min((int(b)+1)*bypp, need-n)
					continue
				}
				for i := 0; i <= int(b) && n < need; i++ {
					n += copy(raw[n:], px)
				}
			}
		}
		y += rep
		if check && !gray3(d.rgb[:3*h.width], raw, bypp, h.subtractive) {
			d.colored, check = true, false
		}
		if skip {
			continue
		}
		if ch == 3 {
			toRGB(row, raw, bypp, h.subtractive)
		} else {
			toGray(row, raw, bypp, h.subtractive)
		}
		if len(gray)+rep*rowLen > cap(gray) {
			want := max(2*cap(gray), len(gray)+rep*rowLen)
			if ch == 3 {
				// Doubling an RGB page leaves as much garbage as the page itself; once a
				// megabyte of it is real, the rest is taken at once.
				want = limit
			}
			grown := make([]byte, len(gray), min(limit, want))
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

// gray3 converts a row of raw pixels to RGB in rgb and reports whether every pixel is gray.
func gray3(rgb, raw []byte, bypp int, subtractive bool) bool {
	if subtractive && bypp == 4 {
		// The gray decode ignores K (toGray), so only the colour path prints a CMYK page's K.
		for i := 3; i < len(raw); i += 4 {
			if raw[i] != 0 {
				return false
			}
		}
	}
	toRGB(rgb, raw, bypp, subtractive)
	for i := 0; i < len(rgb); i += 3 {
		if rgb[i] != rgb[i+1] || rgb[i+1] != rgb[i+2] {
			return false
		}
	}
	return true
}
