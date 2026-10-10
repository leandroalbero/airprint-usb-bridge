package ql

import (
	"bytes"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func embedded(t *testing.T, stem string) *Profile {
	t.Helper()
	p, ok, err := EmbeddedProfileByName(stem)
	if err != nil || !ok {
		t.Fatalf("%s: %v %v", stem, ok, err)
	}
	return p
}

func solid(w, h int) *Bitmap {
	b := NewBitmap(w, h)
	for i := range b.Bits {
		b.Bits[i] = true
	}
	return b
}

func labelJob(m Media, pages ...*Bitmap) Job {
	o := DefaultJobOptions()
	return Job{Media: m, Options: o, Pages: pages}
}

// A full-width black line on a 1296-pin head, worked out from the QL-1100 raster line table:
// with the shipped mirror the right-margin pins come first, so 62 mm (544/696/56) is 7 zero
// bytes, 87 bytes of FF and 68 zero bytes; 102 mm (76/1164/56) is 7 zero bytes, 145 FF, F0 and
// 9 zero bytes.
func TestEncodeWideHeadLines(t *testing.T) {
	p := embedded(t, "ql1100")
	for id, want := range map[string][]byte{
		"62":  concat(zeros(7), repeat(0xFF, 87), zeros(68)),
		"102": concat(zeros(7), repeat(0xFF, 145), []byte{0xF0}, zeros(9)),
	} {
		m := mediaByID(t, p, id)
		b, err := NewEncoder().Encode(p, labelJob(m, solid(m.PrintPins, 1)))
		if err != nil {
			t.Fatal(err)
		}
		i := bytes.Index(b, []byte{'g', 0x00, 162})
		if i < 0 || !bytes.Equal(b[i+3:i+3+162], want) {
			t.Fatalf("%s: line % x", id, b[i+3:])
		}
	}
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func zeros(n int) []byte            { return make([]byte, n) }
func repeat(c byte, n int) []byte   { return bytes.Repeat([]byte{c}, n) }

func TestEncodeCommandSupport(t *testing.T) {
	escA := []byte{0x1b, 0x69, 0x41}
	escK := []byte{0x1b, 0x69, 0x4b}
	escM := func(n byte) []byte { return []byte{0x1b, 0x69, 0x4d, n} }
	modeCmd := []byte{0x1b, 0x40, 0x1b, 0x69, 0x61, 0x01}
	for _, c := range []struct {
		stem            string
		nul             int
		mode, a, k, cut bool
	}{
		{"ql500", 200, false, false, false, false},
		{"ql560", 200, false, false, false, true},
		{"ql570", 200, false, true, true, true},
		{"ql650td", 200, true, false, true, true},
		{"ql700", 200, false, true, true, true},
		{"ql720nw", 200, true, true, true, true},
		{"ql800", 400, true, true, true, true},
		{"ql1110nwb", 350, true, true, true, true},
	} {
		p := embedded(t, c.stem)
		m := mediaByID(t, p, "62")
		b, err := NewEncoder().Encode(p, labelJob(m, solid(10, 3)))
		if err != nil {
			t.Fatalf("%s: %v", c.stem, err)
		}
		if bytes.ContainsFunc(b[:c.nul], func(r rune) bool { return r != 0 }) || b[c.nul] != 0x1b || b[c.nul+1] != 0x40 {
			t.Errorf("%s: preamble is not %d NULs + ESC @", c.stem, c.nul)
		}
		cut := byte(0)
		if c.cut {
			cut = 0x40
		}
		got := []bool{bytes.Contains(b, modeCmd), bytes.Contains(b, escA), bytes.Contains(b, escK), bytes.Contains(b, escM(cut))}
		want := []bool{c.mode, c.a, c.k, true}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: ESC i a, ESC i A, ESC i K, ESC i M %02x present = %v, want %v", c.stem, cut, got, want)
				break
			}
		}
		if bytes.Contains(b, []byte{'M', compressTIFF}) {
			t.Errorf("%s: compression without Options.Compress", c.stem)
		}
	}
}

func TestEncodeRejectsUnsupportedOptions(t *testing.T) {
	ql700 := embedded(t, "ql700")
	j := labelJob(mediaByID(t, ql700, "62"), solid(10, 3))
	j.Options.Compress = true
	if _, err := NewEncoder().Encode(ql700, j); err == nil || !strings.Contains(err.Error(), "compression") {
		t.Fatalf("QL-700 compress: %v", err)
	}
	ql1100 := embedded(t, "ql1100")
	j = labelJob(mediaByID(t, ql1100, "62"), solid(10, 3))
	j.Options.HighRes = true
	if _, err := NewEncoder().Encode(ql1100, j); err == nil || !strings.Contains(err.Error(), "high-resolution") {
		t.Fatalf("QL-1100 high-res: %v", err)
	}
}

func TestEncodeCompressedRoundTrip(t *testing.T) {
	for _, stem := range []string{"ql720nw", "ql1100"} {
		p := embedded(t, stem)
		m := mediaByID(t, p, "62")
		bm := NewBitmap(m.PrintPins, 12)
		for y := 0; y < bm.H; y++ {
			for x := 0; x < bm.W; x++ {
				bm.Set(x, y, y >= 4 && y < 9 && (x/3+y)%4 != 0)
			}
		}
		j := labelJob(m, bm)
		j.Options.Compress = true
		b, err := NewEncoder().Encode(p, j)
		if err != nil {
			t.Fatal(err)
		}
		plain := labelJob(m, bm)
		raw, _ := NewEncoder().Encode(p, plain)
		if len(b) >= len(raw) {
			t.Errorf("%s: compressed %d bytes, plain %d", stem, len(b), len(raw))
		}
		margin := bytes.Index(b, []byte{0x1b, 0x69, 0x64})
		if margin < 0 || !bytes.Equal(b[margin+5:margin+7], []byte{'M', compressTIFF}) {
			t.Fatalf("%s: M 02 does not follow the margin command", stem)
		}
		pages, err := DecodeRaster(p, m, b)
		if err != nil || len(pages) != 1 {
			t.Fatalf("%s: %v", stem, err)
		}
		if pages[0].W != bm.W || pages[0].H != bm.H || !slices.Equal(pages[0].Bits, bm.Bits) {
			t.Fatalf("%s: round trip differs", stem)
		}
		text, _ := Decode(b)
		if !strings.Contains(text, "compression 0x02") || !strings.Contains(text, "raster-lines 12 (7 zero)") {
			t.Fatalf("%s: decode\n%s", stem, text)
		}
	}
}

// The worked example from the reference's "Select compression mode" section.
func TestPackBitsReferenceExample(t *testing.T) {
	plain := concat(zeros(20), []byte{0x22, 0x22, 0x23, 0xBA, 0xBF, 0xA2, 0x22, 0x2B})
	ref := []byte{0xED, 0x00, 0xFF, 0x22, 0x05, 0x23, 0xBA, 0xBF, 0xA2, 0x22, 0x2B}
	got, err := unpackBits(ref, len(plain))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("reference example decodes to % x, %v", got, err)
	}
	// Two equal bytes stay in the literal run: same length as the reference's FF 22 split.
	if enc := packBits(plain); !bytes.Equal(enc, []byte{0xED, 0x00, 0x07, 0x22, 0x22, 0x23, 0xBA, 0xBF, 0xA2, 0x22, 0x2B}) {
		t.Fatalf("packBits % x", enc)
	}
	if enc := packBits(zeros(90)); !bytes.Equal(enc, []byte{0xA7, 0x00}) {
		t.Fatalf("90 zeros % x", enc)
	}
}

// Brother: a compressed line may never be longer than the all-literal form (91 bytes for 90).
func TestPackBitsNeverGrows(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := []byte{0x00, 0xFF, 0x0F}
	for i := 0; i < 5000; i++ {
		n := 90
		if i%2 == 1 {
			n = 162
		}
		line := make([]byte, n)
		for j := range line {
			line[j] = alphabet[r.Intn(len(alphabet))]
		}
		enc := packBits(line)
		if len(enc) > literalSize(n) {
			t.Fatalf("%d bytes for %d: % x", len(enc), n, line)
		}
		back, err := unpackBits(enc, n)
		if err != nil || !bytes.Equal(back, line) {
			t.Fatalf("round trip: %v", err)
		}
	}
	if _, err := unpackBits([]byte{0x05, 1}, 6); err == nil {
		t.Fatal("short literal accepted")
	}
	if _, err := unpackBits([]byte{0xFE}, 3); err == nil {
		t.Fatal("repeat without data accepted")
	}
	if _, err := unpackBits([]byte{0x00, 1}, 2); err == nil {
		t.Fatal("wrong length accepted")
	}
}
