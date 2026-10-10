package ql

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func rasterLine(t *testing.T, b []byte) []byte {
	t.Helper()
	idx := bytes.Index(b, []byte{'g', 0x00, 0x5A})
	if idx < 0 || idx+93 > len(b) {
		t.Fatalf("no raster in %d bytes", len(b))
	}
	return b[idx+3 : idx+93]
}

func printInfoPages(b []byte) []byte {
	var pages []byte
	for i := 0; i+12 < len(b); i++ {
		if b[i] == 0x1b && b[i+1] == 0x69 && b[i+2] == 0x7a {
			pages = append(pages, b[i+11])
		}
	}
	return pages
}

func goldenJob(p *Profile, j Job, mirror bool) []byte {
	var out []byte
	out = append(out, preamble(p)...)
	for i, bm := range j.Pages {
		out = append(out, specPrintInfo(j, i, bm.H)...)
		mode := byte(0)
		if j.Options.AutoCut {
			mode = 0x40
		}
		out = append(out, 0x1b, 0x69, 0x4d, mode)
		if j.Options.AutoCut {
			out = append(out, 0x1b, 0x69, 0x41, byte(j.Options.CutEvery))
		}
		exp := byte(0)
		if j.Options.CutAtEnd {
			exp |= 0x08
		}
		if j.Options.HighRes {
			exp |= 0x40
		}
		out = append(out, 0x1b, 0x69, 0x4b, exp)
		var margin [2]byte
		binary.LittleEndian.PutUint16(margin[:], uint16(j.Options.MarginDots))
		out = append(out, 0x1b, 0x69, 0x64, margin[0], margin[1])
		for y := 0; y < bm.H; y++ {
			out = append(out, 'g', 0x00, 0x5A)
			out = append(out, specLine(p, j.Media, bm, y, mirror)...)
		}
		term := byte(0x0c)
		if i == len(j.Pages)-1 {
			term = byte(lastTerm(j.Options))
		}
		out = append(out, term)
	}
	return out
}

func specPrintInfo(j Job, index, lines int) []byte {
	n1 := byte(0x80)
	if j.Options.ValidateMedia {
		n1 |= 0x02 | 0x04 | 0x08
	}
	if j.Options.QualityPriority {
		n1 |= 0x40
	}
	n9 := byte(0)
	if index > 0 {
		n9 = 1
	}
	return []byte{0x1b, 0x69, 0x7a, n1, byte(j.Media.Kind), byte(j.Media.WidthMM), byte(j.Media.LengthMM), byte(lines), 0, 0, 0, n9, 0}
}

func specLine(p *Profile, m Media, bm *Bitmap, y int, mirror bool) []byte {
	line := make([]byte, p.BytesPerLine)
	for x := 0; x < bm.W; x++ {
		if !bm.At(x, y) {
			continue
		}
		pin := m.LeftPins + x + m.XTrim
		if mirror {
			pin = p.Pins - 1 - pin
		}
		line[pin/8] |= 1 << (7 - uint(pin%8))
	}
	return line
}
