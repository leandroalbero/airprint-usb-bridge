package raster

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// decodeRef is the slice-based decoder the Reader replaced, kept to prove they agree.
func decodeRef(data []byte, lim Limits) ([]Page, error) {
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
		p, next, err := decodePageRef(data, pos, h)
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

func decodePageRef(data []byte, pos int, h pageHeader) (Page, int, error) {
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

func readerDocs() [][]byte {
	text := textGray(9, 300, 40, 2, 1, 0, true)
	g := testutil.GrayPage(300, 40, 300, text)
	return [][]byte{
		testutil.EncodeURF(g, g),
		testutil.EncodeURFCount(0, g, testutil.GrayPage(3, 2, 600, []byte{0, 255, 9, 128, 7, 255})),
		testutil.EncodeURFCount(1, g, g),
		testutil.EncodeURFCount(3, g),
		testutil.EncodeURF(colourPage(2, 300, 40, 600, 32, testutil.URFCMYK, true, text)),
		testutil.EncodePWG(colourPage(3, 300, 40, 300, 24, testutil.PWGRGB, false, text), g),
		testutil.EncodePWG(testutil.Page{Width: 2, Height: 1, XDPI: 600, BPP: 8, ColorSpace: testutil.PWGBlack, Pix: []byte{1, 2}}),
		append(testutil.EncodePWG(g), make([]byte, 1795)...),
		append(testutil.EncodeURF(g), 1, 2, 3),
		{},
		[]byte("UNIRAST\x00"),
		[]byte("UNIRAST\x00\x00"),
		[]byte("RaS2"),
		[]byte("RaS"),
		[]byte("%PDF-1.7"),
	}
}

// TestReaderMatchesSliceDecoder checks every document, and every prefix of the first few, decodes to
// the same pages and the same error as with the old decoder, under several limits.
func TestReaderMatchesSliceDecoder(t *testing.T) {
	limits := []Limits{{}, {MaxGrayBytes: 300 * 40}, {MaxPages: 1}, {WidthMin: 290, WidthMax: 310}}
	docs := readerDocs()
	for i, doc := range docs {
		var cases [][]byte
		if i < 3 || i == 5 {
			for n := 0; n <= len(doc); n += 1 + n/97 {
				cases = append(cases, doc[:n])
			}
		}
		cases = append(cases, doc)
		for _, data := range cases {
			for _, lim := range limits {
				want, werr := decodeRef(data, lim)
				got, gerr := decode(data, lim)
				if fmt.Sprint(werr) != fmt.Sprint(gerr) || !samePages(want, got) {
					t.Fatalf("doc %d len %d %+v: got %d pages %v, want %d pages %v", i, len(data), lim, len(got), gerr, len(want), werr)
				}
				for _, e := range []error{ErrTruncated, ErrTooLarge, ErrUnsupported, ErrNoPages, ErrWidth, ErrResolution} {
					if errors.Is(werr, e) != errors.Is(gerr, e) {
						t.Fatalf("doc %d len %d: errors.Is(%v, %v) differs", i, len(data), gerr, e)
					}
				}
				checkStream(t, data, lim, want, werr)
			}
		}
	}
}

func checkStream(t *testing.T, data []byte, lim Limits, want []Page, werr error) {
	t.Helper()
	perPage := lim
	if lim.MaxGrayBytes > 0 && werr != nil && errors.Is(werr, ErrTooLarge) {
		return // the Reader caps each page, not the document
	}
	for _, skip := range []bool{false, true} {
		r := NewReader(bytes.NewReader(data), perPage)
		var n int
		for {
			var p Page
			var err error
			if skip {
				p, err = r.Skip()
			} else {
				p, err = r.Next()
			}
			if err == io.EOF {
				if werr != nil || n != len(want) {
					t.Fatalf("stream (skip %v) ended after %d pages, want %d, %v", skip, n, len(want), werr)
				}
				break
			}
			if err != nil {
				if werr == nil && !(n == 0 && errors.Is(err, ErrNoPages)) {
					t.Fatalf("stream (skip %v) page %d: %v", skip, n+1, err)
				}
				if werr != nil && fmt.Sprint(err) != fmt.Sprint(werr) {
					t.Fatalf("stream (skip %v): %v, want %v", skip, err, werr)
				}
				if _, again := r.Next(); again != err {
					t.Fatalf("error not sticky: %v then %v", err, again)
				}
				break
			}
			n++
			if werr != nil {
				continue
			}
			w := want[n-1]
			if p.Width != w.Width || p.Height != w.Height || p.XDPI != w.XDPI || p.YDPI != w.YDPI {
				t.Fatalf("stream page %d header %+v", n, p)
			}
			if skip != (p.Gray == nil) || (!skip && !bytes.Equal(p.Gray, w.Gray)) {
				t.Fatalf("stream (skip %v) page %d gray differs", skip, n)
			}
		}
	}
}

func samePages(a, b []Page) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Width != b[i].Width || a[i].Height != b[i].Height || a[i].XDPI != b[i].XDPI ||
			a[i].YDPI != b[i].YDPI || !bytes.Equal(a[i].Gray, b[i].Gray) {
			return false
		}
	}
	return true
}

// TestReaderReusesItsBuffer checks that a long document costs one page of gray, not one per page.
func TestReaderReusesItsBuffer(t *testing.T) {
	g := testutil.GrayPage(300, 40, 300, textGray(9, 300, 40, 2, 1, 0, true))
	pages := make([]testutil.Page, 50)
	for i := range pages {
		pages[i] = g
	}
	r := NewReader(bytes.NewReader(testutil.EncodeURF(pages...)), Limits{MaxGrayBytes: 300 * 40})
	var first *byte
	for i := 0; ; i++ {
		p, err := r.Next()
		if err == io.EOF {
			if i != 50 || r.Pages() != 50 {
				t.Fatalf("%d pages", i)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = &p.Gray[0]
		} else if &p.Gray[0] != first {
			t.Fatalf("page %d got a new buffer", i+1)
		}
	}
}

func FuzzReaderMatchesSliceDecoder(f *testing.F) {
	for _, d := range readerDocs() {
		f.Add(d)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		lim := Limits{MaxGrayBytes: 1 << 20}
		want, werr := decodeRef(data, lim)
		got, gerr := decode(data, lim)
		if fmt.Sprint(werr) != fmt.Sprint(gerr) || !samePages(want, got) {
			t.Fatalf("got %d pages %v, want %d pages %v", len(got), gerr, len(want), werr)
		}
		checkStream(t, data, lim, want, werr)
	})
}
