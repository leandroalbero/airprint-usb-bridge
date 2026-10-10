package postscript

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func newDriver(t *testing.T, opts map[string]string, cfg ...driver.Config) *Driver {
	t.Helper()
	var c driver.Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	c.Options = opts
	d, err := driver.New("postscript", c)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Driver)
}

// textPage is an A4 sheet at 300 dpi with a few bars, some of them in the unprintable margin.
func textPage(m driver.Media) raster.Page {
	return drivertest.SheetPage(m, 300, 0,
		drivertest.Rect{X0: 0, Y0: 0, X1: 2480, Y1: 20},         // margin only: cleared
		drivertest.Rect{X0: 100, Y0: 100, X1: 900, Y1: 140},     // heading
		drivertest.Rect{X0: 100, Y0: 300, X1: 2300, Y1: 304},    // rule
		drivertest.Rect{X0: 1201, Y0: 3000, X1: 1203, Y1: 3300}, // thin line, odd x
	)
}

func TestRenderA4Golden(t *testing.T) {
	d := newDriver(t, nil)
	job := drivertest.Job(t, d.Capabilities())
	job.Name = "golden"
	if job.Media.Name != driver.MediaA4 {
		t.Fatalf("default media %+v", job.Media)
	}
	page := textPage(job.Media)
	want := imaging.Threshold(page, 128)
	for y := 0; y < want.H; y++ {
		for x := 0; x < want.W; x++ {
			if x < 50 || y < 50 || x >= want.W-50 || y >= want.H-50 {
				want.Set(x, y, false)
			}
		}
	}
	res := drivertest.Run(t, d, job, page)
	if len(res.Units) != 1 || res.Units[0].Impressions != 1 {
		t.Fatalf("units %+v", res.Units)
	}
	doc, err := decodeDoc(res.Data)
	if err != nil {
		t.Fatal(err)
	}
	im := doc.Images[0]
	if im.W != 2480 || im.H != 3508 || im.Depth != 1 || im.Scale != "595.2 841.92" {
		t.Fatalf("image %dx%d depth %d scale %s", im.W, im.H, im.Depth, im.Scale)
	}
	if !bytes.Equal(im.Data, want.Pix) {
		t.Fatal("decoded samples differ from the placed, thresholded page")
	}
	if strings.Join(doc.Setup, "|") != "/PageSize [595.28 841.89]" {
		t.Fatalf("setup %q", doc.Setup)
	}
	wantInspect := "PostScript LanguageLevel 2, ASCII85 RunLengthDecode, 1 pages, pjl false\n" +
		"setpagedevice /PageSize [595.28 841.89]\n" +
		"page 1: image 2480x3508 depth 1 scaled 595.2 841.92 pt, "
	if !strings.HasPrefix(res.Inspect[0], wantInspect) || !strings.HasSuffix(res.Inspect[0], ", 41400 black dots\n") {
		t.Fatalf("inspect:\n%s", res.Inspect[0])
	}
	drivertest.GoldenDigest(t, "a4-level2.sha256", res.Data)
	drivertest.Golden(t, "attributes.golden", []byte(res.Attributes))
	if res.TXT["URF"] != "V1.4,W8,CP99,IS1,MT1,RS300" || res.TXT["pdl"] != "image/pwg-raster,image/urf" ||
		res.TXT["Duplex"] != "F" || res.TXT["ty"] != "Generic PostScript Printer" {
		t.Fatalf("TXT %v", res.TXT)
	}
	for _, a := range []string{
		"printer-kind (keyword) = document\n",
		"media-ready (keyword) = iso_a4_210x297mm\n",
		"copies-supported (rangeOfInteger) = 1-99\n",
		"sides-supported (keyword) = one-sided\n",
	} {
		if !strings.Contains(res.Attributes, a) {
			t.Errorf("attributes lack %q", a)
		}
	}
}

func TestRenderVariantsRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		opts   map[string]string
		dpi    int
		level  int
		binary bool
		depth  int
	}{
		{"level3", map[string]string{"level": "3"}, 300, 3, false, 1},
		{"binary-rle", map[string]string{"encoding": "binary"}, 300, 2, true, 1},
		{"binary-flate", map[string]string{"encoding": "binary", "level": "3"}, 300, 3, true, 1},
		{"gray", map[string]string{"depth": "8"}, 300, 2, false, 8},
		{"600dpi", map[string]string{"resolution": "600"}, 600, 2, false, 1},
		{"600dpi-gets-300", map[string]string{"resolution": "600"}, 300, 2, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDriver(t, c.opts, driver.Config{Media: "a5"})
			job := drivertest.Job(t, d.Capabilities())
			if job.Media.Name != driver.MediaA5 {
				t.Fatalf("media %s", job.Media.Name)
			}
			grad := drivertest.SheetPage(job.Media, c.dpi, 0)
			for i := range grad.Gray {
				grad.Gray[i] = byte(i % grad.Width * 255 / grad.Width)
			}
			sheet, err := imaging.Place(grad, job.Media, imaging.PlaceOptions{XDPI: c.dpi, YDPI: c.dpi})
			if err != nil {
				t.Fatal(err)
			}
			want := sheet.Gray
			if c.depth == 1 {
				bm, dithered, err := imaging.Binarize(sheet, "")
				if err != nil || !dithered {
					t.Fatalf("binarize: %v dithered %v", err, dithered)
				}
				want = bm.Pix
			}
			res := drivertest.Run(t, d, job, grad, textPage(job.Media))
			doc, err := decodeDoc(res.Data)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Level != c.level || doc.Binary != c.binary || len(doc.Images) != 2 {
				t.Fatalf("doc level %d binary %v images %d", doc.Level, doc.Binary, len(doc.Images))
			}
			im := doc.Images[0]
			if im.Depth != c.depth || im.W != sheet.Width || im.H != sheet.Height || !bytes.Equal(im.Data, want) {
				t.Fatalf("page 1: %dx%d depth %d, want %dx%d depth %d (samples equal %v)",
					im.W, im.H, im.Depth, sheet.Width, sheet.Height, c.depth, bytes.Equal(im.Data, want))
			}
			if res.Units[0].Impressions != 2 {
				t.Fatalf("impressions %d", res.Units[0].Impressions)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	d := newDriver(t, map[string]string{"duplex": "true", "resolution": "600", "passthrough": "true"},
		driver.Config{Media: "Letter", DeviceID: ieee1284.Parse("MFG:Acme;MDL:Acme Laser 9000;CMD:PJL,POSTSCRIPT;")})
	caps := d.Capabilities()
	drivertest.CheckCapabilities(t, caps)
	if caps.MakeModel != "Acme Laser 9000" || caps.DeviceID != "" || caps.Verified || !caps.CopiesNative {
		t.Fatalf("caps %+v", caps)
	}
	if got := strings.Join(caps.URF, ","); got != "V1.4,W8,CP99,DM1,IS1,MT1,RS300-600" {
		t.Fatalf("URF %s", got)
	}
	if caps.Resolutions[0] != (driver.Resolution{X: 600, Y: 600}) || len(caps.Resolutions) != 2 {
		t.Fatalf("resolutions %v", caps.Resolutions)
	}
	if got := strings.Join(caps.ReadyNames(), ","); got != driver.MediaLetter || caps.Default().Name != driver.MediaLetter {
		t.Fatalf("ready %s default %s", got, caps.Default().Name)
	}
	attrs, txt := drivertest.Attributes(t, caps)
	if txt["Duplex"] != "T" || txt["pdl"] != "application/postscript,image/pwg-raster,image/urf" {
		t.Fatalf("TXT %v", txt)
	}
	for _, a := range []string{
		"printer-device-id (textWithoutLanguage) = MFG:Acme;MDL:Laser 9000;CMD:URF,PWG,PS;\n",
		"sides-supported (1setOf keyword) = one-sided,two-sided-long-edge,two-sided-short-edge\n",
	} {
		if !strings.Contains(attrs, a) {
			t.Errorf("attributes lack %q:\n%s", a, attrs)
		}
	}
	if m := newDriver(t, nil, driver.Config{DeviceID: ieee1284.Parse("MFG:Acme;MDL:Laser 9000;")}).caps.MakeModel; m != "Acme Laser 9000" {
		t.Errorf("make and model %q", m)
	}
}

func TestDuplexSides(t *testing.T) {
	for _, c := range []struct {
		duplex bool
		sides  string
		want   string
	}{
		{true, "two-sided-long-edge", "/Duplex true /Tumble false"},
		{true, "two-sided-short-edge", "/Duplex true /Tumble true"},
		{true, "one-sided", "/Duplex false"},
		{false, "one-sided", ""},
		{false, "two-sided-long-edge", ""},
	} {
		opts := map[string]string{"duplex": "false"}
		if c.duplex {
			opts["duplex"] = "true"
		}
		d := newDriver(t, opts, driver.Config{Media: "a5"})
		job := drivertest.Job(t, d.Capabilities())
		job.Sides = c.sides
		units, err := d.Render(context.Background(), job, []raster.Page{textPage(job.Media), textPage(job.Media)})
		if err != nil {
			t.Fatal(err)
		}
		doc, err := decodeDoc(units[0].Data)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, s := range doc.Setup {
			if strings.HasPrefix(s, "/Duplex") {
				got = s
			}
		}
		if got != c.want {
			t.Errorf("duplex %v sides %s: %q, want %q", c.duplex, c.sides, got, c.want)
		}
	}
}

func TestCopies(t *testing.T) {
	d := newDriver(t, map[string]string{"duplex": "true"})
	job := drivertest.Job(t, d.Capabilities())
	job.Copies = 3
	units, err := d.Render(context.Background(), job, []raster.Page{textPage(job.Media)})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Impressions != 3 || !bytes.Contains(units[0].Data, []byte("<< /NumCopies 3 >> setpagedevice")) {
		t.Fatalf("one page, 3 copies: %d units, %d impressions", len(units), units[0].Impressions)
	}

	// Odd page count, duplex, two copies: whole jobs again, so copy 2 starts on a new sheet.
	job.Copies, job.Sides = 2, "two-sided-long-edge"
	pages := []raster.Page{textPage(job.Media), textPage(job.Media), textPage(job.Media)}
	units, err = d.Render(context.Background(), job, pages)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || units[0].Impressions != 3 || units[1].Impressions != 3 ||
		!bytes.Equal(units[0].Data, units[1].Data) || bytes.Contains(units[0].Data, []byte("NumCopies")) {
		t.Fatalf("3 pages, 2 copies: %d units", len(units))
	}
	if units[1].Label != "job (3 pages), copy 2 of 2" {
		t.Errorf("label %q", units[1].Label)
	}
	for i := range pages {
		if pages[i].Gray != nil {
			t.Errorf("page %d gray not released", i+1)
		}
	}
}

func TestBlankPagesKept(t *testing.T) {
	d := newDriver(t, map[string]string{"duplex": "true"})
	job := drivertest.Job(t, d.Capabilities())
	job.Sides = "two-sided-long-edge"
	res := drivertest.Run(t, d, job, textPage(job.Media), drivertest.SheetPage(job.Media, 300, 0), textPage(job.Media))
	if doc, err := decodeDoc(res.Data); err != nil || len(doc.Images) != 3 || doc.Pages != 3 {
		t.Fatalf("blank back page dropped: %v", err)
	}
}

func TestPassThrough(t *testing.T) {
	src := []byte("%!PS-Adobe-3.0\n%%Pages: 2\n%%EndComments\nshowpage showpage\n%%EOF\n")
	off := newDriver(t, nil)
	job := drivertest.Job(t, off.Capabilities())
	if _, err := off.RenderDocument(context.Background(), job, formatPS, src); !errors.Is(err, driver.ErrDocument) {
		t.Fatalf("pass-through while disabled: %v", err)
	}

	d := newDriver(t, map[string]string{"passthrough": "true"})
	job.Copies = 2
	units, err := d.RenderDocument(context.Background(), job, formatPS, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || !bytes.Equal(units[0].Data, src) || units[0].Impressions != 2 {
		t.Fatalf("units %d, data changed %v, impressions %d", len(units), !bytes.Equal(units[0].Data, src), units[0].Impressions)
	}
	text, err := d.Inspect(units[0].Data)
	if err != nil || text != "pass-through PostScript: 64 bytes, 2 pages (DSC), \"%!PS-Adobe-3.0\"\n" {
		t.Fatalf("inspect %q %v", text, err)
	}
	for _, bad := range [][]byte{[]byte("%PDF-1.7\n"), []byte("hello"), nil} {
		if _, err := d.RenderDocument(context.Background(), job, formatPS, bad); !errors.Is(err, driver.ErrDocument) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if _, err := d.RenderDocument(context.Background(), job, "application/pdf", src); !errors.Is(err, driver.ErrDocument) {
		t.Errorf("pdf accepted: %v", err)
	}

	dp := newDriver(t, map[string]string{"passthrough": "true", "duplex": "true", "pjl": "true"})
	job = drivertest.Job(t, dp.Capabilities())
	job.Sides = "two-sided-short-edge"
	units, err = dp.RenderDocument(context.Background(), job, formatPS, src)
	if err != nil {
		t.Fatal(err)
	}
	want := "\x1b%-12345X@PJL JOB\r\n@PJL ENTER LANGUAGE = POSTSCRIPT\r\n%!PS\n" +
		"[{ << /Duplex true /Tumble true >> setpagedevice } stopped cleartomark\n" + string(src) +
		"\x1b%-12345X@PJL EOJ\r\n\x1b%-12345X"
	if string(units[0].Data) != want || len(units) != 1 {
		t.Fatalf("wrapped:\n%q", units[0].Data)
	}
	if text, err := dp.Inspect(units[0].Data); err != nil || !strings.HasPrefix(text, "pass-through PostScript: ") {
		t.Fatalf("inspect %q %v", text, err)
	}

	units, err = dp.RenderDocument(context.Background(), job, formatPS, append([]byte("\x04"), src...))
	if err != nil || bytes.Contains(units[0].Data, []byte("\x04")) {
		t.Fatalf("leading ^D kept after the Duplex request: %v %q", err, units[0].Data)
	}

	pjl := []byte("\x1b%-12345X@PJL ENTER LANGUAGE = POSTSCRIPT\n%!PS\nshowpage\n\x1b%-12345X")
	units, err = dp.RenderDocument(context.Background(), job, formatPS, pjl)
	if err != nil || !bytes.Equal(units[0].Data, pjl) {
		t.Fatalf("PJL-wrapped document changed: %v %q", err, units[0].Data)
	}
}

func TestErrors(t *testing.T) {
	for _, opts := range []map[string]string{
		{"level": "1"}, {"level": "x"}, {"depth": "4"}, {"encoding": "hex"}, {"resolution": "1200"},
		{"duplex": "maybe"}, {"passthrough": "2"}, {"pjl": "?"}, {"colour": "1"},
	} {
		if _, err := driver.New("postscript", driver.Config{Options: opts}); err == nil {
			t.Errorf("options %v accepted", opts)
		}
	}
	if _, err := driver.New("postscript", driver.Config{Dither: "sometimes"}); err == nil {
		t.Error("dither sometimes accepted")
	}
	_, err := driver.New("postscript", driver.Config{Media: "a3"})
	if err == nil || !strings.Contains(err.Error(), "a4, letter, legal, a5, executive") {
		t.Errorf("unknown media: %v", err)
	}
	if d := newDriver(t, nil, driver.Config{Media: driver.MediaLegal}); d.caps.DefaultMedia != driver.MediaLegal {
		t.Errorf("PWG media name not taken: %s", d.caps.DefaultMedia)
	}

	d := newDriver(t, nil)
	job := drivertest.Job(t, d.Capabilities())
	bad := raster.Page{Width: 10, Height: 10, XDPI: 300, YDPI: 300, Gray: make([]byte, 5)}
	if _, err := d.Render(context.Background(), job, []raster.Page{textPage(job.Media), bad}); !errors.Is(err, driver.ErrDocument) {
		t.Errorf("malformed page: %v", err)
	}
	roll := job
	roll.Media = driver.Media{Name: "roll_current_62x0mm", Width: 6200, Roll: true}
	if _, err := d.Render(context.Background(), roll, []raster.Page{textPage(job.Media)}); !errors.Is(err, driver.ErrDocument) {
		t.Errorf("roll media: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Render(ctx, job, []raster.Page{textPage(job.Media)}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	if units, err := d.Render(context.Background(), job, nil); err != nil || units != nil {
		t.Errorf("no pages: %v %v", units, err)
	}
	if _, err := d.Inspect([]byte("\x1bE\x1b*r1A")); err == nil {
		t.Error("PCL inspected as PostScript")
	}
}

func TestLandscapePageTurned(t *testing.T) {
	d := newDriver(t, nil)
	job := drivertest.Job(t, d.Capabilities())
	land := drivertest.Page(3508, 2480, 300, func(x, y int) byte {
		if x >= 100 && x < 400 && y >= 100 && y < 200 {
			return 0
		}
		return 255
	})
	res := drivertest.Run(t, d, job, land)
	doc, err := decodeDoc(res.Data)
	if err != nil {
		t.Fatal(err)
	}
	if im := doc.Images[0]; im.W != 2480 || im.H != 3508 {
		t.Fatalf("landscape page placed as %dx%d", im.W, im.H)
	}
}

func FuzzInspect(f *testing.F) {
	d := &Driver{}
	var buf bytes.Buffer
	w := newDocWriter(&buf, settings{Level: 2, Depth: 1}, docInfo{Width: 21000, Length: 29700}, 1)
	_ = w.page(pageImage{W: 16, H: 2, XDPI: 300, YDPI: 300, Depth: 1, Data: []byte{1, 2, 3, 4}})
	_ = w.finish()
	f.Add(buf.Bytes())
	f.Add([]byte("%!PS\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = d.Inspect(data)
	})
}
