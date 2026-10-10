package cups

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

// Inspect describes a job's bytes for dry runs: a CUPS raster stream page by page, else a
// summary of PCL commands, ZPL labels or the leading bytes, depending on what the filter wrote.
func (d *Driver) Inspect(data []byte) (string, error) {
	return Inspect(data)
}

// Inspect is Driver.Inspect without a driver.
func Inspect(data []byte) (string, error) {
	switch {
	case len(data) >= 4 && (string(data[:4]) == "RaS3" || string(data[:4]) == "3SaR"):
		return inspectRaster(data)
	case bytes.HasPrefix(data, []byte("\x1b%-12345X")) || bytes.HasPrefix(data, []byte("\x1bE")):
		return fmt.Sprintf("%d bytes PCL: %s", len(data), pclSummary(data, 40)), nil
	case bytes.Contains(data, []byte("^XA")):
		return fmt.Sprintf("%d bytes ZPL: %d ^XA, %d ^XZ, %d ~DG", len(data),
			bytes.Count(data, []byte("^XA")), bytes.Count(data, []byte("^XZ")), bytes.Count(data, []byte("~DG"))), nil
	}
	n := min(len(data), 48)
	return fmt.Sprintf("%d bytes: %q", len(data), data[:n]), nil
}

// inspectRaster lists the pages of an uncompressed v3 stream.
func inspectRaster(data []byte) (string, error) {
	var o binary.ByteOrder = binary.BigEndian
	if string(data[:4]) == "3SaR" {
		o = binary.LittleEndian
	}
	var b strings.Builder
	pos, n := 4, 0
	for pos < len(data) {
		h, err := UnmarshalHeader(data[pos:], o)
		if err != nil {
			return b.String(), err
		}
		pos += HeaderSize
		f, err := layout(&h, int(h.CupsWidth), int(h.CupsHeight))
		if err != nil {
			return b.String(), fmt.Errorf("page %d: %w", n+1, err)
		}
		size := int(h.CupsBytesPerLine) * f.rows
		if int(h.CupsBytesPerLine) != f.bytesPerRow || pos+size > len(data) {
			return b.String(), fmt.Errorf("page %d: %d bytes per line x %d rows does not fit the %d bytes left",
				n+1, h.CupsBytesPerLine, f.rows, len(data)-pos)
		}
		n++
		fmt.Fprintf(&b, "page %d: %s %dx%dpt %dx%d px @%dx%d dpi, %d-bit cs %d order %d, %d bytes/line, bbox %v, duplex %v tumble %v\n",
			n, h.CupsPageSizeName, h.PageSize[0], h.PageSize[1], h.CupsWidth, h.CupsHeight,
			h.HWResolution[0], h.HWResolution[1], h.CupsBitsPerColor, h.CupsColorSpace, h.CupsColorOrder,
			h.CupsBytesPerLine, h.ImagingBoundingBox, h.Duplex, h.Tumble)
		pos += size
	}
	return b.String(), nil
}

// pclSummary lists PCL escape sequences in order, runs of the same command collapsed, raster
// data skipped, up to max entries.
func pclSummary(data []byte, max int) string {
	var out []string
	last, count := "", 0
	emit := func() {
		if last == "" {
			return
		}
		if count > 1 {
			out = append(out, fmt.Sprintf("%s x%d", last, count))
		} else {
			out = append(out, last)
		}
	}
	add := func(cmd string) {
		if cmd == last {
			count++
			return
		}
		emit()
		last, count = cmd, 1
	}
	for i := 0; i < len(data) && len(out) < max; {
		if data[i] != 0x1b {
			if bytes.HasPrefix(data[i:], []byte("@PJL")) {
				j := bytes.IndexByte(data[i:], '\n')
				if j < 0 {
					j = len(data) - i - 1
				}
				add(strings.TrimSpace(string(data[i : i+j])))
				i += j + 1
				continue
			}
			i++
			continue
		}
		i++
		if i >= len(data) {
			break
		}
		c := data[i]
		if c < 0x21 || c > 0x2f {
			add("ESC " + string(c))
			i++
			continue
		}
		param := string(c)
		i++
		group := ""
		if i < len(data) && data[i] >= 0x60 && data[i] <= 0x7e {
			group = string(data[i])
			i++
		}
		for i < len(data) {
			j := i
			for j < len(data) && (data[j] == '+' || data[j] == '-' || data[j] == '.' || (data[j] >= '0' && data[j] <= '9')) {
				j++
			}
			if j >= len(data) {
				i = j
				break
			}
			value, term := string(data[i:j]), data[j]
			i = j + 1
			upper := term &^ 0x20
			cmd := "ESC " + param + group + value + string(upper)
			if upper == 'W' || (param == "*" && group == "b" && upper == 'V') {
				var n int
				fmt.Sscanf(value, "%d", &n)
				cmd = "ESC " + param + group + "#" + string(upper)
				if n > 0 {
					i += min(n, len(data)-i)
				}
			}
			add(cmd)
			if term < 0x60 || term > 0x7e {
				break
			}
		}
	}
	emit()
	return strings.Join(out, ", ")
}
