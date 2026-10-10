package zpl

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
)

// MaxField is the largest ^GF byte count the ZPL guide documents; bigger labels are split into
// bands of whole rows, each its own ^FO … ^GFA field.
const MaxField = 99999

// Encode returns one ZPL label format for bm, which is the whole label (bm.W x bm.H dots).
func (d *Driver) Encode(bm *imaging.Bitmap) []byte {
	var b bytes.Buffer
	if d.opts.Darkness >= 0 {
		fmt.Fprintf(&b, "~SD%02d\n", d.opts.Darkness)
	}
	b.WriteString("^XA\n")
	switch {
	case d.continuous:
		b.WriteString("^MNN\n")
	case d.opts.Tracking == TrackMark:
		b.WriteString("^MNM\n")
	default:
		b.WriteString("^MNY\n")
	}
	fmt.Fprintf(&b, "^PW%d\n^LL%d\n^LH0,0\n", bm.W, bm.H)
	band := max(1, MaxField/max(bm.Stride, 1))
	for y := 0; y < bm.H; y += band {
		part := bm.Rows(y, y+band)
		if part.Blank() {
			continue
		}
		fmt.Fprintf(&b, "^FO0,%d", y)
		b.Write(GraphicField(part, d.opts.Encoding))
		b.WriteString("^FS\n")
	}
	b.WriteString("^XZ\n")
	return b.Bytes()
}

// GraphicField encodes bm as "^GFA,total,total,stride,data" (without ^FO or ^FS).
func GraphicField(bm *imaging.Bitmap, encoding string) []byte {
	n := bm.Stride * bm.H
	out := []byte(fmt.Sprintf("^GFA,%d,%d,%d,", n, n, bm.Stride))
	if encoding == EncodingHex {
		return append(out, HexData(bm)...)
	}
	return append(out, Z64(bm.Pix[:n])...)
}

// Z64 is ZPL's ":Z64:<base64 of zlib data>:<crc>" form, the CRC taken over the base64 text.
func Z64(raw []byte) string {
	var z bytes.Buffer
	w, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	w.Write(raw)
	w.Close()
	b64 := base64.StdEncoding.EncodeToString(z.Bytes())
	return fmt.Sprintf(":Z64:%s:%04X", b64, CRC16([]byte(b64)))
}

// CRC16 is CRC-16/XMODEM (CCITT polynomial 0x1021, initial value 0, no reflection).
func CRC16(data []byte) uint16 {
	var crc uint16
	for _, c := range data {
		crc ^= uint16(c) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// HexData is the ZPL ASCII hex form with its compression scheme: G-Y repeat the next digit 1-19
// times and g-z 20-400 times (counts add up), ',' fills the rest of the row with 0, '!' with 1,
// and ':' repeats the previous row.
func HexData(bm *imaging.Bitmap) string {
	var b strings.Builder
	prev := ""
	for y := 0; y < bm.H; y++ {
		row := strings.ToUpper(hex.EncodeToString(bm.Row(y)))
		if y > 0 && row == prev {
			b.WriteByte(':')
			continue
		}
		prev = row
		body, tail := row, ""
		if t := strings.TrimRight(row, "0"); len(t) < len(row) {
			body, tail = t, ","
		} else if t := strings.TrimRight(row, "F"); len(t) < len(row) {
			body, tail = t, "!"
		}
		for i := 0; i < len(body); {
			j := i + 1
			for j < len(body) && body[j] == body[i] {
				j++
			}
			if j-i >= 3 {
				b.WriteString(repeatCount(j - i))
				b.WriteByte(body[i])
			} else {
				b.WriteString(body[i:j])
			}
			i = j
		}
		b.WriteString(tail)
	}
	return b.String()
}

func repeatCount(n int) string {
	var b strings.Builder
	for ; n >= 400; n -= 400 {
		b.WriteByte('z')
	}
	if n >= 20 {
		b.WriteByte(byte('g' + n/20 - 1))
		n %= 20
	}
	if n > 0 {
		b.WriteByte(byte('G' + n - 1))
	}
	return b.String()
}
