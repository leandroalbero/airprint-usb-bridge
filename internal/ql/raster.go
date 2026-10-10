package ql

import "fmt"

// packRaster maps image row y into one head-width line.
// Ref order (mirror false): pin 0 is byte 0 bit 7. The 62 mm full-black line is 00 0F FF… F0 00.
// Mirror reverses that line so image x=0 lands on the opposite edge.
func packRaster(p *Profile, m Media, row *Bitmap, y int, mirror bool) ([]byte, error) {
	if p.BytesPerLine <= 0 || p.Pins != p.BytesPerLine*8 {
		return nil, fmt.Errorf("profile pins/bytes")
	}
	line := make([]byte, p.BytesPerLine)
	for x := 0; x < row.W; x++ {
		if !row.At(x, y) {
			continue
		}
		pin := m.LeftPins + x + m.XTrim
		if pin < 0 || pin >= p.Pins {
			return nil, fmt.Errorf("pin %d out of range", pin)
		}
		if mirror {
			pin = p.Pins - 1 - pin
		}
		line[pin/8] |= 1 << (7 - uint(pin%8))
	}
	return line, nil
}
