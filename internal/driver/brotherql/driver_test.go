package brotherql

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func newDriver(t *testing.T, mod func(*Options)) *Driver {
	t.Helper()
	o := Options{Profile: profile(t), Media: "62", Presets: []int{2900, 5000}, TrimMax: -1}
	if mod != nil {
		mod(&o)
	}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The 62 mm capabilities are the attribute set iOS was verified against
// (testdata/private/get-printer-attributes-live.txt, checked in internal/ipp).
func TestCapabilities62(t *testing.T) {
	roll := &driver.Roll{Width: 6200, MinLength: 800, MaxLength: 100000, DefaultLength: 10000,
		Margins: driver.Margins{Left: 152, Right: 152}, Type: "roll", Source: "main"}
	preset := func(l int, ready bool) driver.Media {
		return driver.Media{Name: driver.MediaName(6200, l), Width: 6200, Length: l, Margins: roll.Margins,
			Type: "roll", Source: "main", Ready: ready}
	}
	want := driver.Capabilities{
		MakeModel:   "Brother QL-700",
		Kind:        []string{"label", "roll"},
		Resolutions: []driver.Resolution{{X: 300, Y: 300}},
		Qualities:   []int{4},
		URF:         []string{"V1.4", "W8", "CP1", "RS300"},
		PWGTypes:    []string{"sgray_8"},
		MaxCopies:   1,
		Roll:        roll,
		Media:       []driver.Media{preset(2900, false), preset(5000, false)},
		Verified:    true,
	}
	if got := newDriver(t, nil).Capabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	hr := newDriver(t, func(o *Options) { o.HighRes = true; o.MediaPolicy = PolicyRollPresets }).Capabilities()
	if !reflect.DeepEqual(hr.URF, []string{"V1.4", "W8", "CP1", "PQ4-5", "RS300-600"}) ||
		!reflect.DeepEqual(hr.Qualities, []int{4, 5}) ||
		!reflect.DeepEqual(hr.Resolutions, []driver.Resolution{{X: 300, Y: 300}, {X: 600, Y: 600}}) ||
		!reflect.DeepEqual(hr.AlsoAccept, []driver.Resolution{{X: 300, Y: 600}}) {
		t.Fatalf("high-res %+v", hr)
	}
	if !hr.Media[0].Ready || !hr.Media[1].Ready {
		t.Fatal("roll+presets did not ready the presets")
	}

	narrow := newDriver(t, func(o *Options) { o.Media = "29"; o.Presets = nil }).Capabilities()
	// 29 mm prints 306 pins: (2900 - 306*2540/300) / 2 = 155.
	if narrow.Roll.Width != 2900 || narrow.Roll.Margins.Left != 155 || len(narrow.Media) != 0 {
		t.Fatalf("29 mm %+v", narrow.Roll)
	}
}

func TestAttributesGolden(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"attributes-62.golden":         nil,
		"attributes-62-highres.golden": func(o *Options) { o.HighRes = true; o.MediaPolicy = PolicyRollPresets },
	} {
		attrs, txt := drivertest.Attributes(t, newDriver(t, mod).Capabilities())
		drivertest.Golden(t, name, []byte(attrs))
		if name == "attributes-62.golden" && (txt["URF"] != "V1.4,W8,CP1,RS300" || txt["pdl"] != "image/pwg-raster,image/urf") {
			t.Errorf("TXT %v", txt)
		}
	}
}

func grayRect(w, h int, x0, y0, x1, y1 int) raster.Page {
	return drivertest.Page(w, h, 300, func(x, y int) byte {
		if x >= x0 && x < x1 && y >= y0 && y < y1 {
			return 0
		}
		return 255
	})
}

func TestRender(t *testing.T) {
	d := newDriver(t, nil)
	var debug []string
	job := drivertest.Job(t, d.Capabilities())
	job.Debug = func(name string, img image.Image) { debug = append(debug, name) }
	pages := []raster.Page{
		grayRect(732, 100, 100, 10, 400, 40),
		drivertest.Page(732, 50, 300, nil),
		grayRect(732, 80, 18, 5, 714, 6),
	}
	res := drivertest.Run(t, d, job, pages...)
	if len(res.Units) != 2 || !slices.Equal(debug, []string{"p1-label", "p3-label"}) {
		t.Fatalf("units %d debug %v", len(res.Units), debug)
	}
	for i, u := range res.Units {
		qj, ok := u.Meta.(ql.Job)
		if !ok {
			t.Fatalf("unit %d meta %T", i, u.Meta)
		}
		want, err := ql.NewEncoder().Encode(d.prof, qj)
		if err != nil || !bytes.Equal(u.Data, want) {
			t.Fatalf("unit %d data is not the encoded job: %v", i, err)
		}
	}
	if res.Units[0].Label != "label 1 of 2 (page 1)" || res.Units[1].Label != "label 2 of 2 (page 3)" {
		t.Fatalf("labels %q %q", res.Units[0].Label, res.Units[1].Label)
	}
	if h := res.Units[0].Meta.(ql.Job).Pages[0].H; h != 30 {
		t.Fatalf("label 1 is %d rows, want 30 after trimming", h)
	}
	if !strings.Contains(res.Inspect[0], "raster-lines 30") {
		t.Fatalf("inspect %q", res.Inspect[0])
	}
	if pages[0].Gray != nil {
		t.Fatal("page gray kept after conversion")
	}

	wide := []raster.Page{grayRect(732, 20, 0, 0, 10, 10), grayRect(800, 20, 0, 0, 10, 10)}
	units, err := d.Render(context.Background(), job, wide)
	if len(units) != 0 || !errors.Is(err, driver.ErrDocument) || !errors.Is(err, raster.ErrWidth) || !strings.HasPrefix(err.Error(), "page 2: ") {
		t.Fatalf("wrong width: %d units, %v", len(units), err)
	}
	if lim := d.PageLimits(job); lim.WidthMin != 724 || lim.WidthMax != 740 {
		t.Fatalf("limits %+v", lim)
	}
}

func TestRenderHighResJob(t *testing.T) {
	d := newDriver(t, func(o *Options) { o.HighRes = true })
	job := drivertest.Job(t, d.Capabilities())
	p600 := drivertest.Page(1464, 40, 600, func(x, y int) byte {
		if y >= 10 && y < 30 {
			return 0
		}
		return 255
	})
	for q, want := range map[int]bool{driver.QualityNormal: false, driver.QualityHigh: true} {
		job.Quality = q
		p300 := grayRect(732, 20, 100, 5, 200, 10)
		units, err := d.Render(context.Background(), job, []raster.Page{p300, p600})
		if err != nil {
			t.Fatal(err)
		}
		for i, u := range units {
			if u.Meta.(ql.Job).Options.HighRes != want {
				t.Errorf("quality %d unit %d high-res %v", q, i, !want)
			}
		}
	}
}

func TestRenderImage(t *testing.T) {
	d := newDriver(t, nil)
	img := image.NewGray(image.Rect(0, 0, 600, 40))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	img.SetGray(3, 4, color.Gray{Y: 0x7f})
	units, err := d.RenderImage(context.Background(), drivertest.Job(t, d.Capabilities()), img)
	if err != nil || len(units) != 1 || units[0].Label != "600x40" {
		t.Fatalf("%v %v", units, err)
	}
	qj := units[0].Meta.(ql.Job)
	if b := qj.Pages[0]; b.W != 696 || b.H != 40 || !b.At(3, 4) || b.At(4, 4) {
		t.Fatalf("bitmap %dx%d", b.W, b.H)
	}
	if _, err := d.RenderImage(context.Background(), driver.JobInfo{}, image.NewGray(image.Rect(0, 0, 697, 2))); !errors.Is(err, driver.ErrDocument) {
		t.Fatalf("too wide: %v", err)
	}
}

func TestNewFromConfig(t *testing.T) {
	d, err := NewFromConfig(driver.Config{Options: map[string]string{"presets": "29, 62.5mm", "trim": "24", "media-policy": "roll+presets"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.media.ID != "62" || d.ropts.TrimMax != 24 || len(d.caps.Media) != 2 || d.caps.Media[1].Length != 6250 || !d.caps.Media[0].Ready {
		t.Fatalf("%+v %+v", d.ropts, d.caps.Media)
	}
	for name, c := range map[string]driver.Config{
		"trim":    {Options: map[string]string{"trim": "x"}},
		"presets": {Options: map[string]string{"presets": "29,abc"}},
		"policy":  {Options: map[string]string{"media-policy": "sheets"}},
		"media":   {Media: "23x23"},
		"nomedia": {Media: "99"},
		"dither":  {Dither: "sometimes"},
		"model":   {DeviceID: ieee1284.Parse("MFG:Brother;MDL:QL-9999;")},
	} {
		if _, err := NewFromConfig(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewFromConfig(driver.Config{DeviceID: ieee1284.Parse("MFG:Brother;MDL:QL-700;")}); err != nil {
		t.Errorf("QL-700 by model: %v", err)
	}
	if _, err := NewFromConfig(driver.Config{Media: "23x23"}); err == nil || !strings.Contains(err.Error(), "continuous") {
		t.Errorf("die-cut: %v", err)
	}
	info, ok := driver.Lookup(Name)
	if !ok || info.Verified == "" {
		t.Fatal("not registered")
	}
	if _, err := driver.New(Name, driver.Config{Options: map[string]string{"trim": "0"}}); err != nil {
		t.Fatal(err)
	}
}

func TestParseValues(t *testing.T) {
	for in, want := range map[string]int{"max": -1, "unlimited": -1, "-1": -1, "0": 0, "24": 24} {
		if got, err := ParseTrim(in); err != nil || got != want {
			t.Errorf("ParseTrim(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParseTrim("-3"); err == nil {
		t.Error("ParseTrim(-3) accepted")
	}
	got, err := ParsePresets("29, 50mm,62.5")
	if err != nil || !slices.Equal(got, []int{2900, 5000, 6250}) {
		t.Errorf("ParsePresets = %v, %v", got, err)
	}
	if got, err := ParsePresets(""); err != nil || len(got) != 0 {
		t.Errorf("empty presets = %v, %v", got, err)
	}
	if _, err := ParsePresets("29,abc"); err == nil {
		t.Error("bad preset accepted")
	}
}

func TestNewSender(t *testing.T) {
	d := newDriver(t, nil)
	for kind, generic := range map[backend.TargetKind]bool{backend.TargetDryRun: true, backend.TargetUSB: false, backend.TargetTCP: false} {
		s, err := d.NewSender(backend.Target{Kind: kind, Device: "auto", Addr: "127.0.0.1:9", Dir: t.TempDir()})
		if err != nil || (s == nil) != generic {
			t.Errorf("%s: %v %v", kind, s, err)
		}
	}
	s, _ := d.NewSender(backend.Target{Kind: backend.TargetTCP, Addr: "127.0.0.1:9"})
	if err := s.Send(context.Background(), driver.Unit{Label: "x", Data: []byte{1}}); err == nil || !strings.Contains(err.Error(), "ql.Job") {
		t.Fatalf("unit without Meta: %v", err)
	}

	dir := t.TempDir()
	gs, err := driver.NewSender(d, backend.Target{Kind: backend.TargetDryRun, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	units, err := d.Render(context.Background(), drivertest.Job(t, d.Capabilities()), []raster.Page{grayRect(732, 30, 50, 5, 60, 25)})
	if err != nil || gs.Send(context.Background(), units[0]) != nil {
		t.Fatal(err)
	}
}
