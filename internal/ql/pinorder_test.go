package ql

import (
	"bytes"
	"testing"
)

// Cross-checked against a known-good QL-700 print stack on 2026-10-02: image column 0 lands at the
// far end of the raster line, so the shipped profile must mirror.
func TestQL700ProfilePinOrder(t *testing.T) {
	p, err := LoadProfile("profiles/ql700.json")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := p.MediaByID("62")
	bm := NewBitmap(696, 1)
	bm.Set(0, 0, true)
	b, err := NewEncoder().Encode(p, Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{bm}})
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte{'g', 0x00, 0x5A})
	if i < 0 {
		t.Fatal("no raster line")
	}
	line := b[i+3 : i+3+90]
	want := make([]byte, 90)
	want[88] = 0x10
	if !bytes.Equal(line, want) {
		t.Fatalf("pixel (0,0) encoded as %x, want %x", line, want)
	}
}
