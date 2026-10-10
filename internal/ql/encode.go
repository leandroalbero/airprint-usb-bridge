package ql

import (
	"encoding/binary"
	"fmt"
)

// RasterEncoder encodes a Job. Mirror, when set, overrides Profile.Mirror.
type RasterEncoder struct {
	mirror   *bool
	warnings []string
}

func (e *RasterEncoder) SetMirror(v bool) {
	if e == nil {
		return
	}
	e.mirror = &v
}

func (e *RasterEncoder) Warnings() []string {
	if e == nil || len(e.warnings) == 0 {
		return nil
	}
	return append([]string(nil), e.warnings...)
}

func (e *RasterEncoder) mirrored(p *Profile) bool {
	if e != nil && e.mirror != nil {
		return *e.mirror
	}
	return p != nil && p.Mirror
}

func (e *RasterEncoder) Encode(p *Profile, j Job) ([]byte, error) {
	pages, err := e.encodePages(p, j)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, pg := range pages {
		out = append(out, pg...)
	}
	return out, nil
}

func (e *RasterEncoder) encodePages(p *Profile, j Job) ([][]byte, error) {
	if e == nil {
		e = &RasterEncoder{}
	}
	e.warnings = Warnings(p, j)
	if err := validateJob(p, j); err != nil {
		return nil, err
	}
	mirror := e.mirrored(p)
	pages := make([][]byte, len(j.Pages))
	for i, bm := range j.Pages {
		body, err := encodePage(p, j, i, bm, mirror)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", i, err)
		}
		if i == 0 {
			body = append(preamble(p), body...)
		}
		pages[i] = body
	}
	return pages, nil
}

func validateJob(p *Profile, j Job) error {
	if p == nil {
		return fmt.Errorf("nil profile")
	}
	if p.BytesPerLine <= 0 || p.BytesPerLine > 255 || p.Pins != p.BytesPerLine*8 {
		return fmt.Errorf("profile raster width")
	}
	if len(j.Pages) == 0 {
		return fmt.Errorf("job has no pages")
	}
	if j.Options.MarginDots < 0 || j.Options.MarginDots > 0xFFFF {
		return fmt.Errorf("margin %d", j.Options.MarginDots)
	}
	if j.Options.AutoCut && (j.Options.CutEvery < 1 || j.Options.CutEvery > 255) {
		return fmt.Errorf("cut every %d", j.Options.CutEvery)
	}
	term := lastTerm(j.Options)
	if term != TermPrint && term != TermPrintAndFeed {
		return fmt.Errorf("last terminator 0x%02x", term)
	}
	for i, bm := range j.Pages {
		if bm == nil || bm.W <= 0 || bm.H <= 0 {
			return fmt.Errorf("page %d: empty bitmap", i)
		}
		if len(bm.Bits) != bm.W*bm.H {
			return fmt.Errorf("page %d: bits length %d", i, len(bm.Bits))
		}
		if bm.W > j.Media.PrintPins {
			return fmt.Errorf("page %d: %w (%d > %d)", i, ErrBitmapWidth, bm.W, j.Media.PrintPins)
		}
	}
	return nil
}

func lastTerm(o JobOptions) Terminator {
	if o.LastTerminator == 0 {
		return TermPrintAndFeed
	}
	return o.LastTerminator
}

func invalidate() []byte {
	b := make([]byte, 202)
	b[200] = 0x1b
	b[201] = 0x40
	return b
}

func preamble(p *Profile) []byte {
	b := invalidate()
	if p != nil && p.SupportsModeCmd {
		b = append(b, 0x1b, 0x69, 0x61, 0x01)
	}
	return b
}

func encodePage(p *Profile, j Job, index int, bm *Bitmap, mirror bool) ([]byte, error) {
	var b []byte
	b = append(b, printInfo(j, index, bm.H)...)
	b = append(b, 0x1b, 0x69, 0x4d, variousMode(j.Options))
	if j.Options.AutoCut {
		b = append(b, 0x1b, 0x69, 0x41, byte(j.Options.CutEvery))
	}
	b = append(b, 0x1b, 0x69, 0x4b, expandedMode(j.Options))
	var margin [2]byte
	binary.LittleEndian.PutUint16(margin[:], uint16(j.Options.MarginDots))
	b = append(b, 0x1b, 0x69, 0x64, margin[0], margin[1])
	for y := 0; y < bm.H; y++ {
		line, err := packRaster(p, j.Media, bm, y, mirror)
		if err != nil {
			return nil, err
		}
		// {s}=0x00 is "data transmission"; {n} is the byte count. Not a little-endian length.
		b = append(b, 'g', 0x00, byte(p.BytesPerLine))
		b = append(b, line...)
	}
	term := TermPrint
	if index == len(j.Pages)-1 {
		term = lastTerm(j.Options)
	}
	b = append(b, byte(term))
	return b, nil
}

func printInfo(j Job, index, lines int) []byte {
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
	var count [4]byte
	binary.LittleEndian.PutUint32(count[:], uint32(lines))
	return []byte{
		0x1b, 0x69, 0x7a,
		n1,
		byte(j.Media.Kind),
		byte(j.Media.WidthMM),
		byte(j.Media.LengthMM),
		count[0], count[1], count[2], count[3],
		n9,
		0,
	}
}

func variousMode(o JobOptions) byte {
	if o.AutoCut {
		return 0x40
	}
	return 0
}

func expandedMode(o JobOptions) byte {
	var n byte
	if o.CutAtEnd {
		n |= 0x08
	}
	if o.HighRes {
		n |= 0x40
	}
	return n
}
