package ql

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeRaster_roundTripMirrorAndTrim(t *testing.T) {
	p, err := EmbeddedProfile()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := p.MediaByID("62")
	if !ok {
		t.Fatal("media")
	}
	m.XTrim = 3
	bm := NewBitmap(m.PrintPins, 2)
	bm.Set(0, 0, true)
	bm.Set(m.PrintPins-1, 0, true)
	bm.Set(10, 1, true)
	job := Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{bm}}
	raw, err := NewEncoder().Encode(p, job)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRaster(p, m, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].W != bm.W || got[0].H != bm.H {
		t.Fatalf("pages %+v", got)
	}
	if !bytes.Equal(bools(got[0]), bools(bm)) {
		t.Fatal("round trip lost pixels")
	}
}

func bools(b *Bitmap) []byte {
	out := make([]byte, len(b.Bits))
	for i, v := range b.Bits {
		if v {
			out[i] = 1
		}
	}
	return out
}

func TestEmbeddedProfile_equalsShippedFile(t *testing.T) {
	p, err := EmbeddedProfile()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("profiles", "ql700.json")
	file, err := LoadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != file.Model || p.Mirror != file.Mirror || p.Pins != file.Pins || len(p.Media) != len(file.Media) {
		t.Fatalf("embedded %+v file %+v", p.Model, file.Model)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, ql700JSON) {
		t.Fatal("embedded bytes differ from profiles/ql700.json")
	}
	if !p.Mirror {
		t.Fatal("shipped profile must mirror")
	}
}

func TestDetectEditorLite(t *testing.T) {
	root := t.TempDir()
	if DetectEditorLite(root) {
		t.Fatal("missing sysfs must not be editor lite")
	}
	dev := filepath.Join(root, "bus", "usb", "devices", "1-1")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "idVendor"), []byte("04f9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "idProduct"), []byte("2049\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !DetectEditorLite(root) {
		t.Fatal("want editor lite")
	}
	if err := os.WriteFile(filepath.Join(dev, "idProduct"), []byte("2042\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if DetectEditorLite(root) {
		t.Fatal("normal product must not match")
	}
}
