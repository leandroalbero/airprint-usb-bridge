package zpl_test

import (
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/zpl"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
)

func TestCRC16KnownAnswer(t *testing.T) {
	// CRC-16/XMODEM check value from the CRC catalogue; CCITT-FALSE (init 0xFFFF) gives 0x29B1.
	if got := zpl.CRC16([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("CRC16 = %04X, want 31C3", got)
	}
	if got := crcXMODEM([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("test CRC = %04X, want 31C3", got)
	}
}

func bitmapFromRows(t *testing.T, w int, rows ...string) *imaging.Bitmap {
	t.Helper()
	bm := imaging.NewBitmap(w, len(rows))
	for y, r := range rows {
		if len(r) != w {
			t.Fatalf("row %d is %d wide", y, len(r))
		}
		for x, c := range r {
			bm.Set(x, y, c == '#')
		}
	}
	return bm
}

// The expected strings are worked out by hand from the ZPL guide's compression scheme.
func TestHexDataHandChecked(t *testing.T) {
	bm := bitmapFromRows(t, 32,
		"################################", // FFFFFFFF → "!"
		"....####........................", // 0F000000 → "0F,"
		"....####........................", // same row → ":"
		"...............................#", // 00000001 → 7 zeros = M, "M01"
		"####################............", // FFFFF000 → 5 F = K, "KF,"
	)
	if got, want := zpl.HexData(bm), "!0F,:M01KF,"; got != want {
		t.Fatalf("HexData = %q, want %q", got, want)
	}
	if got, want := string(zpl.GraphicField(bm, zpl.EncodingHex)), "^GFA,20,20,4,!0F,:M01KF,"; got != want {
		t.Fatalf("GraphicField = %q, want %q", got, want)
	}
	if got := readHex(t, zpl.HexData(bm), bm.Stride); string(got) != string(bm.Pix) {
		t.Fatalf("decoded % x, want % x", got, bm.Pix)
	}
}

func TestHexDataLongRuns(t *testing.T) {
	lastDot := func(bm *imaging.Bitmap) { bm.Set(bm.W-1, 0, true) }
	for _, tc := range []struct {
		w    int
		draw func(*imaging.Bitmap)
		want string
	}{
		{832, lastDot, "pM01"},   // 207 zeros = 200 (p) + 7 (M), then 01
		{2496, lastDot, "zqI01"}, // 623 zeros = 400 (z) + 220 (q) + 3 (I), then 01
		{40, func(bm *imaging.Bitmap) { copy(bm.Row(0), []byte{0xAA, 0xAB}) }, "IAB,"}, // AAAB000000
	} {
		bm := imaging.NewBitmap(tc.w, 1)
		tc.draw(bm)
		got := zpl.HexData(bm)
		if got != tc.want {
			t.Errorf("width %d: %q, want %q", tc.w, got, tc.want)
		}
		if raw := readHex(t, got, bm.Stride); string(raw) != string(bm.Pix) {
			t.Errorf("width %d: decoded % x", tc.w, raw)
		}
	}
}

func TestZ64Structure(t *testing.T) {
	bm := bitmapFromRows(t, 16, "#..............#", "################", "................")
	field := string(zpl.GraphicField(bm, zpl.EncodingZ64))
	head, data, ok := strings.Cut(field, ":Z64:")
	if !ok || head != "^GFA,6,6,2," {
		t.Fatalf("field %q", field)
	}
	if raw := readZ64(t, data); string(raw) != string(bm.Pix) {
		t.Fatalf("inflated % x, want % x", raw, bm.Pix)
	}
	if !strings.HasSuffix(field, ":"+strings.ToUpper(field[len(field)-4:])) {
		t.Fatalf("CRC not upper-case hex: %q", field)
	}
}
