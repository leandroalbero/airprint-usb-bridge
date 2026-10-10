package ql

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// Expected values are copied from Brother's raster command references (USB appendix, status
// bytes 3 and 4, the command support notes and the raster line tables), not from the JSON.
var refModels = []struct {
	stem           string
	model          string
	pid            uint16
	b3, b4         byte
	pins           int
	invalidate     int
	modeCmd        bool
	compress       bool
	noCutter       bool
	noCutEvery     bool
	noExpanded     bool
	noHighRes      bool
	kindBit        byte
	twoColor       bool
	pageLo, pageHi int
}{
	{"ql500", "QL-500", 0x2015, '0', 'O', 720, 0, false, false, true, true, true, true, 0, false, 295, 11811},
	{"ql550", "QL-550", 0x2016, '0', 'O', 720, 0, false, false, false, true, true, true, 0, false, 295, 11811},
	{"ql560", "QL-560", 0x2027, '4', '1', 720, 0, false, false, false, true, true, true, 0, false, 295, 11811},
	{"ql570", "QL-570", 0x2028, '4', '2', 720, 0, false, false, false, false, false, false, 0, false, 150, 11811},
	{"ql580n", "QL-580N", 0x2029, '4', '3', 720, 0, true, true, false, false, false, false, 0, false, 150, 11811},
	{"ql650td", "QL-650TD", 0x201B, '0', 'Q', 720, 0, true, false, false, true, false, true, 0, false, 295, 11811},
	{"ql700", "QL-700", 0x2042, '4', '5', 720, 0, false, false, false, false, false, false, 0, false, 150, 11811},
	{"ql710w", "QL-710W", 0x2043, '4', '6', 720, 0, true, true, false, false, false, false, 0x40, false, 150, 11811},
	{"ql720nw", "QL-720NW", 0x2044, '4', '7', 720, 0, true, true, false, false, false, false, 0x40, false, 150, 11811},
	{"ql800", "QL-800", 0x209B, '4', '8', 720, 400, true, false, false, false, false, false, 0x40, true, 150, 11811},
	{"ql810w", "QL-810W", 0x209C, '4', '9', 720, 400, true, true, false, false, false, false, 0x40, true, 150, 11811},
	{"ql820nwb", "QL-820NWB", 0x209D, '4', 'A', 720, 400, true, true, false, false, false, false, 0x40, true, 150, 11811},
	{"ql1050", "QL-1050", 0x2020, '0', 'P', 1296, 350, true, true, false, false, false, true, 0, false, 295, 35433},
	{"ql1060n", "QL-1060N", 0x202A, '4', '4', 1296, 350, true, true, false, false, false, true, 0, false, 295, 35433},
	{"ql1100", "QL-1100", 0x20A7, '4', 'C', 1296, 350, true, true, false, false, false, true, 0, false, 301, 35434},
	{"ql1110nwb", "QL-1110NWB", 0x20A8, '4', 'D', 1296, 350, true, true, false, false, false, true, 0, false, 301, 35434},
}

func TestEmbeddedProfilesMatchReferences(t *testing.T) {
	var stems []string
	for _, r := range refModels {
		stems = append(stems, r.stem)
	}
	if got := EmbeddedProfileNames(); !slices.Equal(got, stems) {
		t.Fatalf("embedded %v\nwant     %v", got, stems)
	}
	for _, r := range refModels {
		p, ok, err := EmbeddedProfileByName(r.stem)
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", r.stem, ok, err)
		}
		if p.Model != r.model || ModelKey(p.Model) != r.stem {
			t.Errorf("%s: model %q", r.stem, p.Model)
		}
		if p.Verified == nil || *p.Verified != (r.model == "QL-700") || p.IsVerified() != *p.Verified {
			t.Errorf("%s: verified %v; only the QL-700 is verified", r.model, p.Verified)
		}
		if p.USBVendor != BrotherVendorID || p.USBProduct != r.pid {
			t.Errorf("%s: usb %04x:%04x, want 04f9:%04x", r.model, p.USBVendor, p.USBProduct, r.pid)
		}
		if p.StatusModelByte3 != r.b3 || p.StatusModelByte4 != r.b4 {
			t.Errorf("%s: status model bytes %02x %02x, want %02x %02x", r.model, p.StatusModelByte3, p.StatusModelByte4, r.b3, r.b4)
		}
		if p.Pins != r.pins || p.BytesPerLine != r.pins/8 || p.DPI != 300 || !p.Mirror {
			t.Errorf("%s: pins %d bytes %d dpi %d mirror %v", r.model, p.Pins, p.BytesPerLine, p.DPI, p.Mirror)
		}
		got := []bool{p.SupportsModeCmd, p.SupportsCompress, p.NoAutoCut, p.NoCutEvery, p.NoExpandedMode, p.NoHighRes, p.TwoColor}
		want := []bool{r.modeCmd, r.compress, r.noCutter, r.noCutEvery, r.noExpanded, r.noHighRes, r.twoColor}
		if !slices.Equal(got, want) {
			t.Errorf("%s: esc-i-a, compress, no-cutter, no-cut-every, no-expanded, no-high-res, two-colour = %v, want %v", r.model, got, want)
		}
		if p.InvalidateBytes != r.invalidate || p.StatusMediaKindBit != r.kindBit {
			t.Errorf("%s: invalidate %d kind bit %02x", r.model, p.InvalidateBytes, p.StatusMediaKindBit)
		}
		if p.PageLengthDots.V != [2]int{r.pageLo, r.pageHi} || p.MarginDots.V != [2]int{35, 1500} {
			t.Errorf("%s: page length %v margin %v", r.model, p.PageLengthDots.V, p.MarginDots.V)
		}
		if r.model != "QL-700" && (p.Reference == "" || len(p.Notes) == 0 || !strings.HasPrefix(p.Notes[0], "Unverified")) {
			t.Errorf("%s: reference %q notes %q", r.model, p.Reference, p.Notes)
		}
		if r.twoColor && !strings.Contains(strings.Join(p.Notes, " "), "Two-colour (red/black) printing is not supported") {
			t.Errorf("%s: two-colour limitation not documented in notes", r.model)
		}
		for _, id := range []string{"12", "29", "62"} {
			if m, ok := p.MediaByID(id); !ok || m.Kind != MediaContinuous {
				t.Errorf("%s: no continuous %s", r.model, id)
			}
		}
	}
}

// The QL-700's media entries are the verified ones; every 720-pin profile must lay out the
// same labels on the same pins.
func TestProfilesShareQL700MediaGeometry(t *testing.T) {
	ref, err := EmbeddedProfile()
	if err != nil {
		t.Fatal(err)
	}
	geometry := func(m Media) Media { m.Name, m.FixedMargin = "", 0; return m }
	for _, stem := range EmbeddedProfileNames() {
		p, _, err := EmbeddedProfileByName(stem)
		if err != nil {
			t.Fatal(err)
		}
		if p.Pins != 720 {
			continue
		}
		for _, want := range ref.Media {
			got, ok := p.MediaByID(want.ID)
			if !ok || geometry(got) != geometry(want) {
				t.Errorf("%s media %s: %+v, want %+v", p.Model, want.ID, got, want)
			}
		}
	}
}

// Rows from the 1296-pin tables, checked by hand: left + print + right = 1296.
func TestWideHeadMediaTables(t *testing.T) {
	for stem, rows := range map[string]map[string][3]int{
		"ql1050": {"62": {544, 696, 56}, "102": {76, 1164, 56}, "12": {1116, 106, 74}, "23x23": {976, 236, 84}, "102x152": {76, 1164, 56}},
		"ql1100": {"62": {544, 696, 56}, "103": {58, 1200, 38}, "60x86": {556, 672, 68}, "23x23": {975, 236, 85}, "103x164": {58, 1200, 38}},
	} {
		p, _, err := EmbeddedProfileByName(stem)
		if err != nil {
			t.Fatal(err)
		}
		for id, want := range rows {
			m, ok := p.MediaByID(id)
			if !ok || [3]int{m.LeftPins, m.PrintPins, m.RightPins} != want {
				t.Errorf("%s %s: %+v, want %v", stem, id, m, want)
			}
		}
	}
	p, _, _ := EmbeddedProfileByName("ql1100")
	if m, _ := p.MediaByID("103"); m.WidthMM != 104 {
		t.Errorf("103 mm tape reports width %d; the reference's status and ESC i z use 104", m.WidthMM)
	}
}

func TestProfileLookup(t *testing.T) {
	for in, want := range map[string]string{
		"QL-820NWB":          "QL-820NWB",
		"Brother QL-1110NWB": "QL-1110NWB",
		"QL-820NWBc":         "QL-820NWB",
		"ql700":              "QL-700",
		" ql-580n ":          "QL-580N",
	} {
		p, ok, err := ProfileForModel(in)
		if err != nil || !ok || p.Model != want {
			t.Errorf("ProfileForModel(%q) = %v %v %v", in, p, ok, err)
		}
	}
	for _, in := range []string{"", "QL-9999", "PT-P700", "c"} {
		if _, ok, err := ProfileForModel(in); ok || err != nil {
			t.Errorf("ProfileForModel(%q) = %v %v", in, ok, err)
		}
	}
	for pid, want := range map[uint16]string{0x2042: "QL-700", 0x2049: "QL-700", 0x209D: "QL-820NWB", 0x20A7: "QL-1100", 0x2015: "QL-500"} {
		p, ok, err := ProfileForUSB(BrotherVendorID, pid)
		if err != nil || !ok || p.Model != want {
			t.Errorf("ProfileForUSB(%04x) = %v %v %v", pid, p, ok, err)
		}
	}
	for _, id := range [][2]uint16{{0x04F9, 0x9999}, {0x0922, 0x2042}, {0x04F9, 0}} {
		if _, ok, _ := ProfileForUSB(id[0], id[1]); ok {
			t.Errorf("ProfileForUSB(%04x:%04x) matched", id[0], id[1])
		}
	}
	if m := EmbeddedModels(); len(m) != len(refModels) || m[0] != "QL-500" || m[len(m)-1] != "QL-1110NWB" {
		t.Errorf("models %v", m)
	}
}

func TestIsVerifiedWithoutKey(t *testing.T) {
	p, _ := EmbeddedProfile()
	p.Verified = nil
	if !p.IsVerified() {
		t.Fatal("a QL-700 profile without the key must stay verified")
	}
	p.Model = "QL-710W"
	if p.IsVerified() {
		t.Fatal("other models without the key are unverified")
	}
}

func TestStatusMediaKindBit(t *testing.T) {
	raw := make([]byte, 32)
	raw[0], raw[1], raw[2], raw[3], raw[4] = 0x80, 0x20, 0x42, '4', 'A'
	raw[10], raw[11] = 62, 0x4A
	p, _, _ := EmbeddedProfileByName("ql820nwb")
	s, err := ParseStatus(p, raw)
	if err != nil || s.MediaKind != MediaContinuous || s.Raw[11] != 0x4A {
		t.Fatalf("%+v %v", s, err)
	}
	if m, ok := p.DetectMedia(s); !ok || m.ID != "62" {
		t.Fatalf("detect %v %+v", ok, m)
	}
	ql700, _ := EmbeddedProfile()
	if s, _ := ParseStatus(ql700, raw); s.MediaKind != 0x4A {
		t.Fatalf("QL-700 status kind changed: %02x", byte(s.MediaKind))
	}
}

func TestInvalidatePerProfile(t *testing.T) {
	ql700, _ := EmbeddedProfile()
	if got := Invalidate(ql700); len(got) != 202 || !bytes.Equal(got[200:], []byte{0x1b, 0x40}) || bytes.ContainsFunc(got[:200], func(r rune) bool { return r != 0 }) {
		t.Fatalf("QL-700 invalidate %d bytes", len(got))
	}
	for stem, n := range map[string]int{"ql800": 400, "ql1100": 350, "ql710w": 200} {
		p, _, _ := EmbeddedProfileByName(stem)
		if got := Invalidate(p); len(got) != n+2 || got[n] != 0x1b || got[n+1] != 0x40 {
			t.Errorf("%s: %d bytes", stem, len(got))
		}
	}
	bad, _ := EmbeddedProfile()
	bad.InvalidateBytes = 10
	if err := bad.validate(); err == nil {
		t.Fatal("10 invalidate bytes accepted")
	}
}
