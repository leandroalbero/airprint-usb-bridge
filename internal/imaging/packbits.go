package imaging

import (
	"errors"
	"fmt"
	"io"
)

// PackBits appends src compressed with PackBits (Apple TN1023; TIFF compression 32773; PCL raster
// compression mode 2) to dst. Runs of three or more equal bytes become repeat runs, everything
// else literal runs, each at most 128 bytes.
//
// The same stream followed by the end-of-data byte 0x80 is a PostScript RunLengthDecode stream.
func PackBits(dst, src []byte) []byte {
	for i := 0; i < len(src); {
		dst, i = packRun(dst, src, i)
	}
	return dst
}

// PackBitsTo writes PackBits(nil, src) to w a piece at a time, so its output is never held
// whole.
func PackBitsTo(w io.Writer, src []byte) error {
	const flushAt = 64 << 10
	buf := make([]byte, 0, flushAt+256)
	for i := 0; i < len(src); {
		buf, i = packRun(buf, src, i)
		if len(buf) >= flushAt || i == len(src) {
			if _, err := w.Write(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	return nil
}

// packRun appends the run or literal that starts at src[i] and returns where the next one starts.
func packRun(dst, src []byte, i int) ([]byte, int) {
	run := 1
	for i+run < len(src) && run < 128 && src[i+run] == src[i] {
		run++
	}
	if run >= 3 {
		return append(dst, byte(257-run), src[i]), i + run
	}
	start := i
	for i < len(src) && i-start < 128 {
		if i+2 < len(src) && src[i] == src[i+1] && src[i] == src[i+2] {
			break
		}
		i++
	}
	dst = append(dst, byte(i-start-1))
	return append(dst, src[start:i]...), i
}

var ErrPackBits = errors.New("imaging: bad PackBits data")

// UnpackBits decodes a PackBits stream, stopping at the end of src or after n bytes when n > 0.
// 0x80 is skipped, as TIFF requires.
func UnpackBits(src []byte, n int) ([]byte, error) {
	var out []byte
	for i := 0; i < len(src) && (n <= 0 || len(out) < n); {
		h := int(int8(src[i]))
		i++
		switch {
		case h >= 0:
			if i+h+1 > len(src) {
				return out, fmt.Errorf("%w: literal past end", ErrPackBits)
			}
			out = append(out, src[i:i+h+1]...)
			i += h + 1
		case h != -128:
			if i >= len(src) {
				return out, fmt.Errorf("%w: repeat past end", ErrPackBits)
			}
			for k := 0; k < 1-h; k++ {
				out = append(out, src[i])
			}
			i++
		}
	}
	if n > 0 && len(out) > n {
		return out, fmt.Errorf("%w: %d bytes, want %d", ErrPackBits, len(out), n)
	}
	return out, nil
}
