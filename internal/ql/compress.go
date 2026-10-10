package ql

import "fmt"

const compressTIFF = 0x02

// appendCompressed adds one raster line in TIFF mode: Z for a blank line, else g with PackBits.
func appendCompressed(b, line []byte) []byte {
	blank := true
	for _, c := range line {
		if c != 0 {
			blank = false
			break
		}
	}
	if blank {
		return append(b, 'Z')
	}
	packed := packBits(line)
	return append(append(b, 'g', 0x00, byte(len(packed))), packed...)
}

// packBits encodes runs of 3 or more equal bytes as repeats and everything else as literals.
// Brother requires that a line never grows beyond the all-literal form (91 bytes for 90), so
// the literal form is used whenever it is not larger.
func packBits(src []byte) []byte {
	var out []byte
	lit := -1
	flush := func(end int) {
		for lit >= 0 && lit < end {
			n := min(end-lit, 128)
			out = append(out, byte(n-1))
			out = append(out, src[lit:lit+n]...)
			lit += n
		}
		lit = -1
	}
	for i := 0; i < len(src); {
		run := 1
		for i+run < len(src) && src[i+run] == src[i] && run < 128 {
			run++
		}
		if run >= 3 {
			flush(i)
			out = append(out, byte(1-run), src[i])
			i += run
			continue
		}
		if lit < 0 {
			lit = i
		}
		i += run
	}
	flush(len(src))
	if limit := literalSize(len(src)); len(out) > limit {
		lit = 0
		out = out[:0]
		flush(len(src))
	}
	return out
}

func literalSize(n int) int { return n + (n+127)/128 }

// unpackBits decodes one PackBits line of exactly n bytes.
func unpackBits(src []byte, n int) ([]byte, error) {
	out := make([]byte, 0, n)
	for i := 0; i < len(src); {
		h := int8(src[i])
		i++
		switch {
		case h >= 0:
			k := int(h) + 1
			if i+k > len(src) {
				return nil, fmt.Errorf("packbits: literal past end")
			}
			out = append(out, src[i:i+k]...)
			i += k
		case h == -128:
		default:
			if i >= len(src) {
				return nil, fmt.Errorf("packbits: repeat past end")
			}
			for k := 0; k < 1-int(h); k++ {
				out = append(out, src[i])
			}
			i++
		}
	}
	if len(out) != n {
		return nil, fmt.Errorf("packbits: %d bytes, want %d", len(out), n)
	}
	return out, nil
}
