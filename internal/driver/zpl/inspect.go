package zpl

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strconv"
	"strings"
)

// Inspect lists the ZPL commands in data, one format per line, with each ^GFA field decoded and
// checked (counts, Z64 CRC, size after decompression).
func (d *Driver) Inspect(data []byte) (string, error) { return Inspect(data) }

// Inspect is the driver's Inspector without a Driver.
func Inspect(data []byte) (string, error) {
	var lines []string
	var cur []string
	s := string(data)
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\r' || c == '\n' || c == ' ' || c == '\t' {
			i++
			continue
		}
		if c != '^' && c != '~' {
			return "", fmt.Errorf("zpl: byte %d: %q outside a command", i, c)
		}
		end := i + 1
		for end < len(s) && s[end] != '^' && s[end] != '~' {
			end++
		}
		if end-i < 3 {
			return "", fmt.Errorf("zpl: byte %d: truncated command %q", i, s[i:end])
		}
		name, args := strings.ToUpper(s[i:i+3]), strings.TrimRight(s[i+3:end], "\r\n")
		i = end
		if name == "^GF" {
			desc, err := inspectGF(args)
			if err != nil {
				return "", err
			}
			cur = append(cur, desc)
			continue
		}
		cur = append(cur, name+args)
		if name == "^XZ" {
			lines = append(lines, strings.Join(cur, " "))
			cur = nil
		}
	}
	if len(cur) > 0 {
		return "", fmt.Errorf("zpl: format not closed with ^XZ: %s", strings.Join(cur, " "))
	}
	if len(lines) == 0 {
		return "", errors.New("zpl: no label format")
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func inspectGF(args string) (string, error) {
	pix, stride, enc, err := DecodeGF(args)
	if err != nil {
		return "", err
	}
	ink := 0
	for _, b := range pix {
		ink += bits.OnesCount8(b)
	}
	return fmt.Sprintf("^GFA[%s %dx%d dots, %d bytes, ink %d]", enc, stride*8, len(pix)/stride, len(pix), ink), nil
}

// DecodeGF decodes the arguments of a ^GF command ("A,b,c,d,data") into packed rows.
func DecodeGF(args string) (pix []byte, stride int, encoding string, err error) {
	f := strings.SplitN(args, ",", 5)
	if len(f) != 5 || f[0] != "A" {
		return nil, 0, "", fmt.Errorf("zpl: ^GF%.20s: want A,b,c,d,data", args)
	}
	var n [3]int
	for k := range n {
		if n[k], err = strconv.Atoi(f[k+1]); err != nil || n[k] <= 0 {
			return nil, 0, "", fmt.Errorf("zpl: ^GF count %q", f[k+1])
		}
	}
	total, stride := n[1], n[2]
	if n[0] != total || total%stride != 0 {
		return nil, 0, "", fmt.Errorf("zpl: ^GF counts %d,%d,%d", n[0], total, stride)
	}
	data := strings.TrimRight(f[4], "\r\n")
	if rest, ok := strings.CutPrefix(data, ":Z64:"); ok {
		pix, err = decodeZ64(rest)
		encoding = "z64"
	} else {
		pix, err = decodeHex(data, stride, total)
		encoding = "hex"
	}
	if err != nil {
		return nil, 0, "", err
	}
	if len(pix) != total {
		return nil, 0, "", fmt.Errorf("zpl: ^GF holds %d bytes, header says %d", len(pix), total)
	}
	return pix, stride, encoding, nil
}

func decodeZ64(s string) ([]byte, error) {
	k := strings.LastIndexByte(s, ':')
	if k < 0 {
		return nil, errors.New("zpl: Z64 data without CRC")
	}
	b64, sum := s[:k], s[k+1:]
	want, err := strconv.ParseUint(sum, 16, 16)
	if err != nil || len(sum) != 4 {
		return nil, fmt.Errorf("zpl: Z64 CRC %q", sum)
	}
	if got := CRC16([]byte(b64)); uint64(got) != want {
		return nil, fmt.Errorf("zpl: Z64 CRC %04X, data gives %04X", want, got)
	}
	z, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("zpl: Z64 base64: %w", err)
	}
	r, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, fmt.Errorf("zpl: Z64 zlib: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("zpl: Z64 zlib: %w", err)
	}
	return out, nil
}

func decodeHex(s string, stride, total int) ([]byte, error) {
	rowNibbles := 2 * stride
	var out, row []byte
	count := 0
	endRow := func() {
		out = append(out, row...)
		row = row[len(row):]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'G' && c <= 'Y':
			count += int(c-'G') + 1
		case c >= 'g' && c <= 'z':
			count += 20 * (int(c-'g') + 1)
		case c == ',' || c == '!':
			fill := byte('0')
			if c == '!' {
				fill = 'F'
			}
			for len(row) < rowNibbles {
				row = append(row, fill)
			}
			endRow()
		case c == ':':
			if len(row) != 0 || len(out) < rowNibbles {
				return nil, fmt.Errorf("zpl: hex ':' at %d without a previous row", i)
			}
			out = append(out, out[len(out)-rowNibbles:]...)
		case strings.IndexByte("0123456789ABCDEFabcdef", c) >= 0:
			n := max(count, 1)
			count = 0
			for ; n > 0; n-- {
				row = append(row, c)
				if len(row) == rowNibbles {
					endRow()
				}
			}
		case c == '\r' || c == '\n':
		default:
			return nil, fmt.Errorf("zpl: hex data byte %q at %d", c, i)
		}
		if len(out) > 2*total {
			return nil, fmt.Errorf("zpl: hex data longer than %d bytes", total)
		}
	}
	if len(row) != 0 || count != 0 {
		return nil, errors.New("zpl: hex data ends inside a row")
	}
	pix := make([]byte, len(out)/2)
	for i := range pix {
		v, err := strconv.ParseUint(string(out[2*i:2*i+2]), 16, 8)
		if err != nil {
			return nil, err
		}
		pix[i] = byte(v)
	}
	return pix, nil
}
