package pcl

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
)

const uel = "\x1b%-12345X"

type jobHeader struct {
	name          string
	pjl           bool
	dpi           int
	code          int // ESC &l#A page size
	duplex        int // ESC &l#S: 0 simplex, 1 long edge, 2 short edge
	duplexCapable bool
	color         bool // 24-bit RGB raster
}

// cidRGB24 is Configure Image Data, short form: Device RGB, direct by pixel, 8 bits per primary
// (PCL 5 Color Technical Reference, "Configure Image Data", mode 3 example).
const cidRGB24 = "\x1b*v6W\x00\x03\x00\x08\x08\x08"

// prologue starts the job: PJL header, reset and the job's page settings.
func prologue(h jobHeader) []byte {
	var b bytes.Buffer
	if h.pjl {
		fmt.Fprintf(&b, "%s@PJL JOB NAME=\"%s\"\r\n", uel, h.name)
		fmt.Fprintf(&b, "@PJL SET RESOLUTION=%d\r\n", h.dpi)
		b.WriteString("@PJL ENTER LANGUAGE=PCL\r\n")
	}
	b.WriteString("\x1bE")
	fmt.Fprintf(&b, "\x1b&l%dA\x1b&l0O", h.code)
	// A duplex printer may default to two-sided, so one-sided jobs say so too.
	if h.duplexCapable {
		fmt.Fprintf(&b, "\x1b&l%dS", h.duplex)
	}
	// Page size and orientation reset the text margins; set these after them.
	b.WriteString("\x1b&l0L\x1b&l0E")
	fmt.Fprintf(&b, "\x1b*t%dR", h.dpi)
	if h.color {
		b.WriteString(cidRGB24)
	}
	return b.Bytes()
}

// epilogue ends the job, after the last page or after a failure between pages.
func epilogue(h jobHeader) []byte {
	var b bytes.Buffer
	b.WriteString("\x1bE")
	if h.pjl {
		fmt.Fprintf(&b, "%s@PJL EOJ NAME=\"%s\"\r\n%s", uel, h.name, uel)
	}
	return b.Bytes()
}

// writer encodes pages as PCL raster graphics with one compression mode.
type writer struct {
	compression int
	seed        []byte
	buf         []byte
	out         []byte // the last page, reused for the next
}

// page encodes one logical page: cursor at the top-left corner, the raster rows, form feed.
// Trailing blank rows are not sent; other blank runs become ESC *b#Y.
func (w *writer) page(bm *imaging.Bitmap) []byte {
	b := w.out[:0]
	b = append(b, "\x1b*p0x0Y"...)
	b = append(b, "\x1b*r"+strconv.Itoa(bm.W)+"S"...)
	b = append(b, "\x1b*b"+strconv.Itoa(w.compression)+"M"...)
	b = append(b, "\x1b*r1A"...)
	w.seed = make([]byte, bm.Stride)
	last := -1
	for y := bm.H - 1; y >= 0; y-- {
		if !bm.RowBlank(y) {
			last = y
			break
		}
	}
	skip := 0
	for y := 0; y <= last; y++ {
		row := bm.Row(y)
		if bm.RowBlank(y) && (w.compression == 2 || allZero(w.seed)) {
			skip++
			continue
		}
		if skip > 0 {
			b = append(b, "\x1b*b"+strconv.Itoa(skip)+"Y"...)
			skip = 0
		}
		data := w.row(row)
		b = append(b, "\x1b*b"+strconv.Itoa(len(data))+"W"...)
		b = append(b, data...)
	}
	b = append(b, "\x1b*rB\f"...)
	w.out = b
	return b
}

// row compresses one row. Mode 3 rows are relative to the previous row sent, so blank rows are
// only skipped with ESC *b#Y while that seed is all zero: then it does not matter whether the
// printer clears the seed on a Y offset.
func (w *writer) row(row []byte) []byte {
	w.buf = w.buf[:0]
	if w.compression == 3 {
		w.buf = deltaRow(w.buf, row, w.seed)
		copy(w.seed, row)
		return w.buf
	}
	n := len(row)
	for n > 0 && row[n-1] == 0 {
		n--
	}
	return imaging.PackBits(w.buf, row[:n])
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// deltaRow appends row encoded with PCL compression mode 3 against seed (same length). Each
// command byte holds a replacement count of 1-8 in its top three bits (count-1) and an offset in
// its low five bits, counted from the byte after the previous replacement. An offset of 31 or
// more is 31 followed by extra bytes that add up the rest; a 255 means another byte follows.
// Unchanged bytes cost nothing, so a row equal to the seed is empty.
func deltaRow(dst, row, seed []byte) []byte {
	next := 0
	for i := 0; i < len(row); {
		if row[i] == seed[i] {
			i++
			continue
		}
		start := i
		for i < len(row) && i-start < 8 && row[i] != seed[i] {
			i++
		}
		off := start - next
		cmd := byte(i-start-1) << 5
		if off < 31 {
			dst = append(dst, cmd|byte(off))
		} else {
			dst = append(dst, cmd|31)
			for off -= 31; off >= 255; off -= 255 {
				dst = append(dst, 255)
			}
			dst = append(dst, byte(off))
		}
		dst = append(dst, row[start:i]...)
		next = i
	}
	return dst
}
