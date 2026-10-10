package cups

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// Offsets of cups_page_header2_t printed by offsetof() against the macOS SDK's cups/raster.h;
// they also match the PWG 5102.4 raster header, which shares the layout.
var headerOffsets = map[string]int{
	"MediaClass": 0, "MediaColor": 64, "MediaType": 128, "OutputType": 192,
	"AdvanceDistance": 256, "AdvanceMedia": 260, "Collate": 264, "CutMedia": 268, "Duplex": 272,
	"HWResolution": 276, "ImagingBoundingBox": 284, "InsertSheet": 300, "Jog": 304,
	"LeadingEdge": 308, "Margins": 312, "ManualFeed": 320, "MediaPosition": 324,
	"MediaWeight": 328, "MirrorPrint": 332, "NegativePrint": 336, "NumCopies": 340,
	"Orientation": 344, "OutputFaceUp": 348, "PageSize": 352, "Separations": 360,
	"TraySwitch": 364, "Tumble": 368, "cupsWidth": 372, "cupsHeight": 376, "cupsMediaType": 380,
	"cupsBitsPerColor": 384, "cupsBitsPerPixel": 388, "cupsBytesPerLine": 392,
	"cupsColorOrder": 396, "cupsColorSpace": 400, "cupsCompression": 404, "cupsRowCount": 408,
	"cupsRowFeed": 412, "cupsRowStep": 416, "cupsNumColors": 420,
	"cupsBorderlessScalingFactor": 424, "cupsPageSize": 428, "cupsImagingBBox": 436,
	"cupsInteger": 452, "cupsReal": 516, "cupsString": 580, "cupsMarkerType": 1604,
	"cupsRenderingIntent": 1668, "cupsPageSizeName": 1732,
}

// TestHeaderOffsets fills every field with a distinct value and reads it back at the offsets
// measured from the C header, independently of Marshal's own offsets.
func TestHeaderOffsets(t *testing.T) {
	h := Header{
		MediaClass: "Class", MediaColor: "White", MediaType: "Plain", OutputType: "Out",
		AdvanceDistance: 1, AdvanceMedia: 2, Collate: true, CutMedia: 3, Duplex: true,
		HWResolution: [2]uint32{300, 600}, ImagingBoundingBox: [4]uint32{12, 13, 583, 830},
		InsertSheet: true, Jog: 4, LeadingEdge: 5, Margins: [2]uint32{12, 13}, ManualFeed: true,
		MediaPosition: 6, MediaWeight: 7, MirrorPrint: true, NegativePrint: true, NumCopies: 8,
		Orientation: 9, OutputFaceUp: true, PageSize: [2]uint32{595, 842}, Separations: true,
		TraySwitch: true, Tumble: true, CupsWidth: 2379, CupsHeight: 3404, CupsMediaType: 10,
		CupsBitsPerColor: 1, CupsBitsPerPixel: 1, CupsBytesPerLine: 298, CupsColorOrder: 2,
		CupsColorSpace: 3, CupsCompression: 0xffffffff, CupsRowCount: 11, CupsRowFeed: 12,
		CupsRowStep: 13, CupsNumColors: 1, CupsBorderlessScalingFactor: 1.5,
		CupsPageSize: [2]float32{595.5, 842.25}, CupsImagingBBox: [4]float32{1, 2, 3, 4.5},
		CupsMarkerType: "Toner", CupsRenderingIntent: "Perceptual", CupsPageSizeName: "A4",
	}
	h.CupsInteger[0], h.CupsInteger[15] = 100, 115
	h.CupsReal[0], h.CupsReal[15] = 0.25, 15.5
	h.CupsString[0], h.CupsString[15] = "first", "last"

	for _, o := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		b := h.Marshal(o)
		if len(b) != HeaderSize || HeaderSize != 1796 {
			t.Fatalf("header is %d bytes", len(b))
		}
		u := func(name string, i int) uint32 { return o.Uint32(b[headerOffsets[name]+4*i:]) }
		f := func(name string, i int) float32 { return math.Float32frombits(u(name, i)) }
		s := func(name string, i int) string {
			off := headerOffsets[name] + 64*i
			return string(bytes.TrimRight(b[off:off+64], "\x00"))
		}
		checks := []struct {
			name      string
			got, want any
		}{
			{"MediaClass", s("MediaClass", 0), "Class"},
			{"MediaColor", s("MediaColor", 0), "White"},
			{"MediaType", s("MediaType", 0), "Plain"},
			{"OutputType", s("OutputType", 0), "Out"},
			{"AdvanceDistance", u("AdvanceDistance", 0), uint32(1)},
			{"AdvanceMedia", u("AdvanceMedia", 0), uint32(2)},
			{"Collate", u("Collate", 0), uint32(1)},
			{"CutMedia", u("CutMedia", 0), uint32(3)},
			{"Duplex", u("Duplex", 0), uint32(1)},
			{"HWResolution[0]", u("HWResolution", 0), uint32(300)},
			{"HWResolution[1]", u("HWResolution", 1), uint32(600)},
			{"ImagingBoundingBox[0]", u("ImagingBoundingBox", 0), uint32(12)},
			{"ImagingBoundingBox[3]", u("ImagingBoundingBox", 3), uint32(830)},
			{"InsertSheet", u("InsertSheet", 0), uint32(1)},
			{"Jog", u("Jog", 0), uint32(4)},
			{"LeadingEdge", u("LeadingEdge", 0), uint32(5)},
			{"Margins[0]", u("Margins", 0), uint32(12)},
			{"Margins[1]", u("Margins", 1), uint32(13)},
			{"ManualFeed", u("ManualFeed", 0), uint32(1)},
			{"MediaPosition", u("MediaPosition", 0), uint32(6)},
			{"MediaWeight", u("MediaWeight", 0), uint32(7)},
			{"MirrorPrint", u("MirrorPrint", 0), uint32(1)},
			{"NegativePrint", u("NegativePrint", 0), uint32(1)},
			{"NumCopies", u("NumCopies", 0), uint32(8)},
			{"Orientation", u("Orientation", 0), uint32(9)},
			{"OutputFaceUp", u("OutputFaceUp", 0), uint32(1)},
			{"PageSize[0]", u("PageSize", 0), uint32(595)},
			{"PageSize[1]", u("PageSize", 1), uint32(842)},
			{"Separations", u("Separations", 0), uint32(1)},
			{"TraySwitch", u("TraySwitch", 0), uint32(1)},
			{"Tumble", u("Tumble", 0), uint32(1)},
			{"cupsWidth", u("cupsWidth", 0), uint32(2379)},
			{"cupsHeight", u("cupsHeight", 0), uint32(3404)},
			{"cupsMediaType", u("cupsMediaType", 0), uint32(10)},
			{"cupsBitsPerColor", u("cupsBitsPerColor", 0), uint32(1)},
			{"cupsBitsPerPixel", u("cupsBitsPerPixel", 0), uint32(1)},
			{"cupsBytesPerLine", u("cupsBytesPerLine", 0), uint32(298)},
			{"cupsColorOrder", u("cupsColorOrder", 0), uint32(2)},
			{"cupsColorSpace", u("cupsColorSpace", 0), uint32(3)},
			{"cupsCompression", u("cupsCompression", 0), uint32(0xffffffff)},
			{"cupsRowCount", u("cupsRowCount", 0), uint32(11)},
			{"cupsRowFeed", u("cupsRowFeed", 0), uint32(12)},
			{"cupsRowStep", u("cupsRowStep", 0), uint32(13)},
			{"cupsNumColors", u("cupsNumColors", 0), uint32(1)},
			{"cupsBorderlessScalingFactor", f("cupsBorderlessScalingFactor", 0), float32(1.5)},
			{"cupsPageSize[0]", f("cupsPageSize", 0), float32(595.5)},
			{"cupsPageSize[1]", f("cupsPageSize", 1), float32(842.25)},
			{"cupsImagingBBox[3]", f("cupsImagingBBox", 3), float32(4.5)},
			{"cupsInteger[0]", u("cupsInteger", 0), uint32(100)},
			{"cupsInteger[15]", u("cupsInteger", 15), uint32(115)},
			{"cupsReal[0]", f("cupsReal", 0), float32(0.25)},
			{"cupsReal[15]", f("cupsReal", 15), float32(15.5)},
			{"cupsString[0]", s("cupsString", 0), "first"},
			{"cupsString[15]", s("cupsString", 15), "last"},
			{"cupsMarkerType", s("cupsMarkerType", 0), "Toner"},
			{"cupsRenderingIntent", s("cupsRenderingIntent", 0), "Perceptual"},
			{"cupsPageSizeName", s("cupsPageSizeName", 0), "A4"},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%v %s = %v, want %v", o, c.name, c.got, c.want)
			}
		}
		back, err := UnmarshalHeader(b, o)
		if err != nil || back != h {
			t.Errorf("%v round trip: %v\n got %+v\nwant %+v", o, err, back, h)
		}
	}
	if got := syncV3(); string(got) != "3SaR" {
		t.Errorf("sync %q, want little-endian RaS3", got)
	}
	long := Header{MediaClass: string(bytes.Repeat([]byte("x"), 80))}
	if b := long.Marshal(binary.LittleEndian); b[63] != 0 || b[64] != 0 {
		t.Error("64-byte string not NUL-terminated")
	}
}

func TestApplyCode(t *testing.T) {
	h := defaultHeader()
	code := `%!
<</HWResolution[600 300]/cupsBitsPerColor 8/cupsColorSpace 18/cupsColorOrder 0
  /cupsCompression -1/cupsRowStep -120/MediaType(Glossy\051)/OutputType<4869>
  /Duplex true/Tumble false/cupsInteger3 7/cupsReal2 1.5/cupsString1 (s)
  /Policies << /PageSize 2 >> /Install { pop } /PageSize[5 -2 roll]
  /cupsRenderingIntent /Perceptual /Unknown 5>>setpagedevice
<</ManualFeed true>> pop`
	if err := applyCode(&h, code); err != nil {
		t.Fatal(err)
	}
	want := defaultHeader()
	want.HWResolution = [2]uint32{600, 300}
	want.CupsBitsPerColor, want.CupsColorSpace = 8, CSpaceSW
	want.CupsCompression, want.CupsRowStep = 0xffffffff, uint32(0xffffff88)
	want.MediaType, want.OutputType = "Glossy)", "Hi"
	want.Duplex = true
	want.CupsInteger[3], want.CupsReal[2], want.CupsString[1] = 7, 1.5, "s"
	want.CupsRenderingIntent = "Perceptual"
	if h != want {
		t.Errorf("got  %+v\nwant %+v", h, want)
	}
	for _, bad := range []string{`<</MediaType(open>>setpagedevice`, `<</A 1 setpagedevice`, `<</A <41`} {
		if err := applyCode(&h, bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func page(w, h int, draw func(x, y int) byte) raster.Page {
	p := raster.Page{Width: w, Height: h, XDPI: 300, YDPI: 300, Gray: make([]byte, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p.Gray[y*w+x] = draw(x, y)
		}
	}
	return p
}

// TestEncodePage checks each layout byte by byte on a 10x2 page: row 0 black, white, then
// mid-grey 0x80; row 1 white.
func TestEncodePage(t *testing.T) {
	p := page(10, 2, func(x, y int) byte {
		switch {
		case y == 1:
			return 255
		case x < 4:
			return 0
		case x < 8:
			return 255
		}
		return 0x80
	})
	bm := imaging.Threshold(p, 128)
	cases := []struct {
		name               string
		cs, bpc, order     uint32
		bitsPerPix, perRow int
		want               []byte
	}{
		{"K1", CSpaceK, 1, OrderChunked, 1, 2, []byte{0xf0, 0x00, 0, 0}},
		{"W1", CSpaceW, 1, OrderChunked, 1, 2, []byte{0x0f, 0xff, 0xff, 0xff}},
		{"W8", CSpaceW, 8, OrderChunked, 8, 10, append([]byte{0, 0, 0, 0, 255, 255, 255, 255, 0x80, 0x80}, bytes.Repeat([]byte{255}, 10)...)},
		{"K8", CSpaceK, 8, OrderChunked, 8, 10, append([]byte{255, 255, 255, 255, 0, 0, 0, 0, 0x7f, 0x7f}, make([]byte, 10)...)},
		{"K2", CSpaceK, 2, OrderChunked, 2, 3, []byte{0xff, 0x00, 0x50, 0, 0, 0}},
		{"SW4", CSpaceSW, 4, OrderChunked, 4, 5, []byte{0x00, 0x00, 0xff, 0xff, 0x88, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{"K16", CSpaceK, 16, OrderChunked, 16, 20, append(append(bytes.Repeat([]byte{0xff, 0xff}, 4), append(make([]byte, 8), 0x7f, 0x7f, 0x7f, 0x7f)...), make([]byte, 20)...)},
		{"KCMY1 chunked", CSpaceKCMY, 1, OrderChunked, 4, 5, []byte{0x88, 0x88, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"CMYK1 banded", CSpaceCMYK, 1, OrderBanded, 1, 8, []byte{0, 0, 0, 0, 0, 0, 0xf0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"RGB8 chunked", CSpaceRGB, 8, OrderChunked, 24, 30, nil},
		{"CMY1 planar", CSpaceCMY, 1, OrderPlanar, 1, 2, []byte{0xf0, 0, 0, 0, 0xf0, 0, 0, 0, 0xf0, 0, 0, 0}},
	}
	for _, c := range cases {
		h := Header{CupsColorSpace: c.cs, CupsBitsPerColor: c.bpc, CupsColorOrder: c.order}
		f, err := layout(&h, p.Width, p.Height)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if f.bitsPerPix != c.bitsPerPix || f.bytesPerRow != c.perRow {
			t.Errorf("%s: %d bits/pixel %d bytes/line, want %d %d", c.name, f.bitsPerPix, f.bytesPerRow, c.bitsPerPix, c.perRow)
		}
		got := encodePage(nil, &h, f, p, bm)
		if len(got) != f.bytesPerRow*f.rows {
			t.Errorf("%s: %d bytes, want %d", c.name, len(got), f.bytesPerRow*f.rows)
		}
		if c.want != nil && !bytes.Equal(got, c.want) {
			t.Errorf("%s:\n got % x\nwant % x", c.name, got, c.want)
		}
		if c.name == "RGB8 chunked" && (got[0] != 0 || got[12] != 255 || got[24] != 0x80 || got[29] != 0x80) {
			t.Errorf("%s: % x", c.name, got[:30])
		}
	}
	for _, bad := range []Header{
		{CupsColorSpace: CSpaceK, CupsBitsPerColor: 3},
		{CupsColorSpace: 16, CupsBitsPerColor: 8},
		{CupsColorSpace: CSpaceRGB, CupsBitsPerColor: 1, CupsColorOrder: OrderChunked},
		{CupsColorSpace: CSpaceK, CupsBitsPerColor: 1, CupsColorOrder: 7},
	} {
		if _, err := layout(&bad, 10, 2); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestCropFlip(t *testing.T) {
	p := page(4, 3, func(x, y int) byte { return byte(10*y + x) })
	c := crop(p, 1, -1, 4, 3)
	want := []byte{255, 255, 255, 255, 1, 2, 3, 255, 11, 12, 13, 255}
	if !bytes.Equal(c.Gray, want) {
		t.Errorf("crop % d", c.Gray)
	}
	flip(p, true, false)
	if p.Gray[0] != 3 || p.Gray[4] != 13 {
		t.Errorf("flip x % d", p.Gray)
	}
	flip(p, true, true)
	if p.Gray[0] != 20 || p.Gray[11] != 3 {
		t.Errorf("flip both % d", p.Gray)
	}
	for _, c := range []struct {
		side         string
		tumble, x, y bool
	}{
		{"Normal", false, false, false}, {"Rotated", false, true, true}, {"Rotated", true, false, false},
		{"Flipped", false, false, true}, {"Flipped", true, true, false},
		{"ManualTumble", false, false, false}, {"ManualTumble", true, true, true},
	} {
		if x, y := backSideFlip(c.side, c.tumble); x != c.x || y != c.y {
			t.Errorf("%s tumble=%v: %v %v", c.side, c.tumble, x, y)
		}
	}
}
