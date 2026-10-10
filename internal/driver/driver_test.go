package driver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func TestSelect(t *testing.T) {
	cases := []struct {
		id      string
		ippUSB  bool
		ppd     bool
		want    string
		model   string
		warn    bool
		wantErr error
	}{
		{id: "MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;", want: "brother-ql", model: "QL-700"},
		{id: "MFG:Brother;CMD:PT-CBP;MDL:QL-820NWB;CLS:PRINTER;", want: "brother-ql", model: "QL-820NWB"},
		{id: "MANUFACTURER:Brother;COMMAND SET:PJL,PCL,PCLXL;MODEL:HL-L2350DW series;", want: "pcl"},
		{id: "MFG:DYMO;CMD: ;MDL:LabelWriter 450 Turbo;CLS:PRINTER;", want: "dymo", model: "LabelWriter 450 Turbo"},
		{id: "MFG:DYMO;MDL:LabelWriter 550;", want: "dymo", warn: true},
		{id: "MFG:Zebra Technologies;CMD:ZPL;MDL:ZTC GK420d;CLS:PRINTER;", want: "zpl"},
		{id: "MFG:Generic;CMD:EPL,ZPL2;MDL:Thermal 4x6;", want: "zpl"},
		{id: "MFG:Zebra;CMD:EPL;MDL:LP2844;", wantErr: ErrNoDriver},
		{id: "MFG:Zebra;CMD:EPL2;MDL:LP2844;", ppd: true, want: "cups"},
		{id: "MFG:Zebra Technologies;MDL:ZD420;", want: "zpl"},
		{id: "MFG:Zebra;CMD:EPL,ZPL;MDL:GK420d;", want: "zpl"},
		{id: "MFG:DYMO;MDL:LabelWriter 5XL;", want: "dymo", warn: true},
		{id: "MFG:Hewlett-Packard;CMD:PJL,MLC,PML,PCLXL,POSTSCRIPT,PCL;MDL:HP LaserJet P3005;", want: "pcl"},
		{id: "MFG:HP;MDL:DeskJet 2700;CMD:PCL3GUI,PJL,URF;", want: "pcl", warn: true},
		{id: "MFG:Xerox;CMD:POSTSCRIPT3,PDF;MDL:Phaser;", want: "postscript"},
		{id: "MFG:Kyocera;CMD:PS;MDL:FS-1000;", want: "postscript"},
		{id: "MFG:Acme;CMD:PSX;MDL:Thing;", ppd: true, want: "cups"},
		{id: "MFG:EPSON;CMD:ESCPL2,BDC,D4;MDL:Stylus;", ppd: true, want: "cups"},
		{id: "MFG:EPSON;CMD:ESCPL2,BDC,D4;MDL:Stylus;", wantErr: ErrNoDriver},
		{id: "", wantErr: ErrNoDriver},
		{id: "MFG:HP;CMD:PCL,URF;MDL:LaserJet Pro M404;", ippUSB: true, want: "pcl", wantErr: ErrIPPOverUSB},
	}
	for _, c := range cases {
		sel, err := Select(ieee1284.Parse(c.id), c.ippUSB, c.ppd)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("%q: err %v, want %v", c.id, err, c.wantErr)
			}
		} else if err != nil {
			t.Errorf("%q: %v", c.id, err)
		}
		if sel.Driver != c.want {
			t.Errorf("%q: driver %q, want %q", c.id, sel.Driver, c.want)
		}
		if c.model != "" && sel.Model != c.model {
			t.Errorf("%q: model %q", c.id, sel.Model)
		}
		if (len(sel.Warnings) > 0) != c.warn {
			t.Errorf("%q: warnings %v", c.id, sel.Warnings)
		}
	}
	_, err := Select(ieee1284.Parse("MFG:EPSON;MDL:X;"), false, false)
	if err == nil || !strings.Contains(err.Error(), "--driver") || !strings.Contains(err.Error(), "MFG:EPSON") {
		t.Errorf("error should list the way out: %v", err)
	}
}

func a4Media(t *testing.T, ready bool) []Media {
	t.Helper()
	m, err := Sheets([]string{MediaA4, MediaLetter}, Margins{423, 423, 423, 423}, "stationery", "main")
	if err != nil {
		t.Fatal(err)
	}
	m[0].Ready = ready
	return m
}

func docCaps(t *testing.T) Capabilities {
	return Capabilities{
		MakeModel:   "Example Laser",
		Kind:        []string{"document"},
		Resolutions: []Resolution{{600, 600}},
		Qualities:   []int{QualityNormal},
		URF:         []string{"V1.4", "W8", "CP1", "RS600"},
		PWGTypes:    []string{"sgray_8"},
		Media:       a4Media(t, true),
	}
}

func rollCaps() Capabilities {
	return Capabilities{
		MakeModel:   "Example Label",
		Kind:        []string{"label", "roll"},
		Resolutions: []Resolution{{300, 300}},
		Qualities:   []int{QualityNormal},
		URF:         []string{"V1.4", "W8", "CP1", "RS300"},
		PWGTypes:    []string{"sgray_8"},
		Roll: &Roll{Width: 6200, MinLength: 800, MaxLength: 100000, DefaultLength: 10000,
			Margins: Margins{Left: 152, Right: 152}, Type: "roll", Source: "main"},
		Media: []Media{{Name: MediaName(6200, 2900), Width: 6200, Length: 2900, Type: "roll", Source: "main"}},
	}
}

func TestCapabilitiesValidate(t *testing.T) {
	good := docCaps(t)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if r := rollCaps(); r.Validate() != nil {
		t.Fatal(r.Validate())
	}
	for name, mut := range map[string]func(*Capabilities){
		"no model":  func(c *Capabilities) { c.MakeModel = "" },
		"no media":  func(c *Capabilities) { c.Media = nil },
		"quality":   func(c *Capabilities) { c.Qualities = []int{7} },
		"format":    func(c *Capabilities) { c.Formats = []string{"image/urf"} },
		"dup media": func(c *Capabilities) { c.Media[1].Name = c.Media[0].Name },
		"default":   func(c *Capabilities) { c.DefaultMedia = "iso_a3_297x420mm" },
		"margins":   func(c *Capabilities) { c.Media[0].Margins.Left = 21000 },
		"no urf":    func(c *Capabilities) { c.URF = nil },
		"roll range": func(c *Capabilities) {
			c.Roll = &Roll{Width: 100, MinLength: 9, MaxLength: 8, DefaultLength: 9, Type: "roll", Source: "main"}
		},
		"no resolutions": func(c *Capabilities) { c.Resolutions = nil },
	} {
		c := docCaps(t)
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMediaMatching(t *testing.T) {
	doc := docCaps(t)
	if m, ok := doc.MatchSize(21000, 29700); !ok || m.Name != MediaA4 {
		t.Errorf("A4: %+v %v", m, ok)
	}
	if m, ok := doc.MatchSize(21600, 27900); !ok || m.Name != MediaLetter {
		t.Errorf("Letter within 1 mm: %+v %v", m, ok)
	}
	if _, ok := doc.MatchSize(21000, 27940); ok {
		t.Error("A4 width with Letter length matched")
	}
	if _, ok := doc.MatchRange(21000, 800, 1000); ok {
		t.Error("range on a sheet printer")
	}
	if m, ok := doc.LookupName("na_letter_8.5x11in"); !ok || m.Width != 21590 || m.Length != 27940 {
		t.Errorf("letter name: %+v", m)
	}
	if _, ok := doc.LookupName("iso_a3_297x420mm"); ok {
		t.Error("A3 by name")
	}
	if d := doc.Default(); d.Name != MediaA4 {
		t.Errorf("default %+v", d)
	}
	doc.DefaultMedia = MediaLetter
	if d := doc.Default(); d.Name != MediaLetter {
		t.Errorf("default %+v", d)
	}
	if got := doc.ReadyNames(); !slices.Equal(got, []string{MediaA4}) {
		t.Errorf("ready %v", got)
	}

	roll := rollCaps()
	if m, ok := roll.MatchSize(6150, 1670); !ok || !m.Roll || m.Length != 1670 || m.Margins.Left != 152 {
		t.Errorf("roll length: %+v %v", m, ok)
	}
	if m, ok := roll.MatchSize(6200, 2900); !ok || !m.Roll {
		t.Errorf("preset: %+v %v", m, ok)
	}
	if _, ok := roll.MatchSize(6200, 700); ok {
		t.Error("too short")
	}
	if _, ok := roll.MatchSize(6301, 1000); ok {
		t.Error("too wide")
	}
	if m, ok := roll.MatchRange(6200, 800, 100000); !ok || m.Length != 0 {
		t.Errorf("range: %+v %v", m, ok)
	}
	if _, ok := roll.MatchRange(6200, 700, 1000); ok {
		t.Error("range below the roll")
	}
	for _, n := range []string{"roll_current_62x0mm", "roll_max_62x1000mm", "custom_min_62x8mm", "om_62x29mm_62x29mm", "oe_x_2.44x3in"} {
		if _, ok := roll.LookupName(n); !ok {
			t.Errorf("%s not found", n)
		}
	}
	if got := roll.MediaNames(); !slices.Equal(got, []string{"om_62x29mm_62x29mm", "roll_min_62x8mm", "roll_max_62x1000mm", "custom_min_62x8mm", "custom_max_62x1000mm"}) {
		t.Errorf("media names %v", got)
	}
	if got := roll.ReadyNames(); !slices.Equal(got, []string{"roll_current_62x0mm"}) {
		t.Errorf("ready %v", got)
	}
}

func TestParseMediaName(t *testing.T) {
	for name, want := range map[string][2]int{
		MediaA4: {21000, 29700}, MediaLetter: {21590, 27940}, MediaExecutive: {18415, 26670},
		Media4x6: {10160, 15240}, "om_62x29mm_62x29mm": {6200, 2900}, "custom_min_62.5x8mm": {6250, 800},
	} {
		x, y, ok := ParseMediaName(name)
		if !ok || x != want[0] || y != want[1] {
			t.Errorf("%s: %d %d %v", name, x, y, ok)
		}
	}
	if _, _, ok := ParseMediaName("a4"); ok {
		t.Error("a4 parsed")
	}
	if MMString(6250) != "62.5" || MMString(6200) != "62" || MMString(1) != "0.01" {
		t.Error("MMString")
	}
	if _, err := Sheets([]string{"letter"}, Margins{}, "stationery", "main"); err == nil {
		t.Error("bad sheet name accepted")
	}
}

func TestDocumentError(t *testing.T) {
	base := errors.New("page 2 is 3 inches wide")
	err := fmt.Errorf("render: %w", DocumentError(base))
	if !errors.Is(err, ErrDocument) || !errors.Is(err, base) || err.Error() != "render: page 2 is 3 inches wide" {
		t.Fatalf("%v", err)
	}
	if DocumentError(nil) != nil {
		t.Fatal("nil")
	}
	if !errors.Is(DocumentErrorf("x %d", 1), ErrDocument) {
		t.Fatal("Errorf")
	}
}

func TestRepeatUnits(t *testing.T) {
	u := []Unit{{Label: "a"}, {Label: "b"}}
	if got := RepeatUnits(u, 3); len(got) != 6 || got[2].Label != "a" || got[5].Label != "b" {
		t.Fatalf("%v", got)
	}
	if got := RepeatUnits(u, 0); len(got) != 2 {
		t.Fatal("copies 0")
	}
}

type fakeDriver struct{ caps Capabilities }

func (f fakeDriver) Name() string               { return "fake" }
func (f fakeDriver) Capabilities() Capabilities { return f.caps }
func (f fakeDriver) Render(context.Context, JobInfo, []raster.Page) ([]Unit, error) {
	return nil, nil
}

func TestRegistry(t *testing.T) {
	if _, done := Lookup("test-registry"); done {
		t.Skip("registry already populated by an earlier -count run")
	}
	var got Config
	Register(Info{
		Name:    "test-registry",
		Options: []Option{{Key: "darkness", Default: "15"}},
		New: func(c Config) (Driver, error) {
			got = c
			return fakeDriver{docCaps(t)}, nil
		},
	})
	Register(Info{Name: "test-broken", New: func(Config) (Driver, error) { return fakeDriver{}, nil }})
	if _, ok := Lookup("test-registry"); !ok || !slices.Contains(Names(), "test-registry") {
		t.Fatal("not registered")
	}
	d, err := New("test-registry", Config{Options: map[string]string{"darkness": "20"}})
	if err != nil || d.Name() != "fake" || got.Option("darkness", "15") != "20" {
		t.Fatalf("%v %v", d, err)
	}
	if n, err := got.IntOption("darkness", 0); err != nil || n != 20 {
		t.Fatalf("IntOption %d %v", n, err)
	}
	if b, err := (Config{}).BoolOption("x", true); err != nil || !b {
		t.Fatal("BoolOption default")
	}
	if _, err := New("test-registry", Config{Options: map[string]string{"speed": "1"}}); err == nil || !strings.Contains(err.Error(), "darkness") {
		t.Fatalf("unknown option: %v", err)
	}
	if _, err := New("test-broken", Config{}); err == nil {
		t.Fatal("invalid capabilities accepted")
	}
	if _, err := New("nope", Config{}); err == nil {
		t.Fatal("unknown driver")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	Register(Info{Name: "test-registry", New: func(Config) (Driver, error) { return nil, nil }})
}
