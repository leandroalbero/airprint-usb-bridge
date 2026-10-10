package cups

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// HeaderSize is sizeof(cups_page_header2_t).
const HeaderSize = 1796

// Colour spaces (cups_cspace_t) and orders (cups_order_t) the encoder writes.
const (
	CSpaceW        = 0
	CSpaceRGB      = 1
	CSpaceK        = 3
	CSpaceCMY      = 4
	CSpaceYMC      = 5
	CSpaceCMYK     = 6
	CSpaceYMCK     = 7
	CSpaceKCMY     = 8
	CSpaceSW       = 18
	CSpaceSRGB     = 19
	CSpaceAdobeRGB = 20

	OrderChunked = 0
	OrderBanded  = 1
	OrderPlanar  = 2
)

// Header is cups_page_header2_t, the page header of CUPS raster v1-v3. Field names follow
// cups/raster.h; the comment after each is its byte offset.
type Header struct {
	MediaClass         string    // 0
	MediaColor         string    // 64
	MediaType          string    // 128
	OutputType         string    // 192
	AdvanceDistance    uint32    // 256
	AdvanceMedia       uint32    // 260
	Collate            bool      // 264
	CutMedia           uint32    // 268
	Duplex             bool      // 272
	HWResolution       [2]uint32 // 276
	ImagingBoundingBox [4]uint32 // 284, points: left, bottom, right, top
	InsertSheet        bool      // 300
	Jog                uint32    // 304
	LeadingEdge        uint32    // 308
	Margins            [2]uint32 // 312, points: left, bottom
	ManualFeed         bool      // 320
	MediaPosition      uint32    // 324
	MediaWeight        uint32    // 328
	MirrorPrint        bool      // 332
	NegativePrint      bool      // 336
	NumCopies          uint32    // 340
	Orientation        uint32    // 344
	OutputFaceUp       bool      // 348
	PageSize           [2]uint32 // 352, points
	Separations        bool      // 360
	TraySwitch         bool      // 364
	Tumble             bool      // 368

	CupsWidth        uint32 // 372
	CupsHeight       uint32 // 376
	CupsMediaType    uint32 // 380
	CupsBitsPerColor uint32 // 384
	CupsBitsPerPixel uint32 // 388
	CupsBytesPerLine uint32 // 392
	CupsColorOrder   uint32 // 396
	CupsColorSpace   uint32 // 400
	CupsCompression  uint32 // 404
	CupsRowCount     uint32 // 408
	CupsRowFeed      uint32 // 412
	CupsRowStep      uint32 // 416

	CupsNumColors               uint32      // 420
	CupsBorderlessScalingFactor float32     // 424
	CupsPageSize                [2]float32  // 428
	CupsImagingBBox             [4]float32  // 436
	CupsInteger                 [16]uint32  // 452
	CupsReal                    [16]float32 // 516
	CupsString                  [16]string  // 580, 64 bytes each
	CupsMarkerType              string      // 1604
	CupsRenderingIntent         string      // 1668
	CupsPageSizeName            string      // 1732
}

// defaultHeader is where cupsRasterInterpretPPD starts before applying PPD code.
func defaultHeader() Header {
	return Header{
		HWResolution:                [2]uint32{100, 100},
		NumCopies:                   1,
		PageSize:                    [2]uint32{612, 792},
		ImagingBoundingBox:          [4]uint32{0, 0, 612, 792},
		CupsBitsPerColor:            1,
		CupsColorSpace:              CSpaceK,
		CupsColorOrder:              OrderChunked,
		CupsBorderlessScalingFactor: 1,
	}
}

// byteOrder is what libcups writes on x86 and ARM hosts ("3SaR" on disk); readers swap
// either order, but a filter that skips libcups most likely assumes this one.
var byteOrder binary.ByteOrder = binary.LittleEndian

// syncV3 is CUPS_RASTER_SYNC ("RaS3", uncompressed v3) in byteOrder.
func syncV3() []byte {
	b := make([]byte, 4)
	byteOrder.PutUint32(b, 0x52615333)
	return b
}

type headerWriter struct {
	b     []byte
	order binary.ByteOrder
}

func (w headerWriter) str(off int, s string) {
	// Fixed 64-byte C strings: keep room for the terminating NUL.
	copy(w.b[off:off+63], s)
}
func (w headerWriter) u32(off int, v uint32) { w.order.PutUint32(w.b[off:], v) }
func (w headerWriter) bool(off int, v bool) {
	if v {
		w.u32(off, 1)
	}
}
func (w headerWriter) f32(off int, v float32) { w.u32(off, math.Float32bits(v)) }

// Marshal encodes h in byte order o.
func (h *Header) Marshal(o binary.ByteOrder) []byte {
	w := headerWriter{b: make([]byte, HeaderSize), order: o}
	w.str(0, h.MediaClass)
	w.str(64, h.MediaColor)
	w.str(128, h.MediaType)
	w.str(192, h.OutputType)
	w.u32(256, h.AdvanceDistance)
	w.u32(260, h.AdvanceMedia)
	w.bool(264, h.Collate)
	w.u32(268, h.CutMedia)
	w.bool(272, h.Duplex)
	w.u32(276, h.HWResolution[0])
	w.u32(280, h.HWResolution[1])
	for i, v := range h.ImagingBoundingBox {
		w.u32(284+4*i, v)
	}
	w.bool(300, h.InsertSheet)
	w.u32(304, h.Jog)
	w.u32(308, h.LeadingEdge)
	w.u32(312, h.Margins[0])
	w.u32(316, h.Margins[1])
	w.bool(320, h.ManualFeed)
	w.u32(324, h.MediaPosition)
	w.u32(328, h.MediaWeight)
	w.bool(332, h.MirrorPrint)
	w.bool(336, h.NegativePrint)
	w.u32(340, h.NumCopies)
	w.u32(344, h.Orientation)
	w.bool(348, h.OutputFaceUp)
	w.u32(352, h.PageSize[0])
	w.u32(356, h.PageSize[1])
	w.bool(360, h.Separations)
	w.bool(364, h.TraySwitch)
	w.bool(368, h.Tumble)
	for i, v := range []uint32{h.CupsWidth, h.CupsHeight, h.CupsMediaType, h.CupsBitsPerColor,
		h.CupsBitsPerPixel, h.CupsBytesPerLine, h.CupsColorOrder, h.CupsColorSpace,
		h.CupsCompression, h.CupsRowCount, h.CupsRowFeed, h.CupsRowStep, h.CupsNumColors} {
		w.u32(372+4*i, v)
	}
	w.f32(424, h.CupsBorderlessScalingFactor)
	w.f32(428, h.CupsPageSize[0])
	w.f32(432, h.CupsPageSize[1])
	for i, v := range h.CupsImagingBBox {
		w.f32(436+4*i, v)
	}
	for i, v := range h.CupsInteger {
		w.u32(452+4*i, v)
	}
	for i, v := range h.CupsReal {
		w.f32(516+4*i, v)
	}
	for i, v := range h.CupsString {
		w.str(580+64*i, v)
	}
	w.str(1604, h.CupsMarkerType)
	w.str(1668, h.CupsRenderingIntent)
	w.str(1732, h.CupsPageSizeName)
	return w.b
}

// UnmarshalHeader decodes a header written in byte order o.
func UnmarshalHeader(b []byte, o binary.ByteOrder) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("cups raster: short header (%d bytes)", len(b))
	}
	str := func(off int) string {
		s := b[off : off+64]
		if i := bytes.IndexByte(s, 0); i >= 0 {
			s = s[:i]
		}
		return string(s)
	}
	u := func(off int) uint32 { return o.Uint32(b[off:]) }
	f := func(off int) float32 { return math.Float32frombits(u(off)) }
	h := Header{
		MediaClass: str(0), MediaColor: str(64), MediaType: str(128), OutputType: str(192),
		AdvanceDistance: u(256), AdvanceMedia: u(260), Collate: u(264) != 0, CutMedia: u(268),
		Duplex: u(272) != 0, HWResolution: [2]uint32{u(276), u(280)},
		ImagingBoundingBox: [4]uint32{u(284), u(288), u(292), u(296)},
		InsertSheet:        u(300) != 0, Jog: u(304), LeadingEdge: u(308),
		Margins:    [2]uint32{u(312), u(316)},
		ManualFeed: u(320) != 0, MediaPosition: u(324), MediaWeight: u(328),
		MirrorPrint: u(332) != 0, NegativePrint: u(336) != 0, NumCopies: u(340),
		Orientation: u(344), OutputFaceUp: u(348) != 0,
		PageSize:    [2]uint32{u(352), u(356)},
		Separations: u(360) != 0, TraySwitch: u(364) != 0, Tumble: u(368) != 0,
		CupsWidth: u(372), CupsHeight: u(376), CupsMediaType: u(380), CupsBitsPerColor: u(384),
		CupsBitsPerPixel: u(388), CupsBytesPerLine: u(392), CupsColorOrder: u(396),
		CupsColorSpace: u(400), CupsCompression: u(404), CupsRowCount: u(408),
		CupsRowFeed: u(412), CupsRowStep: u(416), CupsNumColors: u(420),
		CupsBorderlessScalingFactor: f(424),
		CupsPageSize:                [2]float32{f(428), f(432)},
		CupsImagingBBox:             [4]float32{f(436), f(440), f(444), f(448)},
		CupsMarkerType:              str(1604), CupsRenderingIntent: str(1668), CupsPageSizeName: str(1732),
	}
	for i := range h.CupsInteger {
		h.CupsInteger[i] = u(452 + 4*i)
		h.CupsReal[i] = f(516 + 4*i)
		h.CupsString[i] = str(580 + 64*i)
	}
	return h, nil
}

// format describes how a colour space and order lay out pixels.
type format struct {
	colors      int  // channels
	additive    bool // channel value 255 is white (W, sW, RGB) rather than full ink
	blackFirst  int  // index of the K channel in a CMYK-family space, -1 if none
	bitsPerPix  int  // chunked bits per pixel
	bytesPerRow int  // cupsBytesPerLine
	rows        int  // lines per page in the stream (height x colours for planar)
}

// layout validates the colour settings of h for a page w x hgt and returns its format.
func layout(h *Header, w, hgt int) (format, error) {
	bpc := int(h.CupsBitsPerColor)
	switch bpc {
	case 1, 2, 4, 8, 16:
	default:
		return format{}, fmt.Errorf("cupsBitsPerColor %d is not supported (want 1, 2, 4, 8 or 16)", bpc)
	}
	f := format{blackFirst: -1}
	switch h.CupsColorSpace {
	case CSpaceW, CSpaceSW:
		f.colors, f.additive = 1, true
	case CSpaceK:
		f.colors = 1
		f.blackFirst = 0
	case CSpaceRGB, CSpaceSRGB, CSpaceAdobeRGB:
		f.colors, f.additive = 3, true
	case CSpaceCMY, CSpaceYMC:
		f.colors = 3
	case CSpaceCMYK, CSpaceYMCK:
		f.colors, f.blackFirst = 4, 3
	case CSpaceKCMY:
		f.colors, f.blackFirst = 4, 0
	default:
		return format{}, fmt.Errorf("cupsColorSpace %d is not supported (want W, sW, K, RGB, sRGB, AdobeRGB, CMY, YMC, CMYK, YMCK or KCMY)", h.CupsColorSpace)
	}
	switch h.CupsColorOrder {
	case OrderChunked:
		if f.colors == 3 && bpc < 8 {
			return format{}, fmt.Errorf("chunked %d-bit colour with 3 channels is not supported", bpc)
		}
		f.bitsPerPix = bpc * f.colors
		f.bytesPerRow = (w*f.bitsPerPix + 7) / 8
		f.rows = hgt
	case OrderBanded:
		f.bitsPerPix = bpc
		f.bytesPerRow = (w*bpc + 7) / 8 * f.colors
		f.rows = hgt
	case OrderPlanar:
		f.bitsPerPix = bpc
		f.bytesPerRow = (w*bpc + 7) / 8
		f.rows = hgt * f.colors
	default:
		return format{}, fmt.Errorf("cupsColorOrder %d is not supported", h.CupsColorOrder)
	}
	return f, nil
}

// gray reports whether the colour space has one channel.
func (h *Header) gray() bool {
	return h.CupsColorSpace == CSpaceW || h.CupsColorSpace == CSpaceSW || h.CupsColorSpace == CSpaceK
}

var errNotDict = errors.New("not a dictionary")

// psValue is a parsed PostScript literal: a number, bool, string/name, array of numbers, or null.
type psValue struct {
	kind byte // 'n' number, 'b' bool, 's' string or name, 'a' number array, '0' null
	num  float64
	b    bool
	s    string
	arr  []float64
}

// applyCode runs the "<< ... >> setpagedevice" dictionaries of a PPD choice's code against h.
// Anything else in the code (procedures, JCL, other operators) is ignored, as are keys the
// raster header does not have.
func applyCode(h *Header, code string) error {
	toks, err := psTokens(code)
	if err != nil {
		return err
	}
	for i := 0; i < len(toks); i++ {
		if toks[i] != "<<" {
			continue
		}
		dict, end, err := psDict(toks, i)
		if err != nil {
			return err
		}
		i = end
		if end+1 < len(toks) && toks[end+1] == "setpagedevice" {
			for _, kv := range dict {
				setField(h, kv.key, kv.val)
			}
			i++
		}
	}
	return nil
}

type psKV struct {
	key string
	val psValue
}

// psTokens splits PostScript into "<<", ">>", "[", "]", "{", "}", "/name", "(string)" (kept with
// its parentheses, escapes resolved) and other words.
func psTokens(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == 0:
			i++
		case c == '%':
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		case strings.HasPrefix(s[i:], "<<"), strings.HasPrefix(s[i:], ">>"):
			out = append(out, s[i:i+2])
			i += 2
		case c == '<':
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				return nil, errors.New("unterminated hex string")
			}
			out = append(out, "("+hexString(s[i+1:i+j])+")")
			i += j + 1
		case c == '[' || c == ']' || c == '{' || c == '}':
			out = append(out, string(c))
			i++
		case c == '(':
			str, n, err := psString(s[i:])
			if err != nil {
				return nil, err
			}
			out = append(out, "("+str+")")
			i += n
		default:
			j := i + 1
			for j < len(s) && !strings.ContainsRune(" \t\n\r\f\x00()<>[]{}/%", rune(s[j])) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out, nil
}

func hexString(h string) string {
	var b []byte
	hi := -1
	for _, c := range h {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			continue
		}
		if hi < 0 {
			hi = v
		} else {
			b = append(b, byte(hi<<4|v))
			hi = -1
		}
	}
	if hi >= 0 {
		b = append(b, byte(hi<<4))
	}
	return string(b)
}

// psString reads a "(...)" string with nested parentheses and backslash escapes; n is the
// number of input bytes consumed.
func psString(s string) (str string, n int, err error) {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '(':
			if depth > 0 {
				b.WriteByte(c)
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				return b.String(), i + 1, nil
			}
			b.WriteByte(c)
		case '\\':
			i++
			if i >= len(s) {
				return "", 0, errors.New("unterminated string")
			}
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '\n':
			default:
				if e >= '0' && e <= '7' {
					v := 0
					k := 0
					for ; k < 3 && i+k < len(s) && s[i+k] >= '0' && s[i+k] <= '7'; k++ {
						v = v*8 + int(s[i+k]-'0')
					}
					b.WriteByte(byte(v))
					i += k - 1
				} else {
					b.WriteByte(e)
				}
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, errors.New("unterminated string")
}

// psDict parses the dictionary opened at toks[start] ("<<") and returns the index of its ">>".
// Nested dictionaries and procedures are skipped.
func psDict(toks []string, start int) ([]psKV, int, error) {
	if toks[start] != "<<" {
		return nil, 0, errNotDict
	}
	var out []psKV
	i := start + 1
	for i < len(toks) {
		t := toks[i]
		if t == ">>" {
			return out, i, nil
		}
		if !strings.HasPrefix(t, "/") {
			i++
			continue
		}
		key := t[1:]
		i++
		if i >= len(toks) {
			break
		}
		v, next, ok := psParseValue(toks, i)
		if ok {
			out = append(out, psKV{key, v})
		}
		i = next
	}
	return nil, 0, errors.New("unterminated dictionary")
}

func psParseValue(toks []string, i int) (psValue, int, bool) {
	t := toks[i]
	switch {
	case t == "<<":
		depth := 0
		for j := i; j < len(toks); j++ {
			switch toks[j] {
			case "<<":
				depth++
			case ">>":
				depth--
				if depth == 0 {
					return psValue{}, j + 1, false
				}
			}
		}
		return psValue{}, len(toks), false
	case t == "{":
		depth := 0
		for j := i; j < len(toks); j++ {
			switch toks[j] {
			case "{":
				depth++
			case "}":
				depth--
				if depth == 0 {
					return psValue{}, j + 1, false
				}
			}
		}
		return psValue{}, len(toks), false
	case t == "[":
		var arr []float64
		j := i + 1
		ok := true
		for ; j < len(toks) && toks[j] != "]"; j++ {
			n, isNum := psNumber(toks[j])
			if !isNum {
				ok = false
				continue
			}
			arr = append(arr, n)
		}
		return psValue{kind: 'a', arr: arr}, j + 1, ok
	case t == "true" || t == "false":
		return psValue{kind: 'b', b: t == "true"}, i + 1, true
	case t == "null":
		return psValue{kind: '0'}, i + 1, true
	case strings.HasPrefix(t, "("):
		return psValue{kind: 's', s: t[1 : len(t)-1]}, i + 1, true
	case strings.HasPrefix(t, "/"):
		return psValue{kind: 's', s: t[1:]}, i + 1, true
	}
	if n, ok := psNumber(t); ok {
		return psValue{kind: 'n', num: n}, i + 1, true
	}
	return psValue{}, i + 1, false
}

func psNumber(t string) (float64, bool) {
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// psUint is how setpagedevice integers land in the unsigned header fields: negative values
// (darkness -1, label top -120) wrap like the C casts in CUPS.
func psUint(v psValue) (uint32, bool) {
	switch v.kind {
	case 'n':
		return uint32(int32(int64(v.num))), true
	case 'b':
		if v.b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func setField(h *Header, key string, v psValue) {
	u, isInt := psUint(v)
	setU := func(p *uint32) {
		if isInt {
			*p = u
		}
	}
	setB := func(p *bool) {
		switch v.kind {
		case 'b':
			*p = v.b
		case 'n':
			*p = v.num != 0
		}
	}
	setS := func(p *string) {
		if v.kind == 's' {
			*p = v.s
		}
	}
	setPair := func(p *[2]uint32) {
		if v.kind == 'a' && len(v.arr) == 2 {
			p[0], p[1] = uint32(math.Max(0, v.arr[0]+0.5)), uint32(math.Max(0, v.arr[1]+0.5))
		}
	}
	switch key {
	case "MediaClass":
		setS(&h.MediaClass)
	case "MediaColor":
		setS(&h.MediaColor)
	case "MediaType":
		setS(&h.MediaType)
	case "OutputType":
		setS(&h.OutputType)
	case "AdvanceDistance":
		setU(&h.AdvanceDistance)
	case "AdvanceMedia":
		setU(&h.AdvanceMedia)
	case "Collate":
		setB(&h.Collate)
	case "CutMedia":
		setU(&h.CutMedia)
	case "Duplex":
		setB(&h.Duplex)
	case "HWResolution":
		setPair(&h.HWResolution)
	case "InsertSheet":
		setB(&h.InsertSheet)
	case "Jog":
		setU(&h.Jog)
	case "LeadingEdge":
		setU(&h.LeadingEdge)
	case "ManualFeed":
		setB(&h.ManualFeed)
	case "MediaPosition":
		setU(&h.MediaPosition)
	case "MediaWeight":
		setU(&h.MediaWeight)
	case "MirrorPrint":
		setB(&h.MirrorPrint)
	case "NegativePrint":
		setB(&h.NegativePrint)
	case "Orientation":
		setU(&h.Orientation)
	case "OutputFaceUp":
		setB(&h.OutputFaceUp)
	case "PageSize":
		setPair(&h.PageSize)
	case "Separations":
		setB(&h.Separations)
	case "TraySwitch":
		setB(&h.TraySwitch)
	case "Tumble":
		setB(&h.Tumble)
	case "cupsMediaType":
		setU(&h.CupsMediaType)
	case "cupsBitsPerColor":
		setU(&h.CupsBitsPerColor)
	case "cupsColorOrder":
		setU(&h.CupsColorOrder)
	case "cupsColorSpace":
		setU(&h.CupsColorSpace)
	case "cupsCompression":
		setU(&h.CupsCompression)
	case "cupsRowCount":
		setU(&h.CupsRowCount)
	case "cupsRowFeed":
		setU(&h.CupsRowFeed)
	case "cupsRowStep":
		setU(&h.CupsRowStep)
	case "cupsBorderlessScalingFactor":
		if v.kind == 'n' {
			h.CupsBorderlessScalingFactor = float32(v.num)
		}
	case "cupsMarkerType":
		setS(&h.CupsMarkerType)
	case "cupsRenderingIntent":
		setS(&h.CupsRenderingIntent)
	case "cupsPageSizeName":
		setS(&h.CupsPageSizeName)
	default:
		var idx int
		switch {
		case scanIndex(key, "cupsInteger", &idx):
			setU(&h.CupsInteger[idx])
		case scanIndex(key, "cupsReal", &idx):
			if v.kind == 'n' {
				h.CupsReal[idx] = float32(v.num)
			}
		case scanIndex(key, "cupsString", &idx):
			setS(&h.CupsString[idx])
		}
	}
}

func scanIndex(key, prefix string, idx *int) bool {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || rest == "" || len(rest) > 2 {
		return false
	}
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	if n > 15 {
		return false
	}
	*idx = n
	return true
}
