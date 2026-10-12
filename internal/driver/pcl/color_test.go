package pcl

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func colorDriver(t testing.TB, cfg driver.Config) *Driver {
	t.Helper()
	cfg.Color = "on"
	return newDriver(t, cfg)
}

// colorChart is a page the size of m at dpi with primaries, a gradient and black bars.
func colorChart(m driver.Media, dpi int, landscape bool) raster.Page {
	w, h := (m.Width*dpi+1270)/2540, (m.Length*dpi+1270)/2540
	if landscape {
		w, h = h, w
	}
	return drivertest.RGBPage(w, h, dpi, func(x, y int) (byte, byte, byte) {
		fx, fy := float64(x)/float64(w), float64(y)/float64(h)
		switch {
		case fy > 0.1 && fy < 0.2 && fx > 0.1 && fx < 0.3:
			return 255, 0, 0
		case fy > 0.1 && fy < 0.2 && fx > 0.4 && fx < 0.6:
			return 0, 255, 0
		case fy > 0.1 && fy < 0.2 && fx > 0.7 && fx < 0.9:
			return 0, 0, 255
		case fy > 0.3 && fy < 0.4 && fx > 0.1 && fx < 0.9:
			return byte(255 * fx), 128, byte(255 - 255*fx)
		case fy > 0.5 && fy < 0.52 && fx > 0.1 && fx < 0.6:
			return 0, 0, 0
		}
		return 255, 255, 255
	})
}

// wantColor is what the printer should image: the page placed at dpi and cropped to the
// logical page, as RGB rows.
func wantColor(t testing.TB, p raster.Page, job driver.JobInfo, dpi int) raster.Page {
	t.Helper()
	sheet, err := imaging.Place(p, job.Media, imaging.PlaceOptions{XDPI: dpi, YDPI: dpi, Orientation: job.Orientation, Scaling: job.Scaling})
	if err != nil {
		t.Fatal(err)
	}
	pp, _ := paperFor(job.Media.Name)
	off, lw := pp.logical(dpi)
	out := raster.Page{Width: lw, Height: sheet.Height, RGB: make([]byte, 0, 3*lw*sheet.Height)}
	for y := 0; y < sheet.Height; y++ {
		out.RGB = append(out.RGB, sheet.RGB[3*(y*sheet.Width+off):3*(y*sheet.Width+off+lw)]...)
	}
	return out
}

func compareColor(t testing.TB, n int, got pclPage, want raster.Page) {
	t.Helper()
	if !got.color || got.width != want.Width || got.compression != 3 {
		t.Fatalf("page %d: colour %v width %d compression %d, want colour width %d mode 3", n, got.color, got.width, got.compression, want.Width)
	}
	if len(got.rows) > want.Height {
		t.Fatalf("page %d: %d rows, sheet has %d", n, len(got.rows), want.Height)
	}
	stride := 3 * want.Width
	white := bytes.Repeat([]byte{255}, stride)
	for y := 0; y < want.Height; y++ {
		w := want.RGB[y*stride : (y+1)*stride]
		g := white
		if y < len(got.rows) {
			g = got.rows[y]
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("page %d: row %d differs", n, y)
		}
	}
}

func TestColorCapabilities(t *testing.T) {
	for _, c := range []string{"", "auto", "off"} {
		if caps := newDriver(t, driver.Config{Color: c}).Capabilities(); caps.Color || slices.Contains(caps.URF, "SRGB24") {
			t.Errorf("--color %q: colour %v URF %v", c, caps.Color, caps.URF)
		}
	}
	if _, err := New(driver.Config{Color: "yes"}); err == nil {
		t.Error("--color yes accepted")
	}
	d := colorDriver(t, driver.Config{Options: opts("duplex", "true")})
	caps := d.Capabilities()
	drivertest.CheckCapabilities(t, caps)
	if !caps.Color || caps.DefaultColorMode() != "auto" || strings.Join(caps.PWGTypes, ",") != "sgray_8,srgb_8" {
		t.Errorf("caps %+v", caps)
	}
	attrs, txt := drivertest.Attributes(t, caps)
	if txt["URF"] != "V1.4,W8,SRGB24,CP99,DM1,IS1,MT1,RS300" || txt["Color"] != "T" {
		t.Errorf("TXT %v", txt)
	}
	for _, want := range []string{
		"color-supported (boolean) = true\n",
		"print-color-mode-default (keyword) = auto\n",
		"print-color-mode-supported (1setOf keyword) = monochrome,auto,auto-monochrome,color\n",
		"pwg-raster-document-type-supported (1setOf keyword) = sgray_8,srgb_8\n",
		"urf-supported (1setOf keyword) = V1.4,W8,SRGB24,CP99,DM1,IS1,MT1,RS300\n",
	} {
		if !strings.Contains(attrs, want) {
			t.Errorf("attributes lack %q", want)
		}
	}
}

// TestColorHandChecked follows the PCL 5 Color Technical Reference: the short-form Configure Image
// Data command for device RGB, direct by pixel, 8 bits per primary, then full-width 24-bit rows in
// delta row compression.
func TestColorHandChecked(t *testing.T) {
	d := colorDriver(t, driver.Config{Media: "a5"})
	job := jobFor(t, d, "")
	if !job.Color {
		t.Fatal("default job is not in colour")
	}
	// The A5 logical page starts 71 dots in at 300 dpi and is 1598 dots wide. Row 60 (below the
	// 50-dot top margin) has one red dot at logical x 2; row 61 repeats it; nothing below.
	page := drivertest.RGBPage(1748, 2480, 300, func(x, y int) (byte, byte, byte) {
		if x == 73 && (y == 60 || y == 61) {
			return 255, 0, 0
		}
		return 255, 255, 255
	})
	res := render(t, d, job, page)
	head := "\x1bE\x1b&l25A\x1b&l0O\x1b&l0L\x1b&l0E\x1b*t300R\x1b*v6W\x00\x03\x00\x08\x08\x08" +
		"\x1b*p0x0Y\x1b*r1598S\x1b*b3M\x1b*r1A"
	if !bytes.Contains(res.Data, []byte(head)) {
		t.Fatalf("no colour prologue in %q", res.Data[:min(200, len(res.Data))])
	}
	// Row 0 against the zero seed is all 255: 4794 bytes in replacements of 8 (offset 0), the
	// last of 2. Rows 1-59 repeat it (ESC *b0W). Row 60 changes bytes 7 and 8 (G and B of dot
	// 2) to 0: command 0x27 (2 bytes at offset 7). Row 61 is the same: ESC *b0W.
	white := "\x1b*b5394W" + strings.Repeat("\xe0"+strings.Repeat("\xff", 8), 599) + "\x20\xff\xff"
	rows := white + strings.Repeat("\x1b*b0W", 59) + "\x1b*b3W\x27\x00\x00" + "\x1b*b0W" + "\x1b*rB\f\x1bE"
	if !bytes.Contains(res.Data, []byte(rows)) {
		t.Fatalf("colour rows differ:\n%q", res.Data[len(head):min(len(res.Data), len(head)+400)])
	}
	job2 := decodePCL(t, res.Data)
	compareColor(t, 1, job2.pages[0], wantColor(t, page, job, 300))
	text, err := Inspect(res.Data)
	if err != nil || !strings.Contains(text, "ESC *v6W (configure image data: 00 03 00 08 08 08; device RGB, direct by pixel, 24 bits)") ||
		!strings.Contains(text, "raster: 62 rows sent") {
		t.Errorf("inspect %v:\n%s", err, text)
	}
}

// TestColorPages decodes whole colour jobs: turned, scaled and gray pages, at 300 and 600 dpi.
func TestColorPages(t *testing.T) {
	for _, dpi := range []int{300, 600} {
		d := colorDriver(t, driver.Config{Options: opts("dpi", "300,600")})
		job := jobFor(t, d, "")
		job.Resolution = driver.Resolution{X: dpi, Y: dpi}
		caps := d.Capabilities()
		letter, _ := caps.LookupName(driver.MediaLetter)
		pages := []raster.Page{
			colorChart(job.Media, dpi, false),
			colorChart(job.Media, dpi, true),
			colorChart(letter, dpi, false),
			drivertest.SheetPage(job.Media, dpi, 0, drivertest.Rect{X0: 300, Y0: 300, X1: 900, Y1: 340}),
		}
		var want []raster.Page
		for _, p := range pages {
			if p.Gray != nil {
				p = rgbFromGray(p)
			}
			want = append(want, wantColor(t, p, job, dpi))
		}
		res := render(t, d, job, pages...)
		got := decodePCL(t, res.Data)
		if len(got.pages) != len(want) {
			t.Fatalf("%d dpi: %d pages", dpi, len(got.pages))
		}
		for i := range want {
			compareColor(t, i+1, got.pages[i], want[i])
		}
		if dpi == 300 {
			drivertest.GoldenDigest(t, "a4-color-4p.sha256", res.Data)
		}
	}
}

func rgbFromGray(p raster.Page) raster.Page {
	out := p
	out.Gray, out.RGB = nil, make([]byte, 3*len(p.Gray))
	for i, v := range p.Gray {
		out.RGB[3*i], out.RGB[3*i+1], out.RGB[3*i+2] = v, v, v
	}
	return out
}

// A colour-enabled driver prints a job that is not in colour exactly as the gray driver does.
func TestColorDriverMonochromeUnchanged(t *testing.T) {
	gray := newDriver(t, driver.Config{Options: opts("duplex", "true", "compression", "3")})
	color := colorDriver(t, driver.Config{Options: opts("duplex", "true", "compression", "3")})
	for _, mode := range []string{"monochrome", "auto-monochrome", "auto"} {
		job := jobFor(t, color, "")
		job.ColorMode, job.Color = mode, false
		job.Sides, job.Copies = "two-sided-long-edge", 2
		pages := []raster.Page{pattern(2480, 3508, 300), pattern(3508, 2480, 300)}
		want := render(t, gray, job, clonePages(pages)...).Data
		got := render(t, color, job, clonePages(pages)...).Data
		if !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, gray driver %d", mode, len(got), len(want))
		}
		// RGB pages in a gray job are averaged to gray first.
		rgb := []raster.Page{rgbFromGray(pages[0]), rgbFromGray(pages[1])}
		if got := render(t, color, job, rgb...).Data; !bytes.Equal(got, want) {
			t.Errorf("%s: RGB pages of gray content print differently", mode)
		}
	}
}

// Every colour sheet is released once sent, turned and scaled ones included.
func TestColorReleasesBudget(t *testing.T) {
	d := colorDriver(t, driver.Config{})
	job := jobFor(t, d, "")
	job.Budget = driver.NewBudget(driver.MaxSheetBytes)
	render(t, d, job, colorChart(job.Media, 300, false), colorChart(job.Media, 300, true), colorChart(job.Media, 150, false))
	if err := job.Budget.Charge(driver.MaxSheetBytes); err != nil {
		t.Errorf("budget not released: %v", err)
	}
}
