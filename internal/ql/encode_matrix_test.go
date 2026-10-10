package ql

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQL700EncodeMatrix pins the QL-700 byte stream and its dry-run decode over every media and
// option combination. The golden was produced by the encoder before profiles drove it, so any
// profile-driven change that alters QL-700 output fails here.
func TestQL700EncodeMatrix(t *testing.T) {
	p, err := EmbeddedProfile()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, m := range p.Media {
		for _, mirror := range []bool{false, true} {
			for variant := 0; variant < 8; variant++ {
				o := DefaultJobOptions()
				o.HighRes = variant&1 != 0
				if variant&2 != 0 {
					o.AutoCut, o.CutAtEnd = false, false
				}
				if variant&4 != 0 {
					o.MarginDots, o.CutEvery, o.QualityPriority, o.ValidateMedia, o.LastTerminator = 35, 3, true, false, TermPrint
				}
				pages := 1 + variant%3
				j := Job{Media: m, Options: o}
				for pg := 0; pg < pages; pg++ {
					j.Pages = append(j.Pages, matrixBitmap(m.PrintPins-pg*7, 20+pg*13+variant, pg+variant))
				}
				enc := &RasterEncoder{}
				enc.SetMirror(mirror)
				b, err := enc.Encode(p, j)
				if err != nil {
					t.Fatalf("%s mirror=%v variant %d: %v", m.ID, mirror, variant, err)
				}
				text, _ := Decode(b)
				fmt.Fprintf(&out, "%s mirror=%v variant=%d len=%d bytes=%x decode=%x\n",
					m.ID, mirror, variant, len(b), sha256.Sum256(b), sha256.Sum256([]byte(text)))
			}
		}
	}
	path := filepath.Join("testdata", "ql700-encode-matrix.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("QL-700 bytes changed:\n%s", out.String())
	}
}

func matrixBitmap(w, h, seed int) *Bitmap {
	b := NewBitmap(w, h)
	s := uint32(seed*2654435761 + 1)
	for i := range b.Bits {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		b.Bits[i] = s%3 == 0
	}
	return b
}
