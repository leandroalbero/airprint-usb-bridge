package cups

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// systemFilter skips the test unless the CUPS filter is installed (macOS ships rastertohp and
// rastertolabel; Linux has them with the cups package). Their output differs between CUPS
// versions, so the tests check its structure, not its bytes.
func systemFilter(t *testing.T, name string) string {
	t.Helper()
	p, err := findFilter(name, DefaultFilterDirs)
	if err != nil {
		t.Skipf("no system %s: %v", name, err)
	}
	return p
}

func textPage(m driver.Media) raster.Page {
	return drivertest.SheetPage(m, 300, 0,
		drivertest.Rect{X0: 100, Y0: 100, X1: 400, Y1: 140},
		drivertest.Rect{X0: 100, Y0: 200, X1: 300, Y1: 230})
}

func TestRealRasterToHP(t *testing.T) {
	systemFilter(t, "rastertohp")
	d := newDriver(t, driver.Config{PPD: "testdata/laser-pcl.ppd"})
	caps := d.Capabilities()
	job := drivertest.Job(t, caps)
	res := drivertest.Run(t, d, job, textPage(job.Media), textPage(job.Media))
	out := res.Data
	if !bytes.HasPrefix(out, []byte("\x1bE")) || !bytes.HasSuffix(out, []byte("\x0c\x1bE")) {
		t.Errorf("not a reset-framed PCL job: % x ... % x", out[:min(16, len(out))], out[max(0, len(out)-16):])
	}
	for _, want := range []string{
		"\x1b&l26A",   // A4
		"\x1b&l0S",    // simplex
		"\x1b*t300R",  // 300 dpi
		"\x1b*r2379S", // raster width: the A4 imageable area
		"\x1b*r3408T", // raster height
		"\x1b*b298W",  // a raster row of cupsBytesPerLine bytes
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("no %q in the PCL", want)
		}
	}
	if n := bytes.Count(out, []byte("\x1b*r1A")); n != 2 {
		t.Errorf("%d raster starts, want one per page", n)
	}
	if !strings.Contains(res.Inspect[0], "PCL: ESC E, ESC &l6D") ||
		!strings.Contains(res.Inspect[0], "ESC *b#W") {
		t.Errorf("inspect %q", res.Inspect[0])
	}

	job.Media, _ = caps.LookupName(driver.MediaLetter)
	job.Sides = "two-sided-short-edge"
	job.Resolution = driver.Resolution{X: 600, Y: 600}
	out = drivertest.Run(t, d, job, textPage(job.Media)).Data
	for _, want := range []string{"\x1b&l2A", "\x1b*t600R"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("letter duplex 600 dpi: no %q", want)
		}
	}
	// The macOS rastertohp sends &l1S for short-edge too, although the header has Tumble set
	// (TestDriverDuplexAndResolution checks that it does).
	if !bytes.Contains(out, []byte("\x1b&l1S")) && !bytes.Contains(out, []byte("\x1b&l2S")) {
		t.Error("letter duplex: no &l1S or &l2S")
	}
}

func TestRealRasterToLabelZPL(t *testing.T) {
	systemFilter(t, "rastertolabel")
	d := newDriver(t, driver.Config{PPD: "testdata/label-zpl.ppd", Options: map[string]string{"options": "Darkness=Dark"}})
	caps := d.Capabilities()
	job := drivertest.Job(t, caps)
	if job.Media.Name != "om_50.8x25.4mm_50.8x25.4mm" {
		t.Fatalf("default media %+v", job.Media)
	}
	pages := []raster.Page{
		drivertest.SheetPage(job.Media, 203, 0, drivertest.Rect{X0: 20, Y0: 20, X1: 300, Y1: 60}),
		drivertest.SheetPage(job.Media, 203, 0, drivertest.Rect{X0: 20, Y0: 100, X1: 300, Y1: 180}),
	}
	res := drivertest.Run(t, d, job, pages...)
	out := string(res.Data)
	// 2 in at 203 dpi is 406 dots.
	for _, want := range []string{"~DGR:CUPS.GRF,", "^XA", "^PW406", "^XGR:CUPS.GRF,1,1^FS", "^XZ"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in the ZPL", want)
		}
	}
	if n := strings.Count(out, "^XGR:CUPS.GRF"); n != 2 {
		t.Errorf("%d labels recalled, want 2", n)
	}
	if !strings.Contains(res.Inspect[0], "ZPL:") {
		t.Errorf("inspect %q", res.Inspect[0])
	}
}

func TestRealRasterToLabelDymo(t *testing.T) {
	systemFilter(t, "rastertolabel")
	d := newDriver(t, driver.Config{PPD: "testdata/label-dymo.ppd"})
	caps := d.Capabilities()
	if caps.Kind[0] != "label" || caps.Media[0].Name != "om_28.58x88.9mm_28.58x88.9mm" {
		t.Fatalf("caps %+v", caps)
	}
	job := drivertest.Job(t, caps)
	res := drivertest.Run(t, d, job, drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 60, Y0: 100, X1: 280, Y1: 400}))
	out := res.Data
	// ESC @ resets, ESC D sets the bytes per line, SYN starts each raster line.
	for _, want := range []string{"\x1b@", "\x1bD", "\x16"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("no %q in the label data", want)
		}
	}
}

// TestRasterToPWG is an independent check of the raster encoding: libcups' reader (inside
// rastertopwg) parses the stream and re-encodes it as PWG raster, which the bridge's own PWG
// decoder reads back. PWG raster is always the whole page, so rastertopwg places the imageable
// area by the header's ImagingBoundingBox: pixels must come back unchanged at that offset.
func TestRasterToPWG(t *testing.T) {
	pwgFilter := systemFilter(t, "rastertopwg")
	laser, err := os.ReadFile("testdata/laser-pcl.ppd")
	if err != nil {
		t.Fatal(err)
	}
	ppd := filepath.Join(t.TempDir(), "gray8.ppd")
	gray := strings.ReplaceAll(string(laser), "cupsBitsPerColor 1/cupsColorSpace 3", "cupsBitsPerColor 8/cupsColorSpace 18")
	if err := os.WriteFile(ppd, []byte(gray), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newDriver(t, driver.Config{PPD: ppd, Options: map[string]string{"filter": "-"}})
	job := drivertest.Job(t, d.Capabilities())
	job.Media, _ = d.caps.LookupName(driver.MediaA5)
	src := drivertest.Page(1748, 2480, 300, func(x, y int) byte { return byte((x/7 + y/3) % 256) })
	ras := drivertest.Run(t, d, job, src).Data

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pwgFilter, "1", "tester", "test", "1", "")
	cmd.Env = []string{"PPD=" + ppd, "CONTENT_TYPE=application/vnd.cups-raster"}
	cmd.Stdin = bytes.NewReader(ras)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pwg, err := cmd.Output()
	if err != nil {
		t.Fatalf("rastertopwg: %v\n%s", err, stderr.String())
	}
	pages, err := raster.Decode(pwg)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := UnmarshalHeader(ras[4:], byteOrder)
	// A5 is 420x595 pt, 1750x2479 at 300 dpi; its imageable area starts 12 pt (50 px) in from
	// the left and top.
	if len(pages) != 1 || pages[0].Width != 1750 || pages[0].Height != 2479 || pages[0].XDPI != 300 {
		t.Fatalf("PWG pages %d: %dx%d @%d", len(pages), pages[0].Width, pages[0].Height, pages[0].XDPI)
	}
	w, cw, ch := pages[0].Width, int(h.CupsWidth), int(h.CupsHeight)
	data := ras[4+HeaderSize:]
	for y := 0; y < pages[0].Height; y++ {
		row := pages[0].Gray[y*w : (y+1)*w]
		for x, v := range row {
			want := byte(255)
			if x >= 50 && x < 50+cw && y >= 50 && y < 50+ch {
				want = data[(y-50)*cw+x-50]
			}
			if v != want {
				t.Fatalf("pixel (%d,%d) = %d after the libcups round trip, want %d", x, y, v, want)
			}
		}
	}
}
