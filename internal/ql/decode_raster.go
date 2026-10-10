package ql

import "fmt"

// DecodeRaster is the inverse of the encoder: job bytes back to page bitmaps,
// after mirroring and x_trim (LBL-034). Each page is media.PrintPins wide.
func DecodeRaster(p *Profile, m Media, b []byte) ([]*Bitmap, error) {
	if p == nil {
		return nil, fmt.Errorf("decode raster: nil profile")
	}
	if p.BytesPerLine <= 0 || m.PrintPins <= 0 {
		return nil, fmt.Errorf("decode raster: bad profile or media")
	}
	var pages []*Bitmap
	var lines [][]byte
	flush := func() {
		if len(lines) == 0 {
			return
		}
		bm := NewBitmap(m.PrintPins, len(lines))
		for y, line := range lines {
			unpackLine(p, m, bm, y, line)
		}
		pages = append(pages, bm)
		lines = nil
	}
	for i := 0; i < len(b); {
		if b[i] == 'g' {
			if i+2 >= len(b) {
				return nil, fmt.Errorf("decode raster: truncated raster at %d", i)
			}
			n := int(b[i+2])
			if i+3+n > len(b) {
				return nil, fmt.Errorf("decode raster: truncated line at %d", i)
			}
			lines = append(lines, append([]byte(nil), b[i+3:i+3+n]...))
			i += 3 + n
			continue
		}
		if b[i] == byte(TermPrint) || b[i] == byte(TermPrintAndFeed) {
			flush()
			i++
			continue
		}
		if b[i] == 0x1b {
			i = skipESC(b, i)
			continue
		}
		i++
	}
	flush()
	if len(pages) == 0 {
		return nil, fmt.Errorf("decode raster: no page")
	}
	return pages, nil
}

func unpackLine(p *Profile, m Media, bm *Bitmap, y int, line []byte) {
	for encoded := 0; encoded < p.Pins && encoded/8 < len(line); encoded++ {
		if line[encoded/8]&(1<<(7-uint(encoded%8))) == 0 {
			continue
		}
		logical := encoded
		if p.Mirror {
			logical = p.Pins - 1 - encoded
		}
		x := logical - m.LeftPins - m.XTrim
		if x >= 0 && x < bm.W {
			bm.Set(x, y, true)
		}
	}
}

func skipESC(b []byte, i int) int {
	if i+1 >= len(b) {
		return len(b)
	}
	if b[i+1] == '@' {
		return i + 2
	}
	if b[i+1] != 'i' || i+2 >= len(b) {
		return i + 2
	}
	switch b[i+2] {
	case 'S':
		return i + 3
	case 'a', 'M', 'A', 'K':
		if i+3 >= len(b) {
			return len(b)
		}
		return i + 4
	case 'd':
		if i+4 >= len(b) {
			return len(b)
		}
		return i + 5
	case 'z':
		if i+12 >= len(b) {
			return len(b)
		}
		return i + 13
	case 'U':
		if i+5 >= len(b) {
			return len(b)
		}
		return i + 6
	default:
		return i + 3
	}
}
