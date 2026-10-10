package postscript

import (
	"bytes"
	"compress/zlib"
	"encoding/ascii85"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
)

// settings are the per-printer encoding choices (driver options).
type settings struct {
	Level  int  // LanguageLevel: 2 uses RunLengthDecode, 3 FlateDecode
	Depth  int  // bits per sample: 1 or 8
	Binary bool // raw filter data instead of ASCII85
	Duplex bool // the printer has a duplexer: always state /Duplex
	PJL    bool
}

// docInfo is what one job asks for.
type docInfo struct {
	Title     string
	Width     int // sheet, 1/100 mm
	Length    int
	Sides     string
	NumCopies int // > 1 sets /NumCopies
}

const (
	creator = "airprint-usb-bridge"
	procImg = "aubimg"
	uel     = "\x1b%-12345X"
	// a85Line keeps ASCII85 lines well under the DSC limit of 255 characters.
	a85Line = 76
)

// pageImage is one sheet: packed rows of 1-bit samples (1 = black, rows padded to a byte) or
// 8-bit gray (0 = black), covering the whole sheet at XDPI x YDPI.
type pageImage struct {
	W, H       int
	XDPI, YDPI int
	Depth      int
	Data       []byte
}

// docWriter appends a DSC-conforming LanguageLevel 2/3 document to a buffer, page by page, so a
// job never holds more than one placed sheet at a time.
type docWriter struct {
	buf   *bytes.Buffer
	set   settings
	doc   docInfo
	pages int
	n     int
}

func pt(hmm int) float64 { return float64(hmm) * 72 / 2540 }

// num formats a PostScript real with at most two decimals.
func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

func newDocWriter(buf *bytes.Buffer, set settings, doc docInfo, pages int) *docWriter {
	w := &docWriter{buf: buf, set: set, doc: doc, pages: pages}
	if set.PJL {
		buf.WriteString(uel + "@PJL JOB\r\n@PJL ENTER LANGUAGE = POSTSCRIPT\r\n")
	}
	data := "Clean7Bit"
	if set.Binary {
		data = "Binary"
	}
	fmt.Fprintf(buf, "%%!PS-Adobe-3.0\n%%%%Creator: %s\n", creator)
	if t := dscText(doc.Title); t != "" {
		fmt.Fprintf(buf, "%%%%Title: %s\n", t)
	}
	fmt.Fprintf(buf, "%%%%LanguageLevel: %d\n%%%%DocumentData: %s\n%%%%Pages: %d\n%%%%EndComments\n", set.Level, data, pages)

	buf.WriteString("%%BeginProlog\n/aub 8 dict def\n/" + procImg + " { % width height bits " + procImg + " -, samples follow\n")
	buf.WriteString("  aub begin /b exch def /h exch def /w exch def\n")
	src := "currentfile"
	if !set.Binary {
		buf.WriteString("  /s currentfile /ASCII85Decode filter def\n")
		src = "s"
	}
	fmt.Fprintf(buf, "  /d %s /%s filter def\n", src, set.decodeFilter())
	buf.WriteString("  /DeviceGray setcolorspace\n")
	buf.WriteString("  << /ImageType 1 /Width w /Height h /BitsPerComponent b\n")
	buf.WriteString("     /Decode b 1 eq { [1 0] } { [0 1] } ifelse\n")
	buf.WriteString("     /ImageMatrix [w 0 0 h neg 0 h] /DataSource d >> image\n")
	// image stops reading once it has its samples, which can leave the filters short of their
	// EOD markers; whatever is left would then be scanned as PostScript code.
	filters := "  d flushfile"
	if !set.Binary {
		filters += " s flushfile"
	}
	buf.WriteString(filters + "\n  end\n} bind def\n%%EndProlog\n")

	buf.WriteString("%%BeginSetup\n")
	feature(buf, fmt.Sprintf("/PageSize [%s %s]", num(pt(doc.Width)), num(pt(doc.Length))))
	if set.Duplex {
		switch doc.Sides {
		case "two-sided-long-edge":
			feature(buf, "/Duplex true /Tumble false")
		case "two-sided-short-edge":
			feature(buf, "/Duplex true /Tumble true")
		default:
			feature(buf, "/Duplex false")
		}
	}
	if doc.NumCopies > 1 {
		feature(buf, fmt.Sprintf("/NumCopies %d", doc.NumCopies))
	}
	buf.WriteString("%%EndSetup\n")
	return w
}

// feature requests a device setting without failing the job when the printer lacks it (no
// duplexer, size not loaded): setpagedevice's configurationerror is caught by stopped.
func feature(buf *bytes.Buffer, entries string) {
	fmt.Fprintf(buf, "[{ << %s >> setpagedevice } stopped cleartomark\n", entries)
}

func (s settings) decodeFilter() string {
	if s.Level >= 3 {
		return "FlateDecode"
	}
	return "RunLengthDecode"
}

// page appends one sheet. The image fills the sheet from its top-left corner, so a rounding
// difference between the sheet in dots and PageSize in points ends up at the bottom edge.
func (w *docWriter) page(img pageImage) error {
	want := img.W * img.H
	if img.Depth == 1 {
		want = (img.W + 7) / 8 * img.H
	}
	if img.W <= 0 || img.H <= 0 || (img.Depth != 1 && img.Depth != 8) || len(img.Data) != want {
		return fmt.Errorf("postscript: bad page image %dx%d depth %d with %d bytes", img.W, img.H, img.Depth, len(img.Data))
	}
	w.n++
	sx, sy := float64(img.W)*72/float64(img.XDPI), float64(img.H)*72/float64(img.YDPI)
	fmt.Fprintf(w.buf, "%%%%Page: %d %d\n/pagesave save def\n", w.n, w.n)
	fmt.Fprintf(w.buf, "0 %s translate %s %s scale\n", num(pt(w.doc.Length)-sy), num(sx), num(sy))
	fmt.Fprintf(w.buf, "%d %d %d %s\n", img.W, img.H, img.Depth, procImg)
	if err := w.samples(img.Data); err != nil {
		return err
	}
	w.buf.WriteString("pagesave restore\nshowpage\n")
	return nil
}

func (w *docWriter) samples(data []byte) error {
	var enc []byte
	if w.set.Level >= 3 {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		if _, err := zw.Write(data); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		enc = z.Bytes()
	} else {
		enc = imaging.PackBits(make([]byte, 0, len(data)/8+16), data)
		enc = append(enc, 0x80)
	}
	if w.set.Binary {
		w.buf.Write(enc)
		w.buf.WriteByte('\n')
		return nil
	}
	writeASCII85(w.buf, enc)
	return nil
}

// writeASCII85 writes src as ASCII85 lines ending in the EOD marker "~>".
func writeASCII85(buf *bytes.Buffer, src []byte) {
	out := make([]byte, ascii85.MaxEncodedLen(len(src)))
	out = out[:ascii85.Encode(out, src)]
	for len(out) > 0 {
		n := min(a85Line, len(out))
		// '%' is an ASCII85 digit; a line starting with it could read as a DSC comment such as
		// %%EOF to a spooler. The decoder skips the space.
		if out[0] == '%' {
			buf.WriteByte(' ')
		}
		buf.Write(out[:n])
		out = out[n:]
		if len(out) > 0 {
			buf.WriteByte('\n')
		}
	}
	buf.WriteString("~>\n")
}

func (w *docWriter) finish() error {
	if w.n != w.pages {
		return fmt.Errorf("postscript: wrote %d pages, announced %d", w.n, w.pages)
	}
	w.buf.WriteString("%%Trailer\n%%EOF\n")
	if w.set.PJL {
		w.buf.WriteString(uel + "@PJL EOJ\r\n" + uel)
	}
	return nil
}

// dscText makes client text (the job name) safe on a comment line: printable ASCII only, so a
// newline cannot end the comment and inject code, and at most 64 characters.
func dscText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 64 {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return strings.TrimSpace(b.String())
}
