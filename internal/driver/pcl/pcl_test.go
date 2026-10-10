package pcl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func newDriver(t testing.TB, cfg driver.Config) *Driver {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Driver)
}

func opts(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func jobFor(t testing.TB, d *Driver, media string) driver.JobInfo {
	t.Helper()
	caps := d.Capabilities()
	job := drivertest.Job(t, caps)
	if media != "" {
		m, ok := caps.LookupName(media)
		if !ok {
			t.Fatalf("media %s not offered", media)
		}
		job.Media = m
	}
	return job
}

// Logical page in dots at 300 dpi, from the PCL 5 logical page table (portrait).
var logical300 = map[string][2]int{
	driver.MediaA4:        {71, 2338},
	driver.MediaLetter:    {75, 2400},
	driver.MediaLegal:     {75, 2400},
	driver.MediaA5:        {71, 1598},
	driver.MediaExecutive: {75, 2025},
}

// wantPage is what the printer should image: the page placed and binarized the way the driver
// documents, cropped to the logical page.
func wantPage(t testing.TB, p raster.Page, media driver.Media, dpi int, dither string, o imaging.PlaceOptions) *imaging.Bitmap {
	t.Helper()
	o.XDPI, o.YDPI = dpi, dpi
	sheet, err := imaging.Place(p, media, o)
	if err != nil {
		t.Fatal(err)
	}
	bm, _, err := imaging.Binarize(sheet, dither)
	if err != nil {
		t.Fatal(err)
	}
	lp := logical300[media.Name]
	off, w := lp[0]*dpi/300, lp[1]*dpi/300
	out := imaging.NewBitmap(w, bm.H)
	for y := 0; y < bm.H; y++ {
		for x := 0; x < w; x++ {
			out.Set(x, y, bm.At(off+x, y))
		}
	}
	return out
}

func comparePage(t testing.TB, n int, got pclPage, want *imaging.Bitmap) {
	t.Helper()
	if got.width != want.W {
		t.Fatalf("page %d: raster width %d, want %d", n, got.width, want.W)
	}
	if len(got.rows) > want.H {
		t.Fatalf("page %d: %d rows, sheet has %d", n, len(got.rows), want.H)
	}
	blank := make([]byte, want.Stride)
	for y := 0; y < want.H; y++ {
		row := blank
		if y < len(got.rows) {
			row = got.rows[y]
		}
		if !bytes.Equal(row, want.Row(y)) {
			t.Fatalf("page %d row %d differs:\n got %x\nwant %x", n, y, row, want.Row(y))
		}
	}
}

// pattern draws text-like blocks, a diagonal line, a grey ramp and marks touching every edge of
// the page, so cropping and margins show up in the comparison.
func pattern(w, h, dpi int) raster.Page {
	return drivertest.Page(w, h, dpi, func(x, y int) byte {
		switch {
		case y%97 < 9 && x%61 < 40:
			return 0
		case x == y || x+1 == y:
			return 10
		case y > h/2 && y < h/2+dpi/4:
			return byte(x * 255 / w)
		case x < 3 || y < 3 || x >= w-3 || y >= h-3:
			return 0
		}
		return 255
	})
}

func render(t testing.TB, d *Driver, job driver.JobInfo, pages ...raster.Page) drivertest.Result {
	t.Helper()
	return drivertest.Run(t, d, job, pages...)
}

func clonePages(pages []raster.Page) []raster.Page {
	out := make([]raster.Page, len(pages))
	for i, p := range pages {
		out[i] = p
		out[i].Gray = append([]byte(nil), p.Gray...)
	}
	return out
}

// TestHandCheckedA4 spells out the whole job for an A4 page with one small black block, from the
// PCL 5 and PJL references: the block is 16 dots wide starting 8 dots into the logical page
// (which begins 71 dots from the A4 edge at 300 dpi), so rows 100 and 101 are 00 ff ff, a
// 3-byte literal in PackBits (02 00 ff ff); the 100 blank rows above are one Y offset.
func TestHandCheckedA4(t *testing.T) {
	d := newDriver(t, driver.Config{})
	job := jobFor(t, d, "")
	job.Name = "hello"
	page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 79, Y0: 100, X1: 95, Y1: 102})
	res := render(t, d, job, page)
	want := "\x1b%-12345X@PJL JOB NAME=\"hello\"\r\n" +
		"@PJL SET RESOLUTION=300\r\n" +
		"@PJL ENTER LANGUAGE=PCL\r\n" +
		"\x1bE" + // reset
		"\x1b&l26A" + // A4
		"\x1b&l0O" + // portrait
		"\x1b&l0L" + // perforation skip off
		"\x1b&l0E" + // top margin 0
		"\x1b*t300R" + // raster resolution
		"\x1b*p0x0Y" + // cursor to the logical page origin
		"\x1b*r2338S" + // raster width: the A4 logical page
		"\x1b*b2M" + // compression mode 2
		"\x1b*r1A" + // start raster at the cursor
		"\x1b*b100Y" +
		"\x1b*b4W\x02\x00\xff\xff" +
		"\x1b*b4W\x02\x00\xff\xff" +
		"\x1b*rB\f" +
		"\x1bE" +
		"\x1b%-12345X@PJL EOJ NAME=\"hello\"\r\n\x1b%-12345X"
	if got := string(res.Data); got != want {
		t.Fatalf("job bytes\n got %q\nwant %q", got, want)
	}
	if len(res.Units) != 1 || res.Units[0].Impressions != 1 {
		t.Fatalf("units %+v", res.Units)
	}
	drivertest.Golden(t, "a4-block.pcl", res.Data)
	drivertest.Golden(t, "a4-block.txt", []byte(res.Inspect[0]))
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		media       string
		dpi         int
		compression string
		dither      string
	}{
		{driver.MediaA4, 300, "2", "auto"},
		{driver.MediaA4, 300, "3", "on"},
		{driver.MediaLetter, 300, "2", "off"},
		{driver.MediaLetter, 300, "3", "auto"},
		{driver.MediaLegal, 300, "2", "auto"},
		{driver.MediaA5, 300, "3", "off"},
		{driver.MediaExecutive, 300, "2", "on"},
		{driver.MediaA5, 600, "2", "on"},
		{driver.MediaA5, 600, "3", "auto"},
		{driver.MediaA4, 600, "2", "off"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s-%d-mode%s-%s", c.media, c.dpi, c.compression, c.dither), func(t *testing.T) {
			t.Parallel()
			d := newDriver(t, driver.Config{Media: c.media, Dither: c.dither,
				Options: opts("dpi", fmt.Sprint(c.dpi), "compression", c.compression)})
			job := jobFor(t, d, c.media)
			w, h := (job.Media.Width*c.dpi+1270)/2540, (job.Media.Length*c.dpi+1270)/2540
			pages := []raster.Page{pattern(w, h, c.dpi)}
			if c.dpi == 300 {
				pages = append(pages, pattern(w/2, h/2, 150), drivertest.SheetPage(job.Media, 300, 0))
			}
			orig := clonePages(pages)
			res := render(t, d, job, pages...)
			for i := range pages {
				if pages[i].Gray != nil {
					t.Errorf("page %d gray not released", i+1)
				}
			}
			got := decodePCL(t, res.Data)
			if len(got.pages) != len(orig) || got.resets != 2 {
				t.Fatalf("%d pages, %d resets", len(got.pages), got.resets)
			}
			code, _ := paperFor(c.media)
			for i, p := range got.pages {
				if p.sizeCode != code.code || p.dpi != c.dpi || p.compression != int(c.compression[0]-'0') || p.duplex != -1 {
					t.Fatalf("page %d settings %+v", i+1, p)
				}
				comparePage(t, i+1, p, wantPage(t, orig[i], job.Media, c.dpi, c.dither, imaging.PlaceOptions{}))
			}
			if res.Units[0].Impressions != len(orig) {
				t.Errorf("impressions %d", res.Units[0].Impressions)
			}
			if got.pjl[1] != fmt.Sprintf("@PJL SET RESOLUTION=%d", c.dpi) {
				t.Errorf("PJL %q", got.pjl)
			}
			if c.dpi == 300 && len(got.pages[2].rows) != 0 {
				t.Errorf("blank page sent %d rows", len(got.pages[2].rows))
			}
		})
	}
}

func TestLandscapeAndScaling(t *testing.T) {
	d := newDriver(t, driver.Config{Dither: "off"})
	job := jobFor(t, d, driver.MediaLetter)
	job.Orientation = 4
	job.Scaling = "fit"
	land := pattern(1500, 1000, 300)
	orig := clonePages([]raster.Page{land})
	res := render(t, d, job, land)
	got := decodePCL(t, res.Data)
	want := wantPage(t, orig[0], job.Media, 300, "off", imaging.PlaceOptions{Orientation: 4, Scaling: "fit"})
	comparePage(t, 1, got.pages[0], want)
	if want.Ink() == 0 {
		t.Fatal("nothing placed")
	}
}

func TestDuplexAndCopies(t *testing.T) {
	d := newDriver(t, driver.Config{Dither: "off", Options: opts("duplex", "true")})
	caps := d.Capabilities()
	if !caps.Duplex || strings.Join(caps.URF, ",") != "V1.4,W8,CP99,DM1,IS1,MT1,RS300" {
		t.Fatalf("caps %+v", caps)
	}
	m := caps.Default()
	pages := func() []raster.Page {
		var ps []raster.Page
		for i := 0; i < 3; i++ {
			ps = append(ps, drivertest.SheetPage(m, 300, 0, drivertest.Rect{X0: 200, Y0: 200 + 100*i, X1: 400, Y1: 260 + 100*i}))
		}
		return ps
	}
	for _, c := range []struct {
		sides  string
		duplex int
		front  bool
	}{
		{"one-sided", 0, false},
		{"two-sided-long-edge", 1, true},
		{"two-sided-short-edge", 2, true},
	} {
		t.Run(c.sides, func(t *testing.T) {
			t.Parallel()
			job := jobFor(t, d, "")
			job.Sides, job.Copies = c.sides, 2
			in := pages()
			orig := clonePages(in)
			res := render(t, d, job, in...)
			checkCopies(t, res.Units, 2, 3)
			want := make([]*imaging.Bitmap, len(orig))
			for i := range orig {
				want[i] = wantPage(t, orig[i], m, 300, "off", imaging.PlaceOptions{})
			}
			got := decodePCL(t, res.Units[0].Data)
			if len(got.pages) != 3 {
				t.Fatalf("%d pages", len(got.pages))
			}
			for i, p := range got.pages {
				if p.duplex != c.duplex || p.frontSide {
					t.Fatalf("page %d duplex %d front-side %v", i+1, p.duplex, p.frontSide)
				}
				comparePage(t, i+1, p, want[i])
			}
		})
	}

	job := jobFor(t, d, "")
	job.Sides, job.Copies = "two-sided-long-edge", 3
	res := render(t, d, job, pages()[:2]...)
	checkCopies(t, res.Units, 3, 2)

	simplex := newDriver(t, driver.Config{})
	job = jobFor(t, simplex, "")
	job.Copies = 2
	res = render(t, simplex, job, pages()[0])
	checkCopies(t, res.Units, 2, 1)
	if got := decodePCL(t, res.Units[0].Data); len(got.pages) != 1 || got.pages[0].duplex != -1 {
		t.Errorf("simplex copies: %+v", got.pages)
	}
}

// checkCopies checks that each copy is the whole job again, held once: 99 copies of a long
// document must not cost 99 times its size.
func checkCopies(t *testing.T, units []driver.Unit, copies, pages int) {
	t.Helper()
	if len(units) != copies {
		t.Fatalf("%d units for %d copies", len(units), copies)
	}
	for i, u := range units {
		if u.Impressions != pages || len(u.Data) == 0 || &u.Data[0] != &units[0].Data[0] || len(u.Data) != len(units[0].Data) {
			t.Fatalf("copy %d: %d impressions, %d bytes, shared %v", i+1, u.Impressions, len(u.Data), &u.Data[0] == &units[0].Data[0])
		}
		if want := fmt.Sprintf("copy %d of %d", i+1, copies); !strings.HasSuffix(u.Label, want) {
			t.Fatalf("label %q", u.Label)
		}
	}
	if bytes.Contains(units[0].Data, []byte("\x1b&a1G")) {
		t.Fatal("front-side command in a single copy")
	}
}

func TestResolutionChoice(t *testing.T) {
	d := newDriver(t, driver.Config{Options: opts("dpi", "300,600")})
	caps := d.Capabilities()
	if fmt.Sprint(caps.Resolutions) != "[{300 300} {600 600}]" || fmt.Sprint(caps.Qualities) != "[3 4 5]" ||
		caps.URF[len(caps.URF)-1] != "RS300-600" {
		t.Fatalf("caps %v %v %v", caps.Resolutions, caps.Qualities, caps.URF)
	}
	for _, c := range []struct {
		quality int
		res     driver.Resolution
		want    int
	}{
		{driver.QualityNormal, driver.Resolution{}, 300},
		{driver.QualityDraft, driver.Resolution{}, 300},
		{driver.QualityHigh, driver.Resolution{}, 600},
		{driver.QualityDraft, driver.Resolution{X: 600, Y: 600}, 600},
		{driver.QualityHigh, driver.Resolution{X: 300, Y: 300}, 300},
	} {
		job := driver.JobInfo{Quality: c.quality, Resolution: c.res}
		if got := d.resolution(job); got != c.want {
			t.Errorf("quality %d resolution %v: %d dpi, want %d", c.quality, c.res, got, c.want)
		}
	}

	hi := newDriver(t, driver.Config{HighRes: true})
	if fmt.Sprint(hi.Capabilities().Resolutions) != "[{300 300} {600 600}]" {
		t.Errorf("high-res resolutions %v", hi.Capabilities().Resolutions)
	}
	six := newDriver(t, driver.Config{Options: opts("dpi", "600")})
	c6 := six.Capabilities()
	if fmt.Sprint(c6.Resolutions) != "[{600 600}]" || c6.URF[len(c6.URF)-1] != "RS600" || fmt.Sprint(c6.Qualities) != "[4]" {
		t.Errorf("600 caps %v %v %v", c6.Resolutions, c6.URF, c6.Qualities)
	}
	both := newDriver(t, driver.Config{Options: opts("dpi", "600,300")})
	if u := both.Capabilities().URF; u[len(u)-1] != "RS300-600" || both.resolution(driver.JobInfo{Quality: 4}) != 600 {
		t.Errorf("600,300: %v", u)
	}
}

func TestCapabilitiesAndAttributes(t *testing.T) {
	d := newDriver(t, driver.Config{})
	caps := d.Capabilities()
	drivertest.CheckCapabilities(t, caps)
	if caps.Verified || caps.Color || !caps.CopiesNative || caps.MaxCopies != 99 || caps.MakeModel != "Generic PCL 5 Laser" {
		t.Errorf("caps %+v", caps)
	}
	if fmt.Sprint(caps.ReadyNames()) != "[iso_a4_210x297mm]" || caps.Default().Name != driver.MediaA4 {
		t.Errorf("ready %v default %s", caps.ReadyNames(), caps.Default().Name)
	}
	margins := map[string]driver.Margins{
		driver.MediaA4:        {Top: 423, Bottom: 423, Left: 602, Right: 602},
		driver.MediaLetter:    {Top: 423, Bottom: 423, Left: 635, Right: 635},
		driver.MediaLegal:     {Top: 423, Bottom: 423, Left: 635, Right: 635},
		driver.MediaA5:        {Top: 423, Bottom: 423, Left: 602, Right: 669},
		driver.MediaExecutive: {Top: 423, Bottom: 423, Left: 635, Right: 635},
	}
	for _, m := range caps.Media {
		if m.Margins != margins[m.Name] || m.Type != "stationery" || m.Source != "main" {
			t.Errorf("media %+v", m)
		}
	}
	attrs, txt := drivertest.Attributes(t, caps)
	drivertest.Golden(t, "attributes.golden", []byte(attrs))
	if txt["URF"] != "V1.4,W8,CP99,IS1,MT1,RS300" || txt["Duplex"] != "F" || txt["Color"] != "F" {
		t.Errorf("TXT %v", txt)
	}
	for _, want := range []string{
		"printer-kind (keyword) = document\n",
		"media-ready (keyword) = iso_a4_210x297mm\n",
		"copies-supported (rangeOfInteger) = 1-99\n",
	} {
		if !strings.Contains(attrs, want) {
			t.Errorf("attributes lack %q", want)
		}
	}

	duplex := newDriver(t, driver.Config{Media: "Letter", Options: opts("duplex", "true", "dpi", "300,600")})
	attrs, txt = drivertest.Attributes(t, duplex.Capabilities())
	drivertest.Golden(t, "attributes-letter-duplex-600.golden", []byte(attrs))
	if txt["URF"] != "V1.4,W8,CP99,DM1,IS1,MT1,RS300-600" || txt["Duplex"] != "T" {
		t.Errorf("TXT %v", txt)
	}

	id := newDriver(t, driver.Config{DeviceID: ieee1284.Parse("MFG:Hewlett-Packard;MDL:HP LaserJet P2055;CMD:PJL,PCL,POSTSCRIPT;")})
	if mm := id.Capabilities().MakeModel; mm != "Hewlett-Packard HP LaserJet P2055" {
		t.Errorf("make and model %q", mm)
	}
	id = newDriver(t, driver.Config{DeviceID: ieee1284.Parse("MFG:Brother;MDL:Brother HL-L2350DW;CMD:PJL,PCL;")})
	if mm := id.Capabilities().MakeModel; mm != "Brother HL-L2350DW" {
		t.Errorf("make and model %q", mm)
	}
}

func TestRegistered(t *testing.T) {
	info, ok := driver.Lookup("pcl")
	if !ok || info.Verified != "" {
		t.Fatalf("lookup %+v %v", info, ok)
	}
	d, err := driver.New("pcl", driver.Config{Media: "legal", Options: opts("compression", "3", "pjl", "false")})
	if err != nil {
		t.Fatal(err)
	}
	caps := d.Capabilities()
	if d.Name() != "pcl" || caps.Default().Name != driver.MediaLegal {
		t.Fatalf("driver %s default %s", d.Name(), caps.Default().Name)
	}
	if _, err := driver.New("pcl", driver.Config{Options: opts("colour", "1")}); err == nil || !strings.Contains(err.Error(), "dpi, duplex, compression, pjl") {
		t.Errorf("unknown option: %v", err)
	}
}

func TestNoPJL(t *testing.T) {
	d := newDriver(t, driver.Config{Options: opts("pjl", "false")})
	job := jobFor(t, d, "")
	res := render(t, d, job, drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 500, Y0: 500, X1: 900, Y1: 600}))
	if !bytes.HasPrefix(res.Data, []byte("\x1bE\x1b&l26A")) || !bytes.HasSuffix(res.Data, []byte("\x1b*rB\f\x1bE")) ||
		bytes.Contains(res.Data, []byte("PJL")) {
		t.Fatalf("data %q...%q", res.Data[:12], res.Data[len(res.Data)-8:])
	}
	if got := decodePCL(t, res.Data); len(got.pjl) != 0 || len(got.pages) != 1 {
		t.Fatalf("decoded %+v", got)
	}
}

func TestJobNameCannotInjectPJL(t *testing.T) {
	d := newDriver(t, driver.Config{})
	job := jobFor(t, d, "")
	job.Name = "a\"\r\n@PJL SET COPIES=99\r\n\x1b%-12345X" + strings.Repeat("x", 100)
	res := render(t, d, job, drivertest.SheetPage(job.Media, 300, 0))
	got := decodePCL(t, res.Data)
	if len(got.pjl) != 4 {
		t.Fatalf("PJL lines %q", got.pjl)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(got.pjl[0], `@PJL JOB NAME="`), `"`)
	if strings.ContainsAny(name, "\"\r\n\x1b") || len(name) > 64 || !strings.HasPrefix(name, "a@PJL SET COPIES=99%-12345Xx") {
		t.Fatalf("name %q", name)
	}
	job.Name = "\x01\x02"
	if pjlName(job) != "job 1" {
		t.Errorf("fallback name %q", pjlName(job))
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct {
		cfg  driver.Config
		want string
	}{
		{driver.Config{Options: opts("dpi", "200")}, "dpi"},
		{driver.Config{Options: opts("dpi", "300,300")}, "dpi"},
		{driver.Config{Options: opts("compression", "4")}, "compression"},
		{driver.Config{Options: opts("compression", "x")}, "compression"},
		{driver.Config{Options: opts("duplex", "maybe")}, "duplex"},
		{driver.Config{Options: opts("pjl", "2x")}, "pjl"},
		{driver.Config{Media: "a3"}, "a4"},
		{driver.Config{Dither: "sometimes"}, "dither"},
	} {
		if _, err := New(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.cfg, err)
		}
	}

	d := newDriver(t, driver.Config{})
	job := jobFor(t, d, "")
	page := drivertest.SheetPage(job.Media, 300, 0)

	if _, err := d.Render(context.Background(), job, nil); !errors.Is(err, driver.ErrDocument) {
		t.Errorf("no pages: %v", err)
	}
	other := job
	other.Media = driver.Media{Name: driver.Media4x6, Width: 10160, Length: 15240}
	if _, err := d.Render(context.Background(), other, []raster.Page{page}); !errors.Is(err, driver.ErrDocument) {
		t.Errorf("unsupported media: %v", err)
	}
	bad := raster.Page{Width: 10, Height: 10, XDPI: 300, YDPI: 300, Gray: make([]byte, 5)}
	units, err := d.Render(context.Background(), job, []raster.Page{page, bad})
	if !errors.Is(err, driver.ErrDocument) || units != nil || !strings.Contains(err.Error(), "page 2") {
		t.Errorf("malformed page: %v %v", units, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Render(ctx, job, []raster.Page{page}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// TestDeltaRowHandChecked builds mode 3 commands by hand: the top three bits are the replacement
// count minus one, the low five the offset from the byte after the previous replacement, and an
// offset of 31 or more continues in extra bytes (255 meaning "another byte follows").
func TestDeltaRowHandChecked(t *testing.T) {
	seed := make([]byte, 400)
	row := make([]byte, 400)
	row[0] = 0xaa                              // 00 aa
	copy(row[3:], bytes.Repeat([]byte{1}, 10)) // offset 2, 8 bytes: e2 ...; then offset 0, 2 bytes: 20 ...
	row[44] = 0xbb                             // offset 31: 1f 00 bb
	row[331] = 0xcc                            // offset 286 = 31+255+0: 1f ff 00 cc
	want := []byte{0x00, 0xaa, 0xe2, 1, 1, 1, 1, 1, 1, 1, 1, 0x20, 1, 1, 0x1f, 0x00, 0xbb, 0x1f, 0xff, 0x00, 0xcc}
	if got := deltaRow(nil, row, seed); !bytes.Equal(got, want) {
		t.Fatalf("delta\n got % x\nwant % x", got, want)
	}
	if len(deltaRow(nil, row, row)) != 0 {
		t.Error("a row equal to its seed is not empty")
	}
}

func TestDeltaRowRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	seed := make([]byte, 700)
	for n := 0; n < 2000; n++ {
		row := append([]byte(nil), seed...)
		for k := rng.Intn(40); k > 0; k-- {
			i := rng.Intn(len(row))
			for l := rng.Intn(20); l >= 0 && i < len(row); l-- {
				row[i] = byte(rng.Intn(256))
				i++
			}
		}
		if rng.Intn(10) == 0 {
			row = make([]byte, len(row))
		}
		got, err := applyMode3(deltaRow(nil, row, seed), seed)
		if err != nil || !bytes.Equal(got, row) {
			t.Fatalf("row %d: %v", n, err)
		}
		seed = row
	}
}

func TestPackBitsRows(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	w := &writer{compression: 2}
	for n := 0; n < 2000; n++ {
		row := make([]byte, 1+rng.Intn(600))
		for k := rng.Intn(30); k > 0; k-- {
			i := rng.Intn(len(row))
			v := byte(rng.Intn(4) * 85)
			for l := rng.Intn(200); l >= 0 && i < len(row); l-- {
				row[i] = v
				i++
			}
		}
		got, err := unpackMode2(w.row(row), len(row))
		if err != nil || !bytes.Equal(got, row) {
			t.Fatalf("row %d: %v", n, err)
		}
	}
}

func TestInspect(t *testing.T) {
	if _, err := Inspect([]byte("\x1b*r1A\x1b*b9W\x00")); err == nil {
		t.Error("short row accepted")
	}
	if _, err := Inspect([]byte("hello")); err == nil {
		t.Error("text accepted")
	}
	if _, err := Inspect([]byte("\x1b*r1A")); err == nil {
		t.Error("unterminated raster accepted")
	}
	text, err := Inspect([]byte("\x1b*r1A\x1b*b2W\x1bE\x1b*b5Y\x1b*rB\f"))
	if err != nil || !strings.Contains(text, "1 rows sent (2 bytes), 5 rows skipped") || !strings.Contains(text, "FF (page 1)") {
		t.Errorf("%q %v", text, err)
	}
}

func BenchmarkRenderA4(b *testing.B) {
	d := newDriver(b, driver.Config{})
	job := jobFor(b, d, "")
	job.Logf = nil
	page := pattern(2480, 3508, 300)
	for i := 0; i < b.N; i++ {
		p := page
		p.Gray = append([]byte(nil), page.Gray...)
		if _, err := d.Render(context.Background(), job, []raster.Page{p}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestGoldenVariants pins the bytes of the paths TestHandCheckedA4 does not take; the raster
// sections are worked out by hand from the PCL 5 reference.
func TestGoldenVariants(t *testing.T) {
	t.Run("mode3-a5", func(t *testing.T) {
		d := newDriver(t, driver.Config{Media: "a5", Options: opts("compression", "3")})
		job := jobFor(t, d, "")
		// The A5 logical page starts 71 dots in. Rows 50-51: byte 0 set; row 52: byte 1 instead;
		// row 53 blank (a delta back to zero, as the seed is not zero); rows 54-59 blank with a
		// zero seed (a Y offset); row 60: byte 0 again.
		page := drivertest.SheetPage(job.Media, 300, 0,
			drivertest.Rect{X0: 71, Y0: 50, X1: 79, Y1: 52},
			drivertest.Rect{X0: 79, Y0: 52, X1: 87, Y1: 53},
			drivertest.Rect{X0: 71, Y0: 60, X1: 79, Y1: 61})
		res := render(t, d, job, page)
		want := "\x1b&l25A\x1b&l0O\x1b&l0L\x1b&l0E\x1b*t300R\x1b*p0x0Y\x1b*r1598S\x1b*b3M\x1b*r1A" +
			"\x1b*b50Y" +
			"\x1b*b2W\x00\xff" + // offset 0, 1 byte
			"\x1b*b0W" + // same as the seed
			"\x1b*b3W\x20\x00\xff" + // offset 0, 2 bytes
			"\x1b*b2W\x01\x00" + // offset 1, 1 byte: back to blank
			"\x1b*b6Y" +
			"\x1b*b2W\x00\xff" +
			"\x1b*rB\f\x1bE"
		if !bytes.Contains(res.Data, []byte(want)) {
			t.Fatalf("mode 3 page\n got %q\nwant %q", res.Data, want)
		}
		drivertest.Golden(t, "a5-mode3.pcl", res.Data)
	})

	t.Run("letter", func(t *testing.T) {
		d := newDriver(t, driver.Config{Media: "letter"})
		job := jobFor(t, d, "")
		// The Letter logical page starts 75 dots (1/4 inch) in: dots 91-98 are logical byte 2.
		page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 91, Y0: 60, X1: 99, Y1: 61})
		res := render(t, d, job, page)
		want := "\x1b&l2A\x1b&l0O\x1b&l0L\x1b&l0E\x1b*t300R\x1b*p0x0Y\x1b*r2400S\x1b*b2M\x1b*r1A" +
			"\x1b*b60Y\x1b*b4W\x02\x00\x00\xff\x1b*rB\f\x1bE"
		if !bytes.Contains(res.Data, []byte(want)) {
			t.Fatalf("letter page\n got %q\nwant %q", res.Data, want)
		}
	})

	t.Run("600dpi", func(t *testing.T) {
		d := newDriver(t, driver.Config{Options: opts("dpi", "600")})
		job := jobFor(t, d, "")
		// 142 dots in at 600 dpi: dots 142-149 are logical byte 0.
		page := drivertest.SheetPage(job.Media, 600, 0, drivertest.Rect{X0: 142, Y0: 200, X1: 150, Y1: 201})
		res := render(t, d, job, page)
		for _, want := range []string{"@PJL SET RESOLUTION=600\r\n", "\x1b*t600R", "\x1b*r4676S",
			"\x1b*b200Y\x1b*b2W\x00\xff\x1b*rB\f"} {
			if !bytes.Contains(res.Data, []byte(want)) {
				t.Errorf("no %q in %q", want, res.Data)
			}
		}
		drivertest.Golden(t, "a4-600.pcl", res.Data)
	})

	t.Run("duplex-copies", func(t *testing.T) {
		d := newDriver(t, driver.Config{Dither: "off", Options: opts("duplex", "true")})
		job := jobFor(t, d, "")
		job.Sides, job.Copies, job.Name = "two-sided-long-edge", 2, "duplex"
		var pages []raster.Page
		for i := 0; i < 3; i++ {
			pages = append(pages, drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 300, Y0: 300 + 50*i, X1: 340, Y1: 310 + 50*i}))
		}
		res := render(t, d, job, pages...)
		checkCopies(t, res.Units, 2, 3)
		head := "\x1bE\x1b&l26A\x1b&l0O\x1b&l1S\x1b&l0L\x1b&l0E\x1b*t300R\x1b*p0x0Y"
		if !bytes.Contains(res.Units[0].Data, []byte(head)) {
			t.Fatalf("no duplex header in %q", res.Units[0].Data)
		}
		if ffs := bytes.Count(res.Units[0].Data, []byte("\x1b*rB\f")); ffs != 3 {
			t.Fatalf("%d pages", ffs)
		}
		drivertest.GoldenDigest(t, "a4-duplex-3p-2c.sha256", res.Units[0].Data)
	})
}
