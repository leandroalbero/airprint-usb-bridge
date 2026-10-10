package zpl_test

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
)

// This file is a ZPL reader written from the ZPL II guide, kept apart from the driver's own
// Inspect so the tests do not check the encoder against itself.

// zplLabel is one decoded ^XA … ^XZ format.
type zplLabel struct {
	Commands []string // every command except ^GF/^FO/^FS, e.g. "^PW812"
	PW, LL   int
	Fields   []gfField
	Bitmap   *imaging.Bitmap // PW x LL, every field drawn at its ^FO
}

type gfField struct {
	X, Y          int
	Total, Stride int
	Encoding      string // "z64" or "hex"
}

var cmdRE = regexp.MustCompile(`[\^~][A-Z@][A-Z0-9@]`)

// readZPL decodes every label format in data and fails t on anything malformed.
func readZPL(t testing.TB, data []byte) []zplLabel {
	t.Helper()
	s := string(data)
	locs := cmdRE.FindAllStringIndex(s, -1)
	if len(locs) == 0 || strings.TrimSpace(s[:locs[0][0]]) != "" {
		t.Fatalf("data does not start with a ZPL command: %.40q", s)
	}
	var out []zplLabel
	var cur *zplLabel
	var pending []string // tilde commands before ^XA
	fx, fy := 0, 0
	for i, loc := range locs {
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		name := s[loc[0]:loc[1]]
		arg := strings.TrimRight(s[loc[1]:end], "\r\n")
		if name == "^XA" {
			cur = &zplLabel{Commands: pending}
			pending = nil
			continue
		}
		if cur == nil && name[0] == '~' {
			pending = append(pending, name+arg)
			continue
		}
		if cur == nil {
			t.Fatalf("%s%s outside ^XA … ^XZ", name, arg)
		}
		switch name {
		case "^PW":
			cur.PW = atoi(t, arg)
		case "^LL":
			cur.LL = atoi(t, arg)
		case "^FO":
			xy := strings.Split(arg, ",")
			if len(xy) != 2 {
				t.Fatalf("^FO%s", arg)
			}
			fx, fy = atoi(t, xy[0]), atoi(t, xy[1])
			continue
		case "^GF":
			if cur.Bitmap == nil {
				if cur.PW <= 0 || cur.LL <= 0 {
					t.Fatalf("^GF before ^PW/^LL")
				}
				cur.Bitmap = imaging.NewBitmap(cur.PW, cur.LL)
			}
			f, pix := readGF(t, arg)
			f.X, f.Y = fx, fy
			cur.Fields = append(cur.Fields, f)
			w := f.Stride * 8
			for r := 0; r < f.Total/f.Stride; r++ {
				for x := 0; x < w; x++ {
					if pix[r*f.Stride+x/8]&(0x80>>(x%8)) == 0 {
						continue
					}
					if fx+x >= cur.PW || fy+r >= cur.LL {
						t.Fatalf("ink at %d,%d outside the %dx%d label", fx+x, fy+r, cur.PW, cur.LL)
					}
					cur.Bitmap.Set(fx+x, fy+r, true)
				}
			}
			continue
		case "^FS":
			continue
		case "^XZ":
			if cur.Bitmap == nil {
				cur.Bitmap = imaging.NewBitmap(cur.PW, cur.LL)
			}
			out = append(out, *cur)
			cur = nil
			continue
		}
		cur.Commands = append(cur.Commands, name+arg)
	}
	if cur != nil || pending != nil {
		t.Fatal("format not closed with ^XZ")
	}
	return out
}

func atoi(t testing.TB, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("number %q", s)
	}
	return n
}

// readGF decodes "A,b,c,d,data": b and c are the uncompressed byte count, d the bytes per row.
func readGF(t testing.TB, arg string) (gfField, []byte) {
	t.Helper()
	parts := strings.SplitN(arg, ",", 5)
	if len(parts) != 5 || parts[0] != "A" {
		t.Fatalf("^GF%.30s: want A,b,c,d,data", arg)
	}
	b, c, d := atoi(t, parts[1]), atoi(t, parts[2]), atoi(t, parts[3])
	if b != c || d <= 0 || c%d != 0 {
		t.Fatalf("^GF counts %d,%d,%d", b, c, d)
	}
	if c > 99999 {
		t.Fatalf("^GF field of %d bytes, above the documented 99999", c)
	}
	f := gfField{Total: c, Stride: d}
	var pix []byte
	if z, ok := strings.CutPrefix(parts[4], ":Z64:"); ok {
		f.Encoding = "z64"
		pix = readZ64(t, z)
	} else {
		f.Encoding = "hex"
		pix = readHex(t, parts[4], d)
	}
	if len(pix) != c {
		t.Fatalf("^GF data is %d bytes, header says %d", len(pix), c)
	}
	return f, pix
}

func readZ64(t testing.TB, s string) []byte {
	t.Helper()
	b64, crc, ok := strings.Cut(s, ":")
	if !ok || len(crc) != 4 {
		t.Fatalf("Z64 without a 4-digit CRC: ...%.20q", s[max(0, len(s)-20):])
	}
	want, err := strconv.ParseUint(crc, 16, 16)
	if err != nil {
		t.Fatalf("Z64 CRC %q", crc)
	}
	if got := crcXMODEM([]byte(b64)); uint64(got) != want {
		t.Fatalf("Z64 CRC says %s, the base64 text gives %04X", crc, got)
	}
	z, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("Z64 base64: %v", err)
	}
	if len(z) < 2 || z[0]&0x0f != 8 || (uint16(z[0])<<8|uint16(z[1]))%31 != 0 {
		t.Fatalf("Z64 payload is not a zlib stream: % x", z[:min(len(z), 2)])
	}
	r, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("Z64 inflate: %v", err)
	}
	return raw
}

// crcXMODEM feeds the message one bit at a time through the CCITT polynomial, register cleared.
func crcXMODEM(msg []byte) uint16 {
	reg := uint16(0)
	for _, by := range msg {
		for bit := 7; bit >= 0; bit-- {
			in := uint16(by>>bit) & 1
			top := reg >> 15
			reg <<= 1
			if top^in == 1 {
				reg ^= 0x1021
			}
		}
	}
	return reg
}

// readHex expands ZPL's compressed ASCII hex: G..Y = 1..19 and g..z = 20..400 repeats of the next
// digit (summed), "," pads the row with 0, "!" with 1, ":" copies the previous row.
func readHex(t testing.TB, s string, stride int) []byte {
	t.Helper()
	width := stride * 2
	var rows []string
	var row strings.Builder
	rep := 0
	flush := func() {
		rows = append(rows, row.String())
		row.Reset()
	}
	for _, c := range s {
		switch {
		case c >= 'G' && c <= 'Y':
			rep += int(c - 'F')
		case c >= 'g' && c <= 'z':
			rep += 20 * int(c-'f')
		case c == ',':
			row.WriteString(strings.Repeat("0", width-row.Len()))
			flush()
		case c == '!':
			row.WriteString(strings.Repeat("F", width-row.Len()))
			flush()
		case c == ':':
			if row.Len() != 0 || len(rows) == 0 {
				t.Fatal("':' needs a previous whole row")
			}
			rows = append(rows, rows[len(rows)-1])
		case strings.ContainsRune("0123456789ABCDEF", c):
			if rep == 0 {
				rep = 1
			}
			for ; rep > 0; rep-- {
				row.WriteRune(c)
				if row.Len() == width {
					flush()
				}
			}
		default:
			t.Fatalf("hex data has %q", c)
		}
		if row.Len() > width {
			t.Fatal("hex row overflows")
		}
	}
	if row.Len() != 0 || rep != 0 {
		t.Fatalf("hex data ends mid-row (%d nibbles, repeat %d)", row.Len(), rep)
	}
	var out []byte
	for _, r := range rows {
		for i := 0; i < len(r); i += 2 {
			v, err := strconv.ParseUint(r[i:i+2], 16, 8)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, byte(v))
		}
	}
	return out
}

// diffBitmaps describes the first differences between two bitmaps of the same size.
func diffBitmaps(got, want *imaging.Bitmap) string {
	if got.W != want.W || got.H != want.H {
		return fmt.Sprintf("size %dx%d, want %dx%d", got.W, got.H, want.W, want.H)
	}
	var d []string
	for y := 0; y < want.H && len(d) < 5; y++ {
		for x := 0; x < want.W && len(d) < 5; x++ {
			if got.At(x, y) != want.At(x, y) {
				d = append(d, fmt.Sprintf("(%d,%d) got %v", x, y, got.At(x, y)))
			}
		}
	}
	return strings.Join(d, ", ")
}
