package zpl_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/zpl"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func newDriver(t *testing.T, media string, opts map[string]string) driver.Driver {
	t.Helper()
	d, err := driver.New(zpl.Name, driver.Config{Media: media, Options: opts, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// rectBitmap is the label a SheetPage at the driver's dpi must print: exactly the rects.
func rectBitmap(w, h int, rects ...drivertest.Rect) *imaging.Bitmap {
	bm := imaging.NewBitmap(w, h)
	for _, r := range rects {
		for y := r.Y0; y < r.Y1; y++ {
			for x := r.X0; x < r.X1; x++ {
				bm.Set(x, y, true)
			}
		}
	}
	return bm
}

func onlyLabel(t *testing.T, data []byte) zplLabel {
	t.Helper()
	labels := readZPL(t, data)
	if len(labels) != 1 {
		t.Fatalf("%d label formats, want 1", len(labels))
	}
	return labels[0]
}

func TestDefault4x6At203(t *testing.T) {
	d := newDriver(t, "", nil)
	caps := d.Capabilities()
	job := drivertest.Job(t, caps)
	if job.Media.Name != driver.Media4x6 || !job.Media.Ready || caps.Roll != nil {
		t.Fatalf("default media %+v", job.Media)
	}
	if got := caps.ReadyNames(); len(got) != 1 || got[0] != driver.Media4x6 {
		t.Fatalf("ready %v", got)
	}
	// 4 x 6 in at 203 dpi is 812 x 1218 dots: the page is copied dot for dot.
	rects := []drivertest.Rect{{X0: 0, Y0: 0, X1: 812, Y1: 3}, {X0: 100, Y0: 1000, X1: 300, Y1: 1100}, {X0: 805, Y0: 1215, X1: 812, Y1: 1218}}
	pages := []raster.Page{drivertest.SheetPage(job.Media, 203, 0, rects...)}
	res := drivertest.Run(t, d, job, pages...)
	if pages[0].Gray != nil {
		t.Error("page gray not released")
	}
	if len(res.Units) != 1 || res.Units[0].Impressions != 1 || res.Units[0].Label != "label 1 of 1" {
		t.Fatalf("units %+v", res.Units)
	}
	if !strings.HasPrefix(string(res.Data), "^XA\n^MNY\n^PW812\n^LL1218\n^LH0,0\n^FO0,0^GFA,99960,99960,102,:Z64:") {
		t.Fatalf("data starts %.80q", res.Data)
	}
	l := onlyLabel(t, res.Data)
	if l.PW != 812 || l.LL != 1218 {
		t.Fatalf("label %dx%d", l.PW, l.LL)
	}
	// 99999/102 = 980 rows per field.
	if len(l.Fields) != 2 || l.Fields[1].Y != 980 || l.Fields[1].Total != 238*102 || l.Fields[0].Encoding != "z64" {
		t.Fatalf("fields %+v", l.Fields)
	}
	if diff := diffBitmaps(l.Bitmap, rectBitmap(812, 1218, rects...)); diff != "" {
		t.Fatalf("bitmap: %s", diff)
	}
	drivertest.Golden(t, "inspect-4x6-203.golden", []byte(strings.Join(res.Inspect, "")))
	drivertest.Golden(t, "attributes-4x6-203.golden", []byte(res.Attributes))
	if res.TXT["URF"] != "V1.4,W8,CP99,RS203" {
		t.Fatalf("TXT %v", res.TXT)
	}
	for _, want := range []string{
		"printer-kind (keyword) = label\n",
		"printer-resolution-supported (resolution) = 203x203dpi\n",
		"media-ready (keyword) = na_index-4x6_4x6in\n",
		"copies-supported (rangeOfInteger) = 1-99\n",
		"printer-make-and-model (textWithoutLanguage) = Zebra ZPL Label Printer\n",
	} {
		if !strings.Contains(res.Attributes, want) {
			t.Errorf("attributes lack %q", want)
		}
	}
}

// A small hex label, golden byte for byte. The prefix is worked out by hand: rows 0-7 blank (","
// then ":" x7); rows 8-15 are dots 8-397, i.e. 00, 97 F nibbles (j=80 + W=17) then C, rest 0.
func TestHexGolden2x1(t *testing.T) {
	d := newDriver(t, "2x1", map[string]string{"encoding": "hex", "darkness": "15"})
	job := drivertest.Job(t, d.Capabilities())
	if job.Media.Name != "oe_2x1-label_2x1in" {
		t.Fatalf("media %+v", job.Media)
	}
	rects := []drivertest.Rect{{X0: 8, Y0: 8, X1: 398, Y1: 16}, {X0: 8, Y0: 16, X1: 16, Y1: 195}, {X0: 100, Y0: 100, X1: 101, Y1: 101}}
	res := drivertest.Run(t, d, job, drivertest.SheetPage(job.Media, 203, 0, rects...))
	if !strings.HasPrefix(string(res.Data), "~SD15\n^XA\n^MNY\n^PW406\n^LL203\n^LH0,0\n^FO0,0^GFA,10353,10353,51,,:::::::00jWFC,:::::::00FF,") {
		t.Fatalf("data starts %.80q", res.Data)
	}
	if !strings.HasSuffix(string(res.Data), "^FS\n^XZ\n") {
		t.Fatalf("data ends %q", res.Data[len(res.Data)-20:])
	}
	l := onlyLabel(t, res.Data)
	if l.Commands[0] != "~SD15" || l.Fields[0].Encoding != "hex" {
		t.Fatalf("label %+v", l.Commands)
	}
	if diff := diffBitmaps(l.Bitmap, rectBitmap(406, 203, rects...)); diff != "" {
		t.Fatalf("bitmap: %s", diff)
	}
	drivertest.Golden(t, "label-2x1-hex.zpl", res.Data)
}

func TestContinuous300(t *testing.T) {
	d := newDriver(t, "continuous", map[string]string{"dpi": "300"})
	caps := d.Capabilities()
	if caps.Roll == nil || caps.Roll.Width != 10160 || strings.Join(caps.Kind, ",") != "label,roll" {
		t.Fatalf("caps %+v", caps)
	}
	job := drivertest.Job(t, caps)
	if !job.Media.Roll || job.Media.Length != 0 {
		t.Fatalf("job media %+v", job.Media)
	}
	rects := []drivertest.Rect{{X0: 0, Y0: 0, X1: 1200, Y1: 1}, {X0: 600, Y0: 300, X1: 700, Y1: 599}}
	res := drivertest.Run(t, d, job, drivertest.SheetPage(job.Media, 300, 600, rects...))
	if !strings.HasPrefix(string(res.Data), "^XA\n^MNN\n^PW1200\n^LL600\n") {
		t.Fatalf("data starts %.40q", res.Data)
	}
	l := onlyLabel(t, res.Data)
	if diff := diffBitmaps(l.Bitmap, rectBitmap(1200, 600, rects...)); diff != "" {
		t.Fatalf("bitmap: %s", diff)
	}
	drivertest.Golden(t, "attributes-continuous-300.golden", []byte(res.Attributes))

	// A preset length on the roll is the roll cut to that length.
	m, ok := caps.MatchSize(10160, 5080)
	if !ok || !m.Roll || m.Length != 5080 {
		t.Fatalf("4x2 on the roll: %+v %v", m, ok)
	}
	job.Media = m
	res = drivertest.Run(t, d, job, drivertest.SheetPage(m, 300, 0, drivertest.Rect{X0: 10, Y0: 10, X1: 20, Y1: 20}))
	if !strings.HasPrefix(string(res.Data), "^XA\n^MNN\n^PW1200\n^LL600\n") {
		t.Fatalf("data starts %.40q", res.Data)
	}
}

// A page sent at another resolution is scaled to the label at the head's.
func TestResampledPage(t *testing.T) {
	d := newDriver(t, "4x6", map[string]string{"tracking": "mark"})
	job := drivertest.Job(t, d.Capabilities())
	page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 0, Y0: 0, X1: 600, Y1: 900})
	res := drivertest.Run(t, d, job, page)
	l := onlyLabel(t, res.Data)
	if l.PW != 812 || l.LL != 1218 || l.Commands[0] != "^MNM" {
		t.Fatalf("label %dx%d %v", l.PW, l.LL, l.Commands)
	}
	if !l.Bitmap.At(0, 0) || !l.Bitmap.At(400, 600) || l.Bitmap.At(420, 620) {
		t.Fatal("top-left quarter not black")
	}
}

func TestBandsAt300(t *testing.T) {
	d := newDriver(t, "4x6", map[string]string{"dpi": "300", "encoding": "hex"})
	job := drivertest.Job(t, d.Capabilities())
	all := drivertest.Rect{X0: 0, Y0: 0, X1: 1200, Y1: 1800}
	res := drivertest.Run(t, d, job, drivertest.SheetPage(job.Media, 300, 0, all))
	l := onlyLabel(t, res.Data)
	// 99999/150 = 666 rows per field: 666 + 666 + 468.
	if len(l.Fields) != 3 || l.Fields[2].Y != 1332 || l.Fields[2].Total != 468*150 {
		t.Fatalf("fields %+v", l.Fields)
	}
	if l.Bitmap.Ink() != 1200*1800 {
		t.Fatalf("ink %d", l.Bitmap.Ink())
	}
	if !strings.Contains(string(res.Data), "^GFA,99900,99900,150,!:::") {
		t.Fatal("solid rows not encoded as ! and :")
	}
}

func TestBlankBandsSkipped(t *testing.T) {
	d := newDriver(t, "4x6", nil)
	job := drivertest.Job(t, d.Capabilities())
	res := drivertest.Run(t, d, job, drivertest.SheetPage(job.Media, 203, 0, drivertest.Rect{X0: 1, Y0: 1100, X1: 2, Y1: 1101}), drivertest.SheetPage(job.Media, 203, 0))
	if len(res.Units) != 2 || res.Units[1].Label != "label 2 of 2" {
		t.Fatalf("units %d", len(res.Units))
	}
	l := onlyLabel(t, res.Units[0].Data)
	if len(l.Fields) != 1 || l.Fields[0].Y != 980 || l.Bitmap.Ink() != 1 || !l.Bitmap.At(1, 1100) {
		t.Fatalf("fields %+v ink %d", l.Fields, l.Bitmap.Ink())
	}
	if got, want := string(res.Units[1].Data), "^XA\n^MNY\n^PW812\n^LL1218\n^LH0,0\n^XZ\n"; got != want {
		t.Fatalf("blank label %q, want %q", got, want)
	}
	if res.Inspect[1] != "^XA ^MNY ^PW812 ^LL1218 ^LH0,0 ^XZ\n" {
		t.Fatalf("inspect %q", res.Inspect[1])
	}
}

// A client may pick a supported size that is not the loaded one; the label is formatted for it.
func TestUnloadedSize(t *testing.T) {
	d := newDriver(t, "4x6", nil)
	caps := d.Capabilities()
	m, ok := caps.LookupName("oe_2x1-label_2x1in")
	if !ok || m.Ready {
		t.Fatalf("2x1: %+v %v", m, ok)
	}
	job := drivertest.Job(t, caps)
	job.Media = m
	res := drivertest.Run(t, d, job, drivertest.SheetPage(m, 203, 0))
	if !strings.HasPrefix(string(res.Data), "^XA\n^MNY\n^PW406\n^LL203\n") {
		t.Fatalf("data starts %.40q", res.Data)
	}
}

func TestCustomSizes(t *testing.T) {
	for _, tc := range []struct{ media, name string }{
		{"100x150mm", "om_100x150mm_100x150mm"},
		{"4X6IN", driver.Media4x6},
		{"101.6x152.4mm", driver.Media4x6},
		{"2.25x1.25", "oe_2.25x1.25-label_2.25x1.25in"},
	} {
		d := newDriver(t, tc.media, nil)
		caps := d.Capabilities()
		if got := caps.Default().Name; got != tc.name {
			t.Errorf("%s: default %s, want %s", tc.media, got, tc.name)
		}
	}
	d := newDriver(t, "continuous-80mm", nil)
	c := d.Capabilities()
	if c.Roll.Width != 8000 || len(c.Media) != 0 || c.Roll.DefaultLength != 12000 {
		t.Fatalf("80 mm roll: %+v %+v", c.Roll, c.Media)
	}
	d = newDriver(t, "continuous-2in", nil)
	if c := d.Capabilities(); len(c.Media) != 1 || c.Media[0].Name != "oe_2x1-label_2x1in" || c.Media[0].Ready {
		t.Fatalf("2 in roll presets %+v", c.Media)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		media string
		opts  map[string]string
		want  string
	}{
		{"", map[string]string{"dpi": "250"}, "dpi 250"},
		{"", map[string]string{"dpi": "x"}, "want an integer"},
		{"", map[string]string{"darkness": "31"}, "darkness"},
		{"", map[string]string{"darkness": "-1"}, "darkness"},
		{"", map[string]string{"darkness": "dark"}, "darkness"},
		{"", map[string]string{"tracking": "hole"}, "tracking"},
		{"", map[string]string{"encoding": "b64"}, "encoding"},
		{"", map[string]string{"print-width": "-3"}, "print-width"},
		{"", map[string]string{"speed": "4"}, "has no option"},
		{"bogus", nil, "want a label size"},
		{"0x1", nil, "want a label size"},
		{"continuous-", nil, "want a label size"},
		{"5x6", nil, "wider than the print width 832"},
		{"4x200", nil, "longer than ZPL"},
	} {
		_, err := driver.New(zpl.Name, driver.Config{Media: tc.media, Options: tc.opts})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("media %q opts %v: %v, want %q", tc.media, tc.opts, err, tc.want)
		}
	}
	if _, err := driver.New(zpl.Name, driver.Config{Dither: "maybe"}); err == nil {
		t.Error("dither maybe accepted")
	}
	d, err := driver.New(zpl.Name, driver.Config{Media: "5x6", Options: map[string]string{"print-width": "1300", "dpi": "203"}})
	if err != nil {
		t.Fatalf("wide head: %v", err)
	}
	if caps := d.Capabilities(); caps.Default().Width != 12700 {
		t.Fatalf("wide head: %+v", caps.Default())
	}
}

func TestDeviceID(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	d, err := driver.New(zpl.Name, driver.Config{DeviceID: ieee1284.Parse("MFG:Zebra Technologies;CMD:ZPL;MDL:ZTC GK420d;"), Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Capabilities().MakeModel; got != "Zebra ZTC GK420d" || len(logs) != 0 {
		t.Fatalf("make and model %q, logs %v", got, logs)
	}
	d, err = driver.New(zpl.Name, driver.Config{DeviceID: ieee1284.Parse("MFG:Zebra;CMD:EPL;MDL:LP2844;"), Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if d.Capabilities().MakeModel != "Zebra LP2844" || len(logs) != 1 || !strings.Contains(logs[0], "does not list ZPL") {
		t.Fatalf("EPL printer: %q %v", d.Capabilities().MakeModel, logs)
	}
}

func TestRenderErrors(t *testing.T) {
	d := newDriver(t, "4x6", nil)
	job := drivertest.Job(t, d.Capabilities())
	bad := raster.Page{Width: 10, Height: 10, XDPI: 203, YDPI: 203, Gray: make([]byte, 5)}
	units, err := d.Render(context.Background(), job, []raster.Page{drivertest.SheetPage(job.Media, 203, 0), bad})
	if !errors.Is(err, driver.ErrDocument) || units != nil {
		t.Fatalf("malformed page: %v %d units", err, len(units))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Render(ctx, job, []raster.Page{drivertest.SheetPage(job.Media, 203, 0)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}

	roll := newDriver(t, "continuous-10mm", nil)
	job = drivertest.Job(t, roll.Capabilities())
	long := drivertest.Page(80, zpl.MaxDots+1, 203, nil)
	if _, err := roll.Render(context.Background(), job, []raster.Page{long}); !errors.Is(err, driver.ErrDocument) || !strings.Contains(err.Error(), "32000") {
		t.Fatalf("too long: %v", err)
	}
}

func TestInspectRejects(t *testing.T) {
	good := "^XA\n^PW16\n^LL1\n^FO0,0" + string(zpl.GraphicField(imaging.NewBitmap(16, 1), zpl.EncodingZ64)) + "^FS\n^XZ\n"
	if _, err := zpl.Inspect([]byte(good)); err != nil {
		t.Fatal(err)
	}
	crc := good[strings.Index(good, "^FS")-4 : strings.Index(good, "^FS")]
	flipped := "0000"
	if crc == flipped {
		flipped = "0001"
	}
	for name, data := range map[string]string{
		"crc":       strings.Replace(good, crc+"^FS", flipped+"^FS", 1),
		"unclosed":  "^XA^PW8^LL1",
		"text":      "hello^XA^XZ",
		"empty":     "",
		"counts":    "^XA^FO0,0^GFA,3,2,1,FFFF^FS^XZ",
		"hex short": "^XA^FO0,0^GFA,4,4,2,FFF^FS^XZ",
		"hex long":  "^XA^FO0,0^GFA,2,2,2,FFFF00^FS^XZ",
	} {
		if _, err := zpl.Inspect([]byte(data)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	out, err := zpl.Inspect([]byte("~SD20^XA^FO0,0^GFA,4,4,2,!,^FS^XZ"))
	if err != nil || out != "~SD20 ^XA ^FO0,0 ^GFA[hex 16x2 dots, 4 bytes, ink 16] ^FS ^XZ\n" {
		t.Fatalf("inspect %q %v", out, err)
	}
}

func TestRegistered(t *testing.T) {
	info, ok := driver.Lookup(zpl.Name)
	if !ok || info.Verified != "" {
		t.Fatalf("info %+v", info)
	}
	var keys []string
	for _, o := range info.Options {
		keys = append(keys, o.Key)
	}
	if got := strings.Join(keys, ","); got != "dpi,darkness,tracking,encoding,print-width" {
		t.Fatalf("options %s", got)
	}
	d := newDriver(t, "", nil)
	if d.Name() != "zpl" || d.Capabilities().Verified {
		t.Fatal("name or verified")
	}
	if _, ok := d.(driver.Inspector); !ok {
		t.Fatal("no Inspector")
	}
}
