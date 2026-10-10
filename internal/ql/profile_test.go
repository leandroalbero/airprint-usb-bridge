package ql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testProfile(t *testing.T) *Profile {
	t.Helper()
	p, err := LoadProfile(filepath.Join("profiles", "ql700.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Packing tests reason in Brother ref pin order; TestQL700ProfilePinOrder covers the shipped mirror.
	p.Mirror = false
	return p
}

func mediaByID(t *testing.T, p *Profile, id string) Media {
	t.Helper()
	m, ok := p.MediaByID(id)
	if !ok {
		t.Fatalf("media %s", id)
	}
	return m
}

func TestLoadProfile_ql700(t *testing.T) {
	p, err := LoadProfile(filepath.Join("profiles", "ql700.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "QL-700" || p.Pins != 720 || p.BytesPerLine != 90 || p.DPI != 300 {
		t.Fatalf("%+v", p)
	}
	if p.SupportsModeCmd || p.SupportsCompress {
		t.Fatal("QL-700 must not enable ESC i a or compression")
	}
	if p.StatusModelByte3 != 0x34 || p.StatusModelByte4 != 0x35 {
		t.Fatalf("model bytes %02x %02x", p.StatusModelByte3, p.StatusModelByte4)
	}
	if _, ok := p.MediaByID("62"); !ok {
		t.Fatal("missing 62")
	}
	if !p.Mirror {
		t.Fatal("shipped QL-700 profile must mirror (verified against a known-good print stack)")
	}
}

func TestLoadProfile_rejectsBadPins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	body := []byte(`{
		"model":"x","pins":16,"bytes_per_line":2,"dpi":300,
		"continuous_margin_dots":{"value":[35,1500],"source":"documented"},
		"continuous_page_length_dots":{"value":[150,11811],"source":"documented"},
		"media":[{"id":"a","kind":10,"width_mm":62,"left_margin_pins":1,"print_pins":1,"right_margin_pins":1}]
	}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadProfile(path)
	if err == nil || !strings.Contains(err.Error(), "pin columns") {
		t.Fatal(err)
	}
}

func TestDetectMedia(t *testing.T) {
	p := testProfile(t)
	m, ok := p.DetectMedia(Status{MediaKind: MediaContinuous, MediaWidthMM: 62, MediaLengthMM: 0})
	if !ok || m.ID != "62" {
		t.Fatalf("%v %+v", ok, m)
	}
	die, ok := p.DetectMedia(Status{MediaKind: MediaDieCut, MediaWidthMM: 23, MediaLengthMM: 23})
	if !ok || die.ID != "23x23" {
		t.Fatalf("%v %+v", ok, die)
	}
	if _, ok := p.DetectMedia(Status{MediaKind: MediaDieCut, MediaWidthMM: 62, MediaLengthMM: 0}); ok {
		t.Fatal("expected unknown")
	}
	s := Status{MediaKind: MediaContinuous, MediaWidthMM: 17, MediaLengthMM: 3}
	if got := UnknownMedia(s); got != "unknown media 17×3 type 0x0A" {
		t.Fatal(got)
	}
}

func TestWarnings_marginZeroAndShortPage(t *testing.T) {
	p := testProfile(t)
	j := Job{
		Media:   mediaByID(t, p, "62"),
		Options: JobOptions{MarginDots: 0},
		Pages:   []*Bitmap{NewBitmap(8, 56)},
	}
	w := Warnings(p, j)
	if len(w) != 2 {
		t.Fatalf("%v", w)
	}
	j.Options.MarginDots = 35
	j.Pages = []*Bitmap{NewBitmap(8, 150)}
	if w := Warnings(p, j); len(w) != 0 {
		t.Fatal(w)
	}
	round := mediaByID(t, p, "d12")
	j = Job{Media: round, Options: JobOptions{MarginDots: 0}, Pages: []*Bitmap{NewBitmap(8, 94)}}
	if w := Warnings(p, j); len(w) != 1 || !strings.Contains(w[0], "fixed margin 35") {
		t.Fatal(w)
	}
}

func TestLengthMM(t *testing.T) {
	if got := LengthMM(300, 300, false); got != 25 {
		t.Fatal(got)
	}
	if got := LengthMM(300, 300, true); got != 13 {
		t.Fatal(got)
	}
}
