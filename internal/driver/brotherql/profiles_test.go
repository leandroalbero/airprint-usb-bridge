package brotherql

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func builtin(t *testing.T, name string) *ql.Profile {
	t.Helper()
	p, err := LoadProfile(name, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadProfileFor(t *testing.T) {
	file := filepath.Join(t.TempDir(), "custom.json")
	raw, err := os.ReadFile(filepath.Join("..", "..", "ql", "profiles", "ql710w.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, bytes.Replace(raw, []byte(`"QL-710W"`), []byte(`"QL-710W custom"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, model string
		vid, pid    uint16
		want        string
	}{
		{"", "", 0, 0, "QL-700"},
		{"ql700", "", 0, 0, "QL-700"},
		{"QL-820NWB", "QL-700", 0, 0, "QL-820NWB"},
		{"ql1100", "", 0, 0, "QL-1100"},
		{file, "QL-820NWB", 0, 0, "QL-710W custom"},
		{"", "QL-820NWB", 0, 0, "QL-820NWB"},
		{"", "QL-1110NWBc", 0x04F9, 0x2042, "QL-1110NWB"},
		{"", "", 0x04F9, 0x209C, "QL-810W"},
		{"", "", 0x04F9, 0x9999, "QL-700"},
		{"", "QL-820NWB-X", 0x04F9, 0x20A8, "QL-1110NWB"},
	} {
		p, err := LoadProfileFor(c.name, c.model, c.vid, c.pid)
		if err != nil || p.Model != c.want {
			t.Errorf("LoadProfileFor(%q, %q, %04x:%04x) = %v, %v; want %s", c.name, c.model, c.vid, c.pid, p, err, c.want)
		}
	}
	if _, err := LoadProfile("", "QL-9999"); err == nil || !strings.Contains(err.Error(), "QL-820NWB") {
		t.Errorf("unknown model: %v", err)
	}
	if _, err := LoadProfileFor("", "QL-9999", 0x04F9, 0x9999); err == nil {
		t.Error("unknown model and product accepted")
	}
	if _, err := LoadProfile(filepath.Join(t.TempDir(), "missing.json"), ""); err == nil {
		t.Error("missing file accepted")
	}
}

func TestNewFromConfigByModel(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	d, err := NewFromConfig(driver.Config{DeviceID: ieee1284.Parse("MFG:Brother;CMD:PT-CBP;MDL:QL-820NWB;"), HighRes: true, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	c := d.Capabilities()
	if d.Profile().Model != "QL-820NWB" || c.MakeModel != "Brother QL-820NWB" || c.Verified || !d.opts.HighRes {
		t.Fatalf("%s %+v", d.Profile().Model, c)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "two-colour") || !strings.Contains(joined, "unverified") {
		t.Fatalf("logs %q", logs)
	}

	logs = nil
	d, err = NewFromConfig(driver.Config{DeviceID: ieee1284.Parse("MFG:Brother;MDL:QL-1100;"), HighRes: true, Media: "102", Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if d.opts.HighRes || !strings.Contains(strings.Join(logs, "\n"), "--high-res ignored") {
		t.Fatalf("high-res %v logs %q", d.opts.HighRes, logs)
	}
	if c := d.Capabilities(); c.Roll.Width != 10200 || c.Roll.MaxLength != 300000 || c.URF[len(c.URF)-1] != "RS300" {
		t.Fatalf("caps %+v roll %+v", c, c.Roll)
	}

	if _, err := NewFromConfig(driver.Config{Options: map[string]string{"profile": "ql800", "compress": "true"}}); err == nil || !strings.Contains(err.Error(), "compress") {
		t.Fatalf("QL-800 compress: %v", err)
	}
	if _, err := NewFromConfig(driver.Config{Options: map[string]string{"compress": "maybe"}}); err == nil {
		t.Fatal("bad compress value accepted")
	}
	if _, err := New(Options{Profile: builtin(t, "ql1050"), Media: "62", HighRes: true}); err == nil {
		t.Fatal("high-res accepted on a QL-1050")
	}
	ql700, err := NewFromConfig(driver.Config{Logf: logf})
	if err != nil || !ql700.Capabilities().Verified || ql700.Capabilities().Roll.MaxLength != 100000 {
		t.Fatalf("QL-700: %v", err)
	}
}

// One label through the whole driver on unverified profiles: capabilities pass the harness
// checks, the bytes are the encoder's, and the head width, compression and cutter follow the
// profile.
func TestRenderOtherModels(t *testing.T) {
	for _, c := range []struct {
		profile, media string
		compress       bool
		width          int // dots across at 300 dpi
		lineBytes      byte
	}{
		{"ql820nwb", "62", false, 732, 90},
		{"ql810w", "29", true, 342, 0},
		{"ql1100", "102", true, 1205, 0},
		{"ql1060n", "62", false, 732, 162},
		{"ql500", "38", false, 449, 90},
	} {
		t.Run(c.profile, func(t *testing.T) {
			d, err := New(Options{Profile: builtin(t, c.profile), Media: c.media, Compress: c.compress, TrimMax: -1})
			if err != nil {
				t.Fatal(err)
			}
			job := drivertest.Job(t, d.Capabilities())
			page := grayRect(c.width, 60, c.width/4, 10, c.width/2, 30)
			res := drivertest.Run(t, d, job, page)
			if len(res.Units) != 1 {
				t.Fatalf("units %d", len(res.Units))
			}
			qj := res.Units[0].Meta.(ql.Job)
			want, err := ql.NewEncoder().Encode(d.prof, qj)
			if err != nil || !bytes.Equal(res.Units[0].Data, want) {
				t.Fatalf("data is not the encoded job: %v", err)
			}
			if qj.Options.Compress != c.compress || bytes.Contains(res.Data, []byte{'M', 0x02}) != c.compress {
				t.Fatalf("compression %v", qj.Options.Compress)
			}
			if c.lineBytes != 0 && !bytes.Contains(res.Data, []byte{'g', 0x00, c.lineBytes}) {
				t.Fatalf("no %d-byte raster lines", c.lineBytes)
			}
			if d.prof.NoAutoCut != !qj.Options.AutoCut {
				t.Fatalf("auto-cut %v on %s", qj.Options.AutoCut, d.prof.Model)
			}
			pages, err := ql.DecodeRaster(d.prof, d.media, res.Data)
			if err != nil || len(pages) != 1 || pages[0].H != 20 {
				t.Fatalf("decode %v", err)
			}
			if d.Capabilities().Verified || !strings.Contains(res.Inspect[0], "raster-lines 20") {
				t.Fatalf("verified %v inspect %q", d.Capabilities().Verified, res.Inspect[0])
			}
		})
	}
}

func TestAttributesGoldenQL1100(t *testing.T) {
	d, err := New(Options{Profile: builtin(t, "ql1100"), Media: "102", Presets: []int{5100, 15200}, TrimMax: -1})
	if err != nil {
		t.Fatal(err)
	}
	attrs, txt := drivertest.Attributes(t, d.Capabilities())
	drivertest.Golden(t, "attributes-ql1100-102.golden", []byte(attrs))
	if txt["ty"] != "Brother QL-1100" || txt["URF"] != "V1.4,W8,CP1,RS300" {
		t.Errorf("TXT %v", txt)
	}
	units, err := d.Render(context.Background(), drivertest.Job(t, d.Capabilities()), []raster.Page{grayRect(732, 20, 0, 0, 10, 10)})
	if len(units) != 0 || err == nil {
		t.Fatalf("62 mm page on 102 mm roll: %v", err)
	}
}

func TestJobForWithoutCutter(t *testing.T) {
	p := builtin(t, "ql500")
	m, _ := p.MediaByID("62")
	j := JobFor(p, m, ql.NewBitmap(10, 10), false)
	if j.Options.AutoCut || j.Options.CutAtEnd {
		t.Fatalf("%+v", j.Options)
	}
	b, err := ql.NewEncoder().Encode(p, j)
	if err != nil || !bytes.Contains(b, []byte{0x1b, 0x69, 0x4d, 0x00}) || bytes.Contains(b, []byte{0x1b, 0x69, 0x41}) {
		t.Fatalf("%v % x", err, b[200:230])
	}
}
