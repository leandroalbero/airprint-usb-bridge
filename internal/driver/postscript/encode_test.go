package postscript

import (
	"bytes"
	"encoding/ascii85"
	"math/rand"
	"strings"
	"testing"
)

// The expected text is written from the PostScript Language Reference (LanguageLevel 2 image
// dictionary, RunLengthDecode, ASCII85Decode) and the DSC 3.0 conventions, not captured from the
// encoder. The samples were packed and encoded by hand:
//
//	rows FF00 FF00 FF00 0FF0 → RunLength literal run 07 FF00FF00FF000FF0, EOD 80
//	ASCII85 07FF00FF → #QFf(   00FF000F → !<3$0   F080 (partial) → n:1
func TestEncodeHandChecked(t *testing.T) {
	var buf bytes.Buffer
	w := newDocWriter(&buf, settings{Level: 2, Depth: 1}, docInfo{Title: "t", Width: 21000, Length: 29700, Sides: "one-sided"}, 1)
	img := pageImage{W: 16, H: 4, XDPI: 300, YDPI: 300, Depth: 1, Data: []byte{0xff, 0, 0xff, 0, 0xff, 0, 0x0f, 0xf0}}
	if err := w.page(img); err != nil {
		t.Fatal(err)
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	want := `%!PS-Adobe-3.0
%%Creator: airprint-usb-bridge
%%Title: t
%%LanguageLevel: 2
%%DocumentData: Clean7Bit
%%Pages: 1
%%EndComments
%%BeginProlog
/aub 8 dict def
/aubimg { % width height bits aubimg -, samples follow
  aub begin /b exch def /h exch def /w exch def
  /s currentfile /ASCII85Decode filter def
  /d s /RunLengthDecode filter def
  /DeviceGray setcolorspace
  << /ImageType 1 /Width w /Height h /BitsPerComponent b
     /Decode b 1 eq { [1 0] } { [0 1] } ifelse
     /ImageMatrix [w 0 0 h neg 0 h] /DataSource d >> image
  d flushfile s flushfile
  end
} bind def
%%EndProlog
%%BeginSetup
[{ << /PageSize [595.28 841.89] >> setpagedevice } stopped cleartomark
%%EndSetup
%%Page: 1 1
/pagesave save def
0 840.93 translate 3.84 0.96 scale
16 4 1 aubimg
#QFf(!<3$0n:1~>
pagesave restore
showpage
%%Trailer
%%EOF
`
	if got := buf.String(); got != want {
		t.Fatalf("document differs:\n%s\nwant:\n%s", got, want)
	}
	doc, err := decodeDoc(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Images) != 1 || !bytes.Equal(doc.Images[0].Data, img.Data) || doc.Images[0].Scale != "3.84 0.96" {
		t.Fatalf("decoded %+v", doc.Images)
	}
}

func TestEncodeBinaryFlateAndPJL(t *testing.T) {
	var buf bytes.Buffer
	set := settings{Level: 3, Depth: 8, Binary: true, Duplex: true, PJL: true}
	w := newDocWriter(&buf, set, docInfo{Width: 21590, Length: 27940, Sides: "two-sided-short-edge", NumCopies: 4}, 2)
	gray := make([]byte, 30*7)
	for i := range gray {
		gray[i] = byte(i * 37)
	}
	for p := 0; p < 2; p++ {
		if err := w.page(pageImage{W: 30, H: 7, XDPI: 600, YDPI: 600, Depth: 8, Data: gray}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, s := range []string{
		"\x1b%-12345X@PJL JOB\r\n@PJL ENTER LANGUAGE = POSTSCRIPT\r\n%!PS-Adobe-3.0\n",
		"%%LanguageLevel: 3\n%%DocumentData: Binary\n%%Pages: 2\n",
		"  /d currentfile /FlateDecode filter def\n",
		"  d flushfile\n",
		"[{ << /PageSize [612 792] >> setpagedevice } stopped cleartomark\n",
		"[{ << /Duplex true /Tumble true >> setpagedevice } stopped cleartomark\n",
		"[{ << /NumCopies 4 >> setpagedevice } stopped cleartomark\n",
		"0 791.16 translate 3.6 0.84 scale\n30 7 8 aubimg\n",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q", s)
		}
	}
	if strings.Contains(out, "ASCII85") || !strings.HasSuffix(out, "%%EOF\n\x1b%-12345X@PJL EOJ\r\n\x1b%-12345X") {
		t.Errorf("unexpected encoding or ending:\n%q", out[len(out)-60:])
	}
	doc, err := decodeDoc(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !doc.PJL || doc.Level != 3 || !doc.Binary || len(doc.Images) != 2 {
		t.Fatalf("decoded %+v", doc)
	}
	for _, im := range doc.Images {
		if !bytes.Equal(im.Data, gray) {
			t.Fatal("gray samples differ after FlateDecode")
		}
	}
}

func TestPageRejectsBadImage(t *testing.T) {
	var buf bytes.Buffer
	w := newDocWriter(&buf, settings{Level: 2, Depth: 1}, docInfo{Width: 21000, Length: 29700}, 1)
	if err := w.page(pageImage{W: 9, H: 2, XDPI: 300, YDPI: 300, Depth: 1, Data: make([]byte, 2)}); err == nil {
		t.Fatal("short 1-bit data accepted")
	}
	if err := w.finish(); err == nil {
		t.Fatal("finish accepted fewer pages than announced")
	}
}

func TestASCII85LinesNeverStartWithPercent(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	hits := 0
	for try := 0; try < 2000 && hits < 5; try++ {
		src := make([]byte, 400)
		rng.Read(src)
		enc := make([]byte, ascii85.MaxEncodedLen(len(src)))
		enc = enc[:ascii85.Encode(enc, src)]
		starts := false
		for i := a85Line; i < len(enc); i += a85Line {
			starts = starts || enc[i] == '%'
		}
		if !starts && enc[0] != '%' {
			continue
		}
		hits++
		var buf bytes.Buffer
		writeASCII85(&buf, src)
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.HasPrefix(line, "%") || len(line) > a85Line+1 {
				t.Fatalf("line %q", line)
			}
		}
		text := strings.TrimSuffix(buf.String(), "~>\n")
		dec := make([]byte, len(text))
		n, _, err := ascii85.Decode(dec, []byte(text), true)
		if err != nil || !bytes.Equal(dec[:n], src) {
			t.Fatalf("round trip: %v", err)
		}
	}
	if hits == 0 {
		t.Fatal("no sample put '%' at a line start")
	}
}

func TestDSCTextCannotEscapeComment(t *testing.T) {
	got := dscText("report\n%%EOF\r\nshowpage é" + strings.Repeat("x", 100))
	if strings.ContainsAny(got, "\r\n") || len(got) > 64 || !strings.HasPrefix(got, "report?%%EOF??showpage ?") {
		t.Fatalf("%q", got)
	}
}

func TestDSCPages(t *testing.T) {
	for _, c := range []struct {
		doc  string
		want int
	}{
		{"%!PS\nshowpage\n", 1},
		{"%!PS-Adobe-3.0\n%%Pages: 3\n%%EndComments\n", 3},
		{"%!PS-Adobe-3.0\n%%Pages: (atend)\n%%BeginDocument: x\n%%Pages: 9\n%%EndDocument\n%%Trailer\n%%Pages: 2\n", 2},
		{"%!PS-Adobe-3.0\n%%Pages: 4\n%%BeginDocument: x\n%%Pages: 9\n%%EndDocument\n", 4},
		{"%!PS-Adobe-3.0\r\n%%Pages: 5\r\n", 5},
		{"%!PS\n% not %%Pages: 7\n", 1},
	} {
		if got := dscPages([]byte(c.doc)); got != c.want {
			t.Errorf("%q: %d pages, want %d", c.doc, got, c.want)
		}
	}
}
