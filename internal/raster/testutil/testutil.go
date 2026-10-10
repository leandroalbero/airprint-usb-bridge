// Package testutil writes synthetic URF and PWG raster streams for tests.
package testutil

import (
	"bytes"
	"encoding/binary"
)

// Colour spaces as they appear in the page headers.
const (
	URFGray = 0
	URFRGB  = 1
	URFCMYK = 6

	PWGBlack = 3
	PWGCMYK  = 6
	PWGGray  = 18
	PWGRGB   = 19
)

type Page struct {
	Width, Height int
	XDPI, YDPI    int // YDPI 0 means XDPI; URF has a single resolution field
	BPP           int // 8, 24 or 32
	ColorSpace    int
	Pix           []byte // Width*Height*BPP/8 samples, row-major
}

// GrayPage wraps an 8-bit additive gray image (0 = black).
func GrayPage(w, h, dpi int, gray []byte) Page {
	return Page{Width: w, Height: h, XDPI: dpi, BPP: 8, Pix: gray}
}

// EncodeURF writes an Apple URF stream whose header announces len(pages).
func EncodeURF(pages ...Page) []byte {
	return EncodeURFCount(uint32(len(pages)), pages...)
}

// EncodeURFCount writes a URF stream with an arbitrary page count field (0 = unknown).
func EncodeURFCount(count uint32, pages ...Page) []byte {
	var b bytes.Buffer
	b.WriteString("UNIRAST\x00")
	_ = binary.Write(&b, binary.BigEndian, count)
	for _, p := range pages {
		b.Write(URFPageHeader(p))
		writeLines(&b, p, p.ColorSpace == URFCMYK)
	}
	return b.Bytes()
}

func URFPageHeader(p Page) []byte {
	h := make([]byte, 32)
	h[0] = byte(p.BPP)
	h[1] = byte(p.ColorSpace)
	h[2] = 1
	h[3] = 4
	binary.BigEndian.PutUint32(h[12:], uint32(p.Width))
	binary.BigEndian.PutUint32(h[16:], uint32(p.Height))
	binary.BigEndian.PutUint32(h[20:], uint32(p.XDPI))
	return h
}

// EncodePWG writes a PWG raster (RaS2) stream.
func EncodePWG(pages ...Page) []byte {
	var b bytes.Buffer
	b.WriteString("RaS2")
	for _, p := range pages {
		b.Write(PWGPageHeader(p))
		writeLines(&b, p, p.ColorSpace == PWGBlack || p.ColorSpace == PWGCMYK)
	}
	return b.Bytes()
}

func PWGPageHeader(p Page) []byte {
	h := make([]byte, 1796)
	copy(h, "PwgRaster")
	ydpi := p.YDPI
	if ydpi == 0 {
		ydpi = p.XDPI
	}
	put := func(off, v int) { binary.BigEndian.PutUint32(h[off:], uint32(v)) }
	put(276, p.XDPI)
	put(280, ydpi)
	if p.XDPI > 0 && ydpi > 0 {
		put(352, p.Width*72/p.XDPI)
		put(356, p.Height*72/ydpi)
	}
	put(372, p.Width)
	put(376, p.Height)
	bpc := 8
	put(384, bpc)
	put(388, p.BPP)
	put(392, p.Width*p.BPP/8)
	put(400, p.ColorSpace)
	put(420, p.BPP/bpc)
	return h
}

// writeLines emits the shared URF/PWG line compression: a line-repeat byte, then
// pixel runs, literals and an end-of-line fill when the tail is paper.
func writeLines(b *bytes.Buffer, p Page, subtractive bool) {
	bypp := p.BPP / 8
	stride := p.Width * bypp
	var paper byte = 0xff
	if subtractive {
		paper = 0
	}
	line := func(y int) []byte { return p.Pix[y*stride : (y+1)*stride] }
	for y := 0; y < p.Height; {
		cur := line(y)
		rep := 1
		for y+rep < p.Height && rep < 256 && bytes.Equal(line(y+rep), cur) {
			rep++
		}
		b.WriteByte(byte(rep - 1))
		writeLine(b, cur, bypp, paper)
		y += rep
	}
}

func writeLine(b *bytes.Buffer, line []byte, bypp int, paper byte) {
	n := len(line) / bypp
	px := func(i int) []byte { return line[i*bypp : (i+1)*bypp] }
	end := n
	for end > 0 && isPaper(px(end-1), paper) {
		end--
	}
	for x := 0; x < end; {
		run := 1
		for x+run < end && run < 128 && bytes.Equal(px(x+run), px(x)) {
			run++
		}
		if run > 1 {
			b.WriteByte(byte(run - 1))
			b.Write(px(x))
			x += run
			continue
		}
		lit := 1
		for x+lit < end && lit < 128 && (x+lit+1 >= end || !bytes.Equal(px(x+lit), px(x+lit+1))) {
			lit++
		}
		if lit == 1 {
			b.WriteByte(0)
		} else {
			b.WriteByte(byte(257 - lit))
		}
		b.Write(line[x*bypp : (x+lit)*bypp])
		x += lit
	}
	if end < n {
		b.WriteByte(128)
	}
}

func isPaper(px []byte, paper byte) bool {
	for _, v := range px {
		if v != paper {
			return false
		}
	}
	return true
}
