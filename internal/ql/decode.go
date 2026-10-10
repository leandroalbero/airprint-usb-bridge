package ql

import (
	"encoding/hex"
	"fmt"
	"strings"
)

func decode(b []byte) (string, error) {
	var lines []string
	i := 0
	for i < len(b) {
		if b[i] == 0x00 {
			n := 1
			for i+n < len(b) && b[i+n] == 0x00 {
				n++
			}
			if n >= 200 {
				lines = append(lines, fmt.Sprintf("invalidate %d", n))
			} else {
				lines = append(lines, fmt.Sprintf("zeros %d", n))
			}
			i += n
			continue
		}
		line, next, ok := decodeCmd(b, i)
		if ok {
			lines = append(lines, line)
			i = next
			continue
		}
		start := i
		for i < len(b) && !atCmd(b, i) {
			i++
		}
		if i == start {
			i++
		}
		lines = append(lines, fmt.Sprintf("garbage at %d len=%d hex=%s", start, i-start, hex.EncodeToString(b[start:i])))
	}
	return strings.Join(lines, "\n"), nil
}

func atCmd(b []byte, i int) bool {
	if i >= len(b) {
		return false
	}
	switch b[i] {
	case 0x00, 0x0c, 0x1a, 'g', 'Z', 'M':
		return true
	case 0x1b:
		return true
	default:
		return false
	}
}

func decodeCmd(b []byte, i int) (string, int, bool) {
	switch b[i] {
	case 0x0c:
		return "print", i + 1, true
	case 0x1a:
		return "print-and-feed", i + 1, true
	case 'Z':
		return "zero-raster", i + 1, true
	case 'M':
		if i+1 >= len(b) {
			return fmt.Sprintf("truncated compression at %d", i), len(b), true
		}
		return fmt.Sprintf("compression 0x%02x", b[i+1]), i + 2, true
	case 'g':
		return decodeRaster(b, i)
	case 0x1b:
		return decodeESC(b, i)
	default:
		return "", i, false
	}
}

func decodeRaster(b []byte, i int) (string, int, bool) {
	count := 0
	j := i
	for j < len(b) && b[j] == 'g' {
		if j+2 >= len(b) || j+3+int(b[j+2]) > len(b) {
			if count == 0 {
				return fmt.Sprintf("truncated raster at %d", i), len(b), true
			}
			break
		}
		j += 3 + int(b[j+2])
		count++
	}
	if count == 0 {
		return fmt.Sprintf("truncated raster at %d", i), len(b), true
	}
	return fmt.Sprintf("raster-lines %d", count), j, true
}

func decodeESC(b []byte, i int) (string, int, bool) {
	if i+1 >= len(b) {
		return fmt.Sprintf("truncated esc at %d", i), len(b), true
	}
	if b[i+1] == '@' && i+1 < len(b) {
		if b[i+1] == 0x40 {
			return "initialize", i + 2, true
		}
	}
	if b[i+1] != 0x69 {
		return "", i, false
	}
	if i+2 >= len(b) {
		return fmt.Sprintf("truncated esc-i at %d", i), len(b), true
	}
	switch b[i+2] {
	case 'S':
		return "status-request", i + 3, true
	case 'a':
		if i+3 >= len(b) {
			return fmt.Sprintf("truncated mode-switch at %d", i), len(b), true
		}
		return fmt.Sprintf("mode-switch %d", b[i+3]), i + 4, true
	case 'z':
		return decodePrintInfo(b, i)
	case 'M':
		if i+3 >= len(b) {
			return fmt.Sprintf("truncated various-mode at %d", i), len(b), true
		}
		ac := 0
		if b[i+3]&0x40 != 0 {
			ac = 1
		}
		return fmt.Sprintf("various-mode auto-cut=%d", ac), i + 4, true
	case 'A':
		if i+3 >= len(b) {
			return fmt.Sprintf("truncated cut-every at %d", i), len(b), true
		}
		return fmt.Sprintf("cut-every %d", b[i+3]), i + 4, true
	case 'K':
		if i+3 >= len(b) {
			return fmt.Sprintf("truncated expanded-mode at %d", i), len(b), true
		}
		n := b[i+3]
		cut, hi := 0, 0
		if n&0x08 != 0 {
			cut = 1
		}
		if n&0x40 != 0 {
			hi = 1
		}
		return fmt.Sprintf("expanded-mode flags=0x%02x cut-at-end=%d high-res=%d", n, cut, hi), i + 4, true
	case 'd':
		if i+4 >= len(b) {
			return fmt.Sprintf("truncated margin at %d", i), len(b), true
		}
		margin := int(b[i+3]) | int(b[i+4])<<8
		return fmt.Sprintf("margin %d", margin), i + 5, true
	default:
		return "", i, false
	}
}

func decodePrintInfo(b []byte, i int) (string, int, bool) {
	if i+12 >= len(b) {
		return fmt.Sprintf("truncated print-info at %d", i), len(b), true
	}
	n1 := b[i+3]
	kind := b[i+4]
	width := b[i+5]
	length := b[i+6]
	lines := int(b[i+7]) | int(b[i+8])<<8 | int(b[i+9])<<16 | int(b[i+10])<<24
	page := b[i+11]
	return fmt.Sprintf("print-info flags=0x%02x media=0x%02x width=%d length=%d lines=%d page=%d", n1, kind, width, length, lines, page), i + 13, true
}
