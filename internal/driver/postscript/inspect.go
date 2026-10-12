package postscript

import (
	"bytes"
	"compress/zlib"
	"encoding/ascii85"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
)

var errInspect = errors.New("postscript: not a document this driver wrote")

// decoded is a document this driver wrote, read back.
type decoded struct {
	PJL      bool
	Level    int
	Binary   bool
	Pages    int      // %%Pages
	Setup    []string // setpagedevice requests, e.g. "/PageSize [595.28 841.89]"
	Images   []decodedImage
	Trailing bool // %%EOF present
}

type decodedImage struct {
	W, H, Depth int
	RGB         bool   // 8-bit DeviceRGB samples
	Scale       string // "<sx> <sy>" in points
	Encoded     int    // bytes of filter data in the file
	Ink         int    // black dots of a 1-bit image
	Data        []byte // samples, as pageImage.Data; nil unless kept
}

// Inspect describes a unit for dry runs: the settings and each page image of a rendered job (its
// samples decoded and checked), or the size and DSC page count of a pass-through document.
func (d *Driver) Inspect(data []byte) (string, error) {
	doc, err := decodeImages(data, false)
	if errors.Is(err, errInspect) {
		body := data
		if bytes.HasPrefix(body, []byte(uel)) {
			if i := bytes.Index(body, []byte("%!")); i >= 0 {
				body = body[i:]
			}
		}
		if !bytes.HasPrefix(bytes.TrimLeft(body, "\x04"), []byte("%!")) {
			return "", fmt.Errorf("postscript: unit is not PostScript")
		}
		first, _, _ := bytes.Cut(bytes.TrimLeft(body, "\x04"), []byte("\n"))
		return fmt.Sprintf("pass-through PostScript: %d bytes, %d pages (DSC), %q\n",
			len(data), dscPages(data), strings.TrimSpace(dscText(string(first)))), nil
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	enc := "ASCII85"
	if doc.Binary {
		enc = "binary"
	}
	filter := "RunLengthDecode"
	if doc.Level >= 3 {
		filter = "FlateDecode"
	}
	fmt.Fprintf(&b, "PostScript LanguageLevel %d, %s %s, %d pages, pjl %v\n", doc.Level, enc, filter, doc.Pages, doc.PJL)
	for _, s := range doc.Setup {
		fmt.Fprintf(&b, "setpagedevice %s\n", s)
	}
	for i, im := range doc.Images {
		ink := ""
		if im.Depth == 1 {
			ink = fmt.Sprintf(", %d black dots", im.Ink)
		}
		if im.RGB {
			ink = ", RGB"
		}
		fmt.Fprintf(&b, "page %d: image %dx%d depth %d scaled %s pt, %d bytes encoded%s\n",
			i+1, im.W, im.H, im.Depth, im.Scale, im.Encoded, ink)
	}
	return b.String(), nil
}

// decodeDoc parses the structure newDocWriter produces and decodes every image, keeping the
// samples.
func decodeDoc(data []byte) (*decoded, error) { return decodeImages(data, true) }

// decodeImages is decodeDoc keeping the samples or not: a dry run checks each image and keeps
// only its summary, so a long colour job is not held decoded.
func decodeImages(data []byte, keep bool) (*decoded, error) {
	doc := &decoded{}
	if rest, ok := bytes.CutPrefix(data, []byte(uel)); ok {
		i := bytes.Index(rest, []byte("%!"))
		if i < 0 {
			return nil, errInspect
		}
		doc.PJL, data = true, rest[i:]
	}
	if !bytes.HasPrefix(data, []byte("%!PS-Adobe-3.0\n%%Creator: "+creator+"\n")) {
		return nil, errInspect
	}
	if _, err := fmt.Sscanf(dscValue(data, "%%LanguageLevel:"), "%d", &doc.Level); err != nil {
		return nil, fmt.Errorf("postscript: %%%%LanguageLevel: %w", err)
	}
	if _, err := fmt.Sscanf(dscValue(data, "%%Pages:"), "%d", &doc.Pages); err != nil {
		return nil, fmt.Errorf("postscript: %%%%Pages: %w", err)
	}
	doc.Binary = dscValue(data, "%%DocumentData:") == "Binary"

	pos := bytes.Index(data, []byte("%%BeginSetup\n"))
	end := bytes.Index(data, []byte("%%EndSetup\n"))
	if pos < 0 || end < pos {
		return nil, fmt.Errorf("postscript: no setup section")
	}
	for _, line := range strings.Split(string(data[pos:end]), "\n") {
		if s, ok := strings.CutPrefix(line, "[{ << "); ok {
			s, _, _ = strings.Cut(s, " >> setpagedevice")
			doc.Setup = append(doc.Setup, s)
		}
	}

	pos = end
	call := []byte(" " + procImg + "\n")
	callRGB := []byte(" " + procRGB + "\n")
	for {
		i := bytes.Index(data[pos:], call)
		rgb := false
		if j := bytes.Index(data[pos:], callRGB); j >= 0 && (i < 0 || j < i) {
			i, rgb = j, true
		}
		if i < 0 {
			break
		}
		lineStart := bytes.LastIndexByte(data[:pos+i], '\n') + 1
		im := decodedImage{RGB: rgb, Depth: 8}
		var err error
		if rgb {
			_, err = fmt.Sscanf(string(data[lineStart:pos+i]), "%d %d", &im.W, &im.H)
		} else {
			_, err = fmt.Sscanf(string(data[lineStart:pos+i]), "%d %d %d", &im.W, &im.H, &im.Depth)
		}
		if err != nil {
			return nil, fmt.Errorf("postscript: image %d: %w", len(doc.Images)+1, err)
		}
		if prev := bytes.LastIndex(data[:lineStart], []byte(" scale\n")); prev >= 0 {
			from := bytes.LastIndex(data[:prev], []byte(" translate "))
			if from >= 0 {
				im.Scale = string(data[from+len(" translate ") : prev])
			}
		}
		start := pos + i + len(call)
		if rgb {
			start = pos + i + len(callRGB)
		}
		n, err := doc.decodeImage(&im, data[start:])
		if err != nil {
			return nil, fmt.Errorf("postscript: image %d: %w", len(doc.Images)+1, err)
		}
		switch {
		case im.Depth == 1:
			im.Ink = (&imaging.Bitmap{W: im.W, H: im.H, Stride: (im.W + 7) / 8, Pix: im.Data}).Ink()
		case im.RGB && doc.Level < 3 && keep:
			im.Data = interleave(im.Data, im.W)
		}
		if !keep {
			im.Data = nil
		}
		doc.Images = append(doc.Images, im)
		pos = start + n
	}
	if len(doc.Images) != doc.Pages || bytes.Count(data, []byte("\nshowpage\n")) != doc.Pages {
		return nil, fmt.Errorf("postscript: %d images, %d showpage, %%%%Pages: %d",
			len(doc.Images), bytes.Count(data, []byte("\nshowpage\n")), doc.Pages)
	}
	doc.Trailing = bytes.Contains(data[pos:], []byte("%%Trailer\n%%EOF\n"))
	if !doc.Trailing {
		return nil, fmt.Errorf("postscript: no %%%%Trailer / %%%%EOF")
	}
	return doc, nil
}

// decodeImage decodes the samples at the start of src and returns how many bytes of src they
// used, up to and including the filters' EOD markers.
func (doc *decoded) decodeImage(im *decodedImage, src []byte) (int, error) {
	if im.W <= 0 || im.H <= 0 || im.W > 1<<16 || im.H > 1<<16 || (im.Depth != 1 && im.Depth != 8) {
		return 0, fmt.Errorf("bad image %dx%d depth %d", im.W, im.H, im.Depth)
	}
	want := im.W * im.H
	if im.Depth == 1 {
		want = (im.W + 7) / 8 * im.H
	}
	if im.RGB {
		want *= 3
	}
	if want > 1<<28 {
		return 0, fmt.Errorf("image %dx%d too large", im.W, im.H)
	}
	raw, used := src, len(src)
	if !doc.Binary {
		end := bytes.Index(src, []byte("~>"))
		if end < 0 {
			return 0, fmt.Errorf("no ASCII85 end marker")
		}
		buf, err := io.ReadAll(ascii85.NewDecoder(bytes.NewReader(src[:end])))
		if err != nil {
			return 0, fmt.Errorf("ASCII85: %w", err)
		}
		raw, used = buf, end+2
	}
	var consumed int
	var err error
	if doc.Level >= 3 {
		im.Data, consumed, err = inflate(raw, want)
	} else {
		im.Data, consumed, err = runLengthDecode(raw, want)
	}
	if err != nil {
		return 0, err
	}
	if len(im.Data) != want {
		return 0, fmt.Errorf("%d sample bytes, want %d", len(im.Data), want)
	}
	if doc.Binary {
		used = consumed
	} else if consumed != len(raw) {
		return 0, fmt.Errorf("%d bytes after the filter's end of data", len(raw)-consumed)
	}
	im.Encoded = used
	return used, nil
}

// interleave turns rows of R, G and B samples into RGB rows.
func interleave(planar []byte, w int) []byte {
	out := make([]byte, len(planar))
	for r := 0; r < len(planar); r += 3 * w {
		pr, pg, pb := planar[r:r+w], planar[r+w:r+2*w], planar[r+2*w:r+3*w]
		for x := range w {
			out[r+3*x], out[r+3*x+1], out[r+3*x+2] = pr[x], pg[x], pb[x]
		}
	}
	return out
}

// runLengthDecode decodes a RunLengthDecode stream up to its EOD byte (128), returning the bytes
// of src it used.
func runLengthDecode(src []byte, limit int) ([]byte, int, error) {
	out := make([]byte, 0, min(limit, 4*len(src)))
	for i := 0; i < len(src); {
		h := int(src[i])
		i++
		switch {
		case h == 128:
			return out, i, nil
		case h < 128:
			if i+h+1 > len(src) {
				return nil, 0, fmt.Errorf("RunLengthDecode: literal past end")
			}
			out = append(out, src[i:i+h+1]...)
			i += h + 1
		default:
			if i >= len(src) {
				return nil, 0, fmt.Errorf("RunLengthDecode: repeat past end")
			}
			for k := 0; k < 257-h; k++ {
				out = append(out, src[i])
			}
			i++
		}
		if len(out) > limit {
			return nil, 0, fmt.Errorf("RunLengthDecode: more than %d bytes", limit)
		}
	}
	return nil, 0, fmt.Errorf("RunLengthDecode: no end of data")
}

func inflate(src []byte, limit int) ([]byte, int, error) {
	r := bytes.NewReader(src)
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("FlateDecode: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(zr, int64(limit)+1))
	if err != nil {
		return nil, 0, fmt.Errorf("FlateDecode: %w", err)
	}
	if err := zr.Close(); err != nil {
		return nil, 0, fmt.Errorf("FlateDecode: %w", err)
	}
	return out, len(src) - r.Len(), nil
}

// dscValue is the rest of the first line starting with key.
func dscValue(data []byte, key string) string {
	i := bytes.Index(data, []byte("\n"+key))
	if i < 0 {
		return ""
	}
	line := data[i+1+len(key):]
	if j := bytes.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	return strings.TrimSpace(string(line))
}
