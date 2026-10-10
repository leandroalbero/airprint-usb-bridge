package ql

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncode_62mmOnePageGolden(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	bm := NewBitmap(8, 2)
	bm.Set(0, 0, true)
	bm.Set(7, 0, true)
	opts := DefaultJobOptions()
	job := Job{Media: m, Options: opts, Pages: []*Bitmap{bm}}
	got, err := NewEncoder().Encode(p, job)
	if err != nil {
		t.Fatal(err)
	}
	want := goldenJob(p, job, false)
	if !bytes.Equal(got, want) {
		t.Fatalf("mismatch\n got %x\nwant %x", got, want)
	}
	enc := NewEncoder().(*RasterEncoder)
	if _, err := enc.Encode(p, job); err != nil {
		t.Fatal(err)
	}
	if len(enc.Warnings()) == 0 {
		t.Fatal("margin 0 and short page must warn")
	}
}

func TestEncode_62mmFullLineMatchesBrotherRef(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	bm := NewBitmap(m.PrintPins, 1)
	for x := 0; x < bm.W; x++ {
		bm.Set(x, 0, true)
	}
	got, err := NewEncoder().Encode(p, Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{bm}})
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(got, []byte{'g', 0x00, 0x5A})
	if idx < 0 {
		t.Fatal("missing raster command")
	}
	line := got[idx+3 : idx+93]
	want := make([]byte, 90)
	want[1] = 0x0F
	for i := 2; i <= 87; i++ {
		want[i] = 0xFF
	}
	want[88] = 0xF0
	if !bytes.Equal(line, want) {
		t.Fatalf("ref line\n got %x\nwant %x", line, want)
	}
}

func TestEncode_mirrorMovesPin(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	bm := NewBitmap(1, 1)
	bm.Set(0, 0, true)
	job := Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{bm}}

	ref, err := NewEncoder().Encode(p, job)
	if err != nil {
		t.Fatal(err)
	}
	refLine := rasterLine(t, ref)
	// pin 12 = byte 1 bit 3. Ref order, not compensated.
	if refLine[1] != 0x08 {
		t.Fatalf("ref pixel byte1=%02x", refLine[1])
	}

	enc := NewEncoder().(*RasterEncoder)
	enc.SetMirror(true)
	flipped, err := enc.Encode(p, job)
	if err != nil {
		t.Fatal(err)
	}
	flipLine := rasterLine(t, flipped)
	// pin 12 mirrored across 720 pins -> pin 707 -> byte 88 bit 4.
	if flipLine[88] != 0x10 || flipLine[1] != 0 {
		t.Fatalf("mirror line byte1=%02x byte88=%02x", flipLine[1], flipLine[88])
	}
}

func TestEncode_threePageOptionCombos(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	pages := []*Bitmap{NewBitmap(4, 3), NewBitmap(4, 3), NewBitmap(4, 3)}
	tests := []struct {
		name string
		opts JobOptions
		chk  func(t *testing.T, b []byte)
	}{
		{
			name: "defaults",
			opts: DefaultJobOptions(),
			chk: func(t *testing.T, b []byte) {
				want := goldenJob(p, Job{Media: m, Options: DefaultJobOptions(), Pages: pages}, false)
				if !bytes.Equal(b, want) {
					t.Fatalf("len %d vs %d", len(b), len(want))
				}
			},
		},
		{
			name: "chain",
			opts: JobOptions{AutoCut: false, CutAtEnd: false, ValidateMedia: true, LastTerminator: TermPrintAndFeed},
			chk: func(t *testing.T, b []byte) {
				if bytes.Contains(b, []byte{0x1b, 0x69, 0x41}) {
					t.Fatal("ESC i A sent without auto-cut")
				}
				if bytes.Contains(b, []byte{0x1b, 0x69, 0x4d, 0x40}) {
					t.Fatal("auto-cut bit set")
				}
				if bytes.Contains(b, []byte{0x1b, 0x69, 0x4b, 0x08}) {
					t.Fatal("cut-at-end set")
				}
			},
		},
		{
			name: "quality-highres",
			opts: JobOptions{AutoCut: true, CutEvery: 1, CutAtEnd: true, QualityPriority: true, HighRes: true, ValidateMedia: true, LastTerminator: TermPrintAndFeed},
			chk: func(t *testing.T, b []byte) {
				if !bytes.Contains(b, []byte{0x1b, 0x69, 0x7a, 0x8e | 0x40}) {
					t.Fatal("quality flag missing")
				}
				if !bytes.Contains(b, []byte{0x1b, 0x69, 0x4b, 0x48}) {
					t.Fatal("expanded mode")
				}
			},
		},
		{
			name: "no-validate-margin-1000-term-0c",
			opts: JobOptions{AutoCut: true, CutEvery: 3, CutAtEnd: true, MarginDots: 1000, ValidateMedia: false, LastTerminator: TermPrint},
			chk: func(t *testing.T, b []byte) {
				if !bytes.Contains(b, []byte{0x1b, 0x69, 0x7a, 0x80}) {
					t.Fatal("flags")
				}
				if bytes.Contains(b, []byte{0x1b, 0x69, 0x7a, 0x8e}) {
					t.Fatal("validation flags set")
				}
				if !bytes.Contains(b, []byte{0x1b, 0x69, 0x64, 0xe8, 0x03}) {
					t.Fatal("margin")
				}
				if !bytes.Contains(b, []byte{0x1b, 0x69, 0x41, 0x03}) {
					t.Fatal("cut every")
				}
				if b[len(b)-1] != 0x0c {
					t.Fatalf("last %02x", b[len(b)-1])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewEncoder().Encode(p, Job{Media: m, Options: tt.opts, Pages: pages})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(got, []byte{0x1b, 0x69, 0x61}) {
				t.Fatal("ESC i a")
			}
			if bytes.Count(got, []byte{0x1b, 0x40}) != 1 {
				t.Fatal("preamble count")
			}
			infos := printInfoPages(got)
			if len(infos) != 3 || infos[0] != 0 || infos[1] != 1 || infos[2] != 1 {
				t.Fatalf("page index %v", infos)
			}
			tt.chk(t, got)
		})
	}
}

func TestEncode_56dotMarginZeroUnchanged(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	bm := NewBitmap(8, 56)
	opts := DefaultJobOptions()
	got, err := NewEncoder().Encode(p, Job{Media: m, Options: opts, Pages: []*Bitmap{bm}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte{0x1b, 0x69, 0x64, 0x00, 0x00}) {
		t.Fatal("margin clamped")
	}
	if !bytes.Contains(got, []byte{0x1b, 0x69, 0x7a, 0x8e, 0x0a, 62, 0, 56, 0, 0, 0, 0, 0}) {
		t.Fatal("line count not 56")
	}
	if bytes.Count(got, []byte{'g', 0x00, 0x5A}) != 56 {
		t.Fatal("raster lines")
	}
}

func TestEncode_widerBitmap(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "29")
	_, err := NewEncoder().Encode(p, Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(m.PrintPins+1, 2)}})
	if !errors.Is(err, ErrBitmapWidth) {
		t.Fatal(err)
	}
}

func TestEncode_narrowLeftAligned(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "29")
	bm := NewBitmap(1, 1)
	bm.Set(0, 0, true)
	got, err := NewEncoder().Encode(p, Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{bm}})
	if err != nil {
		t.Fatal(err)
	}
	line := rasterLine(t, got)
	// pin 408, byte 51, bit 7.
	if line[51] != 0x80 {
		t.Fatalf("byte51=%02x", line[51])
	}
}
