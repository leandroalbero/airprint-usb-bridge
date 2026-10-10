package ql

import (
	"strings"
	"testing"
)

func TestDecode_roundTripMentions(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	got, err := NewEncoder().Encode(p, Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(4, 2), NewBitmap(4, 2)}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := Decode(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"invalidate 200", "initialize", "print-info", "raster-lines 2", "print", "print-and-feed"} {
		if !strings.Contains(text, s) {
			t.Fatalf("missing %q in %s", s, text)
		}
	}
}

func TestDecode_garbageAndTruncation(t *testing.T) {
	text, err := Decode([]byte{0x01, 0x02, 0x1b, 0x69})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "garbage") || !strings.Contains(text, "truncated") {
		t.Fatal(text)
	}
	text, err = Decode(nil)
	if err != nil || text != "" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestDecode_statusRequestAndMode(t *testing.T) {
	text, err := Decode([]byte{0x1b, 0x69, 0x53, 0x1b, 0x69, 0x61, 0x01, 0x4d, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"status-request", "mode-switch 1", "compression 0x00"} {
		if !strings.Contains(text, s) {
			t.Fatal(text)
		}
	}
}
