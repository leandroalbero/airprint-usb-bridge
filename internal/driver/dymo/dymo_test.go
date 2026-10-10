package dymo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// parsed is what the test parser reads back from one unit, following the LW 450 Technical
// Reference independently of the encoder.
type parsed struct {
	resync   int // leading ESC bytes before the first command
	dotTab   int
	bpl      int
	length   int // ESC L value, MSB first
	mode     byte
	density  byte
	rows     [][]byte
	feed     byte // final ESC E / ESC G
	commands []string
}

// parse is strict: it accepts only what a label from this driver should contain and fails on
// anything else, including data after the final form feed.
func parse(t *testing.T, data []byte) parsed {
	t.Helper()
	var p parsed
	p.bpl = 84 // power-on default
	i := 0
	for i < len(data) && data[i] == esc {
		i++
	}
	// The last ESC of the run starts the first command.
	p.resync = i - 1
	i--
	for i < len(data) {
		switch data[i] {
		case syn:
			if i+1+p.bpl > len(data) {
				t.Fatalf("raster line at %d truncated", i)
			}
			p.rows = append(p.rows, append([]byte(nil), data[i+1:i+1+p.bpl]...))
			i += 1 + p.bpl
		case esc:
			if i+1 >= len(data) {
				t.Fatalf("ESC at end of data")
			}
			cmd := data[i+1]
			p.commands = append(p.commands, string(cmd))
			i += 2
			switch cmd {
			case 'B':
				p.dotTab = int(data[i])
				i++
			case 'D':
				p.bpl = int(data[i])
				i++
			case 'L':
				p.length = int(data[i])<<8 | int(data[i+1])
				i += 2
			case 'h', 'i':
				p.mode = cmd
			case 'c', 'd', 'e', 'g':
				p.density = cmd
			case 'E', 'G':
				p.feed = cmd
				if i != len(data) {
					t.Fatalf("%d bytes after the form feed", len(data)-i)
				}
			default:
				t.Fatalf("unexpected command ESC %q at %d", cmd, i-2)
			}
		default:
			t.Fatalf("unexpected byte 0x%02x at %d", data[i], i)
		}
	}
	if p.feed == 0 {
		t.Fatal("no form feed at the end")
	}
	return p
}

func (p parsed) ink(x, y int) bool {
	if y >= len(p.rows) || x/8 >= len(p.rows[y]) {
		return false
	}
	return p.rows[y][x/8]&(0x80>>(x%8)) != 0
}

// checkBitmap compares the parsed rows with the rects (in label dots) for every dot of a w x h
// label; rows past the last one sent count as white.
func checkBitmap(t *testing.T, p parsed, w, h int, rects ...drivertest.Rect) {
	t.Helper()
	bad := 0
	for y := 0; y < h; y++ {
		for x := 0; x < max(w, p.bpl*8); x++ {
			want := false
			for _, r := range rects {
				if x >= r.X0 && x < r.X1 && y >= r.Y0 && y < r.Y1 {
					want = true
				}
			}
			if got := p.ink(x, y); got != want {
				if bad < 5 {
					t.Errorf("dot (%d,%d) = %v, want %v", x, y, got, want)
				}
				bad++
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d dots differ", bad)
	}
}

func newTest(t *testing.T, c driver.Config) *Driver {
	t.Helper()
	if c.Logf == nil {
		c.Logf = t.Logf
	}
	d, err := newDriver(c)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func opts(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestDieCutHandChecked(t *testing.T) {
	d := newTest(t, driver.Config{Media: "30332"})
	job := drivertest.Job(t, d.Capabilities())
	if job.Media.Name != "om_25x25mm_25x25mm" || job.Media.Roll {
		t.Fatalf("default media %+v", job.Media)
	}
	// 25 mm at 300 dpi = 295.3 dots: 295 dots, 37 bytes per line.
	rect := drivertest.Rect{X0: 40, Y0: 30, X1: 201, Y1: 100}
	page := drivertest.SheetPage(job.Media, 300, 0, rect)
	if page.Width != 295 || page.Height != 295 {
		t.Fatalf("page %dx%d", page.Width, page.Height)
	}
	res := drivertest.Run(t, d, job, page)
	if len(res.Units) != 1 || res.Units[0].Label != "label 1 of 1 (page 1)" || res.Units[0].Impressions != 1 {
		t.Fatalf("units %+v", res.Units)
	}
	data := res.Units[0].Data

	// Hand-built from the manual: 85 ESC (one more than the 84-byte head line), ESC B 0,
	// ESC D 37, ESC L 0x01 0x72 (295 lines + 75 = 370, MSB first), ESC h (300x300 text mode),
	// ESC e (normal density).
	want := append(bytes.Repeat([]byte{0x1b}, 85),
		0x1b, 0x42, 0x00,
		0x1b, 0x44, 37,
		0x1b, 0x4c, 0x01, 0x72,
		0x1b, 0x68,
		0x1b, 0x65)
	if !bytes.HasPrefix(data, want) {
		t.Fatalf("header\n got % x\nwant % x", data[:len(want)], want)
	}
	// Lines stop at the last inked row (99); the form feed finds the next label.
	body := data[len(want):]
	if len(body) != 100*(1+37)+2 || body[0] != 0x16 || !bytes.Equal(body[len(body)-2:], []byte{0x1b, 0x45}) {
		t.Fatalf("body %d bytes, starts % x, ends % x", len(body), body[:2], body[len(body)-2:])
	}
	// Row 30, the rect's first: dots 40..200 black. Byte 5 holds dots 40-47.
	row30 := body[30*38+1 : 31*38]
	if row30[4] != 0 || row30[5] != 0xff || row30[25] != 0x80 || row30[26] != 0 {
		t.Fatalf("row 30: % x", row30)
	}

	p := parse(t, data)
	if p.resync != 85 || p.bpl != 37 || p.length != 370 || p.mode != 'h' || p.density != 'e' || p.feed != 'E' || len(p.rows) != 100 {
		t.Fatalf("parsed %+v", p)
	}
	checkBitmap(t, p, 295, 295, rect)
	if got, want := res.Inspect[0], "resync 85 | dot-tab 0 | bytes-per-line 37 | label-length 370 | text-mode 300x300 | density normal | lines 100 (11270 dots) | form-feed"; got != want {
		t.Fatalf("inspect\n got %s\nwant %s", got, want)
	}
	drivertest.Golden(t, "diecut-30332.bin", data)
}

// Raster bytes equal to <esc>, <syn> and <etb> must stay pixels: the printer reads exactly
// bytes-per-line bytes after <syn>.
func TestDataLooksLikeCommands(t *testing.T) {
	d := newTest(t, driver.Config{Media: "99010"})
	job := drivertest.Job(t, d.Capabilities())
	pattern := []byte{esc, syn, etb, 'E', esc, 'L'}
	draw := func(x, y int) byte {
		if y < 20 || y >= 60 || x < 16 || x >= 16+8*len(pattern) {
			return 255
		}
		if pattern[(x-16)/8]&(0x80>>(x%8)) != 0 {
			return 0
		}
		return 255
	}
	page := drivertest.Page(331, 1051, 300, draw)
	res := drivertest.Run(t, d, job, page)
	p := parse(t, res.Units[0].Data)
	if len(p.rows) != 60 || p.bpl != 42 {
		t.Fatalf("rows %d bpl %d", len(p.rows), p.bpl)
	}
	for y := 20; y < 60; y++ {
		if !bytes.Equal(p.rows[y][2:2+len(pattern)], pattern) {
			t.Fatalf("row %d: % x", y, p.rows[y][:10])
		}
	}
	if !strings.Contains(res.Inspect[0], "lines 60 (") {
		t.Fatalf("inspect %s", res.Inspect[0])
	}
}

func TestContinuousTrim(t *testing.T) {
	d := newTest(t, driver.Config{Media: "continuous"})
	caps := d.Capabilities()
	if caps.Roll == nil || caps.Roll.Width != 5600 || len(caps.Media) != 0 {
		t.Fatalf("caps %+v", caps)
	}
	job := drivertest.Job(t, caps)
	if !job.Media.Roll || job.Media.Length != 0 {
		t.Fatalf("job media %+v", job.Media)
	}
	// 56 mm = 661 dots: 83 bytes per line.
	rects := []drivertest.Rect{{X0: 20, Y0: 100, X1: 300, Y1: 150}, {X0: 400, Y0: 180, X1: 640, Y1: 230}}
	page := drivertest.SheetPage(job.Media, 300, 600, rects...)
	if page.Width != 661 {
		t.Fatalf("page width %d", page.Width)
	}
	res := drivertest.Run(t, d, job, page)
	p := parse(t, res.Units[0].Data)
	if p.length != 0xffff || p.bpl != 83 || p.resync != 85 || len(p.rows) != 130 {
		t.Fatalf("parsed length %#x bpl %d resync %d rows %d", p.length, p.bpl, p.resync, len(p.rows))
	}
	for i := range rects {
		rects[i].Y0 -= 100
		rects[i].Y1 -= 100
	}
	checkBitmap(t, p, 661, 130, rects...)
	if !strings.Contains(res.Inspect[0], "label-length continuous | text-mode 300x300 | density normal | lines 130 (") {
		t.Fatalf("inspect %s", res.Inspect[0])
	}
	drivertest.Golden(t, "continuous-56.bin", res.Units[0].Data)
	drivertest.Golden(t, "attributes-continuous.golden", []byte(res.Attributes))
	if !strings.Contains(res.Attributes, "media-ready (keyword) = roll_current_56x0mm\n") ||
		!strings.Contains(res.Attributes, "printer-kind (1setOf keyword) = label,roll\n") {
		t.Fatalf("attributes:\n%s", res.Attributes)
	}

	// trim=false keeps the requested length.
	d = newTest(t, driver.Config{Media: "continuous-57", Options: opts("trim", "false")})
	job = drivertest.Job(t, d.Capabilities())
	m, ok := d.caps.MatchSize(5700, 5000)
	if !ok || !m.Roll {
		t.Fatalf("roll match %+v %v", m, ok)
	}
	job.Media = m
	page = drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 100, Y0: 200, X1: 200, Y1: 210})
	res = drivertest.Run(t, d, job, page)
	p = parse(t, res.Units[0].Data)
	if len(p.rows) != 591 || p.bpl != 85-1 {
		t.Fatalf("rows %d bpl %d", len(p.rows), p.bpl)
	}
	checkBitmap(t, p, 673, 591, drivertest.Rect{X0: 100, Y0: 200, X1: 200, Y1: 210})
}

func TestContinuousTooLong(t *testing.T) {
	d := newTest(t, driver.Config{Media: "continuous-20"})
	job := drivertest.Job(t, d.Capabilities())
	page := drivertest.Page(236, 15000, 300, func(x, y int) byte { return byte(255 * ((x / 50) % 2)) })
	_, err := d.Render(context.Background(), job, []raster.Page{page})
	if !errors.Is(err, driver.ErrDocument) || !strings.Contains(err.Error(), "at most 1000 mm") {
		t.Fatalf("err %v", err)
	}
}

func TestMultiPageSkipsBlank(t *testing.T) {
	d := newTest(t, driver.Config{Options: opts("density", "dark")})
	job := drivertest.Job(t, d.Capabilities())
	if job.Media.Name != "om_36x89mm_36x89mm" {
		t.Fatalf("default %s", job.Media.Name)
	}
	r := drivertest.Rect{X0: 50, Y0: 50, X1: 300, Y1: 400}
	pages := []raster.Page{
		drivertest.SheetPage(job.Media, 300, 0, r),
		drivertest.SheetPage(job.Media, 300, 0),
		// A 600 dpi page is reduced to the 300 dpi head.
		drivertest.SheetPage(job.Media, 600, 0, drivertest.Rect{X0: 100, Y0: 100, X1: 600, Y1: 800}),
	}
	res := drivertest.Run(t, d, job, pages...)
	if len(res.Units) != 2 || res.Units[0].Label != "label 1 of 2 (page 1)" || res.Units[1].Label != "label 2 of 2 (page 3)" {
		t.Fatalf("units %d %q", len(res.Units), res.Units[0].Label)
	}
	for _, pg := range pages {
		if pg.Gray != nil {
			t.Error("page gray not freed")
		}
	}
	for _, u := range res.Units {
		p := parse(t, u.Data)
		if p.density != 'g' || p.bpl != 54 || p.length != 1051+75 {
			t.Fatalf("parsed %+v", p)
		}
		checkBitmap(t, p, 425, 1051, r)
	}
	drivertest.GoldenDigest(t, "diecut-99012-dark.sha256", res.Data)
}

func TestLandscapePageTurned(t *testing.T) {
	d := newTest(t, driver.Config{Media: "99012"})
	job := drivertest.Job(t, d.Capabilities())
	// An 89 x 36 mm landscape page with a bar along its top edge lands on the label's left side
	// after a counter-clockwise turn.
	page := drivertest.Page(1051, 425, 300, func(x, y int) byte {
		if y >= 20 && y < 40 && x >= 100 && x < 900 {
			return 0
		}
		return 255
	})
	res := drivertest.Run(t, d, job, page)
	p := parse(t, res.Units[0].Data)
	checkBitmap(t, p, 425, 1051, drivertest.Rect{X0: 20, Y0: 1051 - 900, X1: 40, Y1: 1051 - 100})
}

func TestWideLabelCroppedToHead(t *testing.T) {
	d := newTest(t, driver.Config{Media: "30256"})
	caps := d.Capabilities()
	ready := caps.ReadyNames()
	if len(ready) != 1 || ready[0] != "om_59x102mm_59x102mm" {
		t.Fatalf("ready %v", ready)
	}
	m, _ := caps.LookupName(ready[0])
	if m.Margins.Right != 5900-5690 {
		t.Fatalf("margins %+v", m.Margins)
	}
	job := drivertest.Job(t, caps)
	page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 0, Y0: 0, X1: 697, Y1: 1205})
	res := drivertest.Run(t, d, job, page)
	p := parse(t, res.Units[0].Data)
	if p.bpl != 84 {
		t.Fatalf("bpl %d", p.bpl)
	}
	// Margins: 12 dots (1 mm) left, top and bottom; the right one starts at the head's edge.
	checkBitmap(t, p, 672, 1205, drivertest.Rect{X0: 12, Y0: 12, X1: 672, Y1: 1205 - 12})
}

func TestDotTab(t *testing.T) {
	d := newTest(t, driver.Config{Media: "11354", Options: opts("dot-tab", "2")})
	job := drivertest.Job(t, d.Capabilities())
	page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 0, Y0: 0, X1: 673, Y1: 378})
	res := drivertest.Run(t, d, job, page)
	p := parse(t, res.Units[0].Data)
	if p.dotTab != 2 || p.bpl != 82 {
		t.Fatalf("dot tab %d bpl %d", p.dotTab, p.bpl)
	}
	if !strings.Contains(res.Inspect[0], "dot-tab 2 | bytes-per-line 82") {
		t.Fatalf("inspect %s", res.Inspect[0])
	}
}

func Test4XL(t *testing.T) {
	id := ieee1284.Parse("MFG:DYMO;CMD: ;MDL:LabelWriter 4XL;CLS:PRINTER;")
	d := newTest(t, driver.Config{DeviceID: id})
	caps := d.Capabilities()
	if caps.MakeModel != "DYMO LabelWriter 4XL" || caps.DefaultMedia != driver.Media4x6 {
		t.Fatalf("caps %q %q", caps.MakeModel, caps.DefaultMedia)
	}
	job := drivertest.Job(t, caps)
	r := drivertest.Rect{X0: 100, Y0: 100, X1: 1100, Y1: 1700}
	res := drivertest.Run(t, d, job, drivertest.SheetPage(job.Media, 300, 0, r))
	p := parse(t, res.Units[0].Data)
	// 4 in = 1200 dots: 150 bytes; the resync run beats the 4XL's 156-byte line.
	if p.resync != 157 || p.bpl != 150 || p.length != 1800+75 {
		t.Fatalf("resync %d bpl %d length %d", p.resync, p.bpl, p.length)
	}
	checkBitmap(t, p, 1200, 1800, r)
	drivertest.GoldenDigest(t, "diecut-4xl-4x6.sha256", res.Data)
	drivertest.Golden(t, "attributes-4xl.golden", []byte(res.Attributes))

	d = newTest(t, driver.Config{Media: "continuous"})
	if d.caps.Roll.Width != 5600 {
		t.Fatal(d.caps.Roll.Width)
	}
	d = newTest(t, driver.Config{Media: "continuous", Options: opts("model", "4xl")})
	if d.caps.Roll.Width != 10500 || d.caps.MakeModel != "DYMO LabelWriter 4XL" {
		t.Fatal(d.caps.Roll.Width, d.caps.MakeModel)
	}
}

func TestDieCutAttributes(t *testing.T) {
	id := ieee1284.Parse("MFG:DYMO;CMD: ;MDL:LabelWriter 450 Turbo;CLS:PRINTER;")
	d := newTest(t, driver.Config{DeviceID: id})
	caps := d.Capabilities()
	attrs, txt := drivertest.Attributes(t, caps)
	drivertest.CheckCapabilities(t, caps)
	drivertest.Golden(t, "attributes-diecut.golden", []byte(attrs))
	for _, want := range []string{
		"printer-make-and-model (textWithoutLanguage) = DYMO LabelWriter 450 Turbo\n",
		"printer-kind (keyword) = label\n",
		"media-ready (keyword) = om_36x89mm_36x89mm\n",
		"media-default (keyword) = om_36x89mm_36x89mm\n",
	} {
		if !strings.Contains(attrs, want) {
			t.Errorf("attributes lack %q", want)
		}
	}
	if strings.Contains(attrs, "roll_") || strings.Contains(attrs, "4x6") {
		t.Errorf("die-cut 450 attributes mention a roll or 4XL media:\n%s", attrs)
	}
	if txt["URF"] != "V1.4,W8,CP99,RS300" || txt["ty"] != "DYMO LabelWriter 450 Turbo" {
		t.Errorf("TXT %v", txt)
	}
}

func TestNewErrors(t *testing.T) {
	for _, c := range []struct {
		cfg  driver.Config
		want string
	}{
		{driver.Config{DeviceID: ieee1284.Parse("MFG:DYMO;MDL:LabelWriter 550;")}, "5xx series"},
		{driver.Config{DeviceID: ieee1284.Parse("MFG:DYMO;MDL:LabelWriter 550 Turbo;")}, "5xx series"},
		{driver.Config{DeviceID: ieee1284.Parse("MFG:DYMO;MDL:LabelWriter 5XL;")}, "5xx series"},
		{driver.Config{Options: opts("model", "550")}, "not supported"},
		{driver.Config{Options: opts("model", "LabelWriter 5XL")}, "not supported"},
		{driver.Config{Options: opts("model", "QL-700")}, "want a LabelWriter"},
		{driver.Config{Options: opts("density", "extra")}, "density"},
		{driver.Config{Options: opts("dot-tab", "84")}, "dot-tab=84"},
		{driver.Config{Options: opts("dot-tab", "x")}, "integer"},
		{driver.Config{Options: opts("trim", "maybe")}, "true or false"},
		{driver.Config{Dither: "sometimes"}, "dither"},
		{driver.Config{Media: "62"}, "unknown"},
		{driver.Config{Media: "4x6"}, "unknown for the LabelWriter 450"},
		{driver.Config{Media: "continuous-5"}, "continuous width"},
		{driver.Config{Media: "continuous-70"}, "continuous width"},
	} {
		_, err := newDriver(c.cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err %v, want %q", c.cfg, err, c.want)
		}
	}
}

func TestModelAndMediaNames(t *testing.T) {
	for _, c := range []struct {
		cfg    driver.Config
		model  string
		ready  string
		header int
	}{
		{driver.Config{}, "DYMO LabelWriter 450", "om_36x89mm_36x89mm", 672},
		{driver.Config{Options: opts("model", "450 Turbo")}, "DYMO LabelWriter 450 Turbo", "om_36x89mm_36x89mm", 672},
		{driver.Config{Options: opts("model", "LabelWriter 400"), Media: "30252"}, "DYMO LabelWriter 400", "om_28x89mm_28x89mm", 672},
		{driver.Config{Media: "om_19x51mm_19x51mm"}, "DYMO LabelWriter 450", "om_19x51mm_19x51mm", 672},
		{driver.Config{Options: opts("model", "4XL"), Media: "S0904980"}, "DYMO LabelWriter 4XL", "om_104x159mm_104x159mm", 1248},
		{driver.Config{DeviceID: ieee1284.Parse("MFG:DYMO;MDL:LabelWriter Wireless;")}, "DYMO LabelWriter Wireless", "om_36x89mm_36x89mm", 672},
	} {
		d := newTest(t, c.cfg)
		if d.caps.MakeModel != c.model || d.caps.ReadyNames()[0] != c.ready || d.model.headDots != c.header {
			t.Errorf("%+v: %q %v %d", c.cfg, d.caps.MakeModel, d.caps.ReadyNames(), d.model.headDots)
		}
	}
}

func TestRegistry(t *testing.T) {
	info, ok := driver.Lookup(Name)
	if !ok || !strings.Contains(info.Media, "99012") || info.Verified != "" {
		t.Fatalf("info %+v", info)
	}
	dr, err := driver.New(Name, driver.Config{Media: "continuous", Options: opts("density", "light")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dr.(driver.Inspector); !ok {
		t.Error("no Inspector")
	}
	if _, err := driver.New(Name, driver.Config{Options: opts("speed", "fast")}); err == nil {
		t.Error("unknown option accepted")
	}
}

func TestRenderCancelled(t *testing.T) {
	d := newTest(t, driver.Config{})
	job := drivertest.Job(t, d.Capabilities())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.Render(ctx, job, []raster.Page{drivertest.SheetPage(job.Media, 300, 0)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestRenderMalformedPage(t *testing.T) {
	d := newTest(t, driver.Config{})
	job := drivertest.Job(t, d.Capabilities())
	_, err := d.Render(context.Background(), job, []raster.Page{{Width: 10, Height: 10, XDPI: 300, YDPI: 300}})
	if !errors.Is(err, driver.ErrDocument) {
		t.Fatalf("err %v", err)
	}
}

func TestDecode(t *testing.T) {
	// Appendix A examples: 0x0F = 16 white, 0xFF = 128 black, 0x80 = 1 black.
	line := []byte{etb, 0xff, 0x80, 0x0f, 0x7f, 0x7f, 0x7f, 0x7f, 0x0e}
	stream := append([]byte{esc, esc, esc, esc, 'D', 84, esc, 'L', 0x0b, 0xf2, esc, 'i', esc, 'f', 1, 10, esc, 'q', '1'}, line...)
	stream = append(stream, esc, 'G')
	got, err := Decode(stream)
	if err != nil {
		t.Fatal(err)
	}
	want := "resync 3 | bytes-per-line 84 | label-length 3058 | graphics-mode 300x600 | skip 10 lines | roll 1 | lines 1 (129 dots) | short-form-feed"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	for _, bad := range [][]byte{
		{esc, 'D', 2, syn, 1},
		{esc, 'Z'},
		{0x41},
		{esc, 'D', 2, etb, 0x07},
		{esc, 'D', 1, etb, 0x3f},
		{esc, 'L', 1},
	} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("% x decoded", bad)
		}
	}
}

func ExampleDecode() {
	data := append(bytes.Repeat([]byte{esc}, 85), esc, 'D', 2, esc, 'L', 0xff, 0xff, esc, 'e', syn, 0xf0, 0x01, esc, 'E')
	s, _ := Decode(data)
	fmt.Println(s)
	// Output: resync 85 | bytes-per-line 2 | label-length continuous | density normal | lines 1 (5 dots) | form-feed
}
