// Package drivertest is the test harness for drivers: synthetic pages, a JobInfo like the bridge
// builds, Run (capability checks, Render, IPP attribute dump) and golden files.
package drivertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ipp"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// Page builds a w x h gray page at dpi; draw returns each pixel (0 black, 255 white). A nil draw
// gives a white page.
func Page(w, h, dpi int, draw func(x, y int) byte) raster.Page {
	p := raster.Page{Width: w, Height: h, XDPI: dpi, YDPI: dpi, Gray: make([]byte, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(255)
			if draw != nil {
				v = draw(x, y)
			}
			p.Gray[y*w+x] = v
		}
	}
	return p
}

// RGBPage builds a w x h RGB page at dpi; draw returns each pixel's R, G and B. A nil draw
// gives a white page.
func RGBPage(w, h, dpi int, draw func(x, y int) (r, g, b byte)) raster.Page {
	p := raster.Page{Width: w, Height: h, XDPI: dpi, YDPI: dpi, RGB: make([]byte, 3*w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b := byte(255), byte(255), byte(255)
			if draw != nil {
				r, g, b = draw(x, y)
			}
			o := 3 * (y*w + x)
			p.RGB[o], p.RGB[o+1], p.RGB[o+2] = r, g, b
		}
	}
	return p
}

// Rect is a black rectangle in page pixels, X1 and Y1 exclusive.
type Rect struct{ X0, Y0, X1, Y1 int }

// SheetPage is a white page the size of m at dpi (as an iPhone sends it) with black rects. A
// roll (m.Length 0) gets length dots.
func SheetPage(m driver.Media, dpi, length int, rects ...Rect) raster.Page {
	w := (m.Width*dpi + 1270) / 2540
	h := length
	if m.Length > 0 {
		h = (m.Length*dpi + 1270) / 2540
	}
	return Page(w, h, dpi, func(x, y int) byte {
		for _, r := range rects {
			if x >= r.X0 && x < r.X1 && y >= r.Y0 && y < r.Y1 {
				return 0
			}
		}
		return 255
	})
}

// Job is the JobInfo the bridge passes for a job that sent no template attributes: one copy,
// default media, quality, colour mode (in colour on a colour printer), one-sided, logging to t.
func Job(t testing.TB, caps driver.Capabilities) driver.JobInfo {
	return driver.JobInfo{
		ID: 1, Name: "test", User: "tester", Format: "image/pwg-raster",
		Copies:    1,
		Media:     caps.Default(),
		Quality:   caps.DefaultQuality(),
		ColorMode: caps.DefaultColorMode(),
		Color:     caps.UseColor(caps.DefaultColorMode()),
		Sides:     "one-sided",
		Logf:      t.Logf,
	}
}

type Result struct {
	Units []driver.Unit
	Data  []byte // every unit's Data, in order
	// Attributes is the Get-Printer-Attributes answer for the driver's Capabilities, one
	// attribute per line (ipp.FormatAttributes).
	Attributes string
	TXT        map[string]string
	// Inspect is the Inspector's text for each unit, when the driver has one.
	Inspect []string
}

// Run checks d's Capabilities (CheckCapabilities), renders pages with job and returns the units
// with the IPP attribute dump. Any error fails t.
func Run(t testing.TB, d driver.Driver, job driver.JobInfo, pages ...raster.Page) Result {
	t.Helper()
	caps := d.Capabilities()
	CheckCapabilities(t, caps)
	var res Result
	res.Attributes, res.TXT = Attributes(t, caps)
	units, err := d.Render(context.Background(), job, pages)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	res.Units = units
	in, _ := d.(driver.Inspector)
	for _, u := range units {
		res.Data = append(res.Data, u.Data...)
		if in != nil {
			text, err := in.Inspect(u.Data)
			if err != nil {
				t.Errorf("Inspect(%s): %v", u.Label, err)
			}
			res.Inspect = append(res.Inspect, text)
		}
	}
	return res
}

// FailingPages is a PageReader over pages that fails with err after them, like a spool file
// that cannot be read any further.
func FailingPages(err error, pages ...raster.Page) driver.PageReader {
	return &failingPages{pages: pages, err: err}
}

type failingPages struct {
	pages []raster.Page
	err   error
}

func (f *failingPages) Next() (raster.Page, error) {
	if len(f.pages) == 0 {
		return raster.Page{}, f.err
	}
	p := f.pages[0]
	f.pages = f.pages[1:]
	return p, nil
}

// Attributes returns the IPP attribute dump and the DNS-SD TXT record for caps.
func Attributes(t testing.TB, caps driver.Capabilities) (string, map[string]string) {
	t.Helper()
	cfg := ipp.Config{Name: "Test Printer", UUID: "00000000-0000-4000-8000-000000000000", Caps: caps}
	attrs, err := ipp.PrinterAttributes(cfg)
	if err != nil {
		t.Fatalf("attributes: %v", err)
	}
	return ipp.FormatAttributes(attrs), cfg.TXT("printer.local", 8631)
}

// CheckCapabilities fails t when caps are invalid or miss what AirPrint / IPP Everywhere clients
// need: URF V1.4 and CP<n>, an RS token naming every resolution, W8 (and SRGB24 with srgb_8
// exactly when Color is set), DM1 for duplex, sgray_8, and at least one ready medium.
func CheckCapabilities(t testing.TB, caps driver.Capabilities) {
	t.Helper()
	if err := caps.Validate(); err != nil {
		t.Fatal(err)
	}
	urf := map[string]bool{}
	var rs []int
	for _, tok := range caps.URF {
		urf[tok] = true
		if strings.HasPrefix(tok, "RS") {
			for _, f := range strings.Split(tok[2:], "-") {
				n, err := strconv.Atoi(f)
				if err != nil {
					t.Errorf("URF %s: bad resolution", tok)
				}
				rs = append(rs, n)
			}
		}
	}
	for _, need := range []string{"V1.4", "W8"} {
		if !urf[need] {
			t.Errorf("URF lacks %s: %v", need, caps.URF)
		}
	}
	if !slices.ContainsFunc(caps.URF, isCopies) {
		t.Errorf("URF lacks CP<n> (copies): %v", caps.URF)
	}
	for _, r := range caps.Resolutions {
		if !slices.Contains(rs, r.X) || !slices.Contains(rs, r.Y) {
			t.Errorf("resolution %dx%d not in the URF RS token %v", r.X, r.Y, caps.URF)
		}
	}
	if caps.Color && (!urf["SRGB24"] || !slices.Contains(caps.PWGTypes, "srgb_8")) {
		t.Errorf("colour printer needs URF SRGB24 and PWG srgb_8: %v %v", caps.URF, caps.PWGTypes)
	}
	if !caps.Color && (urf["SRGB24"] || slices.Contains(caps.PWGTypes, "srgb_8")) {
		t.Errorf("gray printer advertises URF SRGB24 or PWG srgb_8: %v %v", caps.URF, caps.PWGTypes)
	}
	if caps.Duplex && !hasPrefix(caps.URF, "DM") {
		t.Errorf("duplex printer needs a URF DM token: %v", caps.URF)
	}
	if !slices.Contains(caps.PWGTypes, "sgray_8") {
		t.Errorf("PWGTypes lacks sgray_8: %v", caps.PWGTypes)
	}
	if len(caps.ReadyNames()) == 0 {
		t.Error("no ready media: set Roll or mark a Media Ready")
	}
	if slices.Contains(caps.Kind, "document") && caps.Roll != nil {
		t.Error("a document printer with a roll: use Kind label,roll")
	}
}

func isCopies(tok string) bool {
	n, err := strconv.Atoi(strings.TrimPrefix(tok, "CP"))
	return strings.HasPrefix(tok, "CP") && err == nil && n >= 1
}

func hasPrefix(list []string, p string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Golden compares got with testdata/<name> in the calling package's directory. With the
// environment variable UPDATE_GOLDEN=1 it writes the file instead.
func Golden(t testing.TB, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with UPDATE_GOLDEN=1 to create it)", err)
	}
	if !bytes.Equal(got, want) {
		i := 0
		for i < len(got) && i < len(want) && got[i] == want[i] {
			i++
		}
		t.Fatalf("%s differs at byte %d (got %d bytes, want %d); rerun with UPDATE_GOLDEN=1 if the change is intended",
			path, i, len(got), len(want))
	}
}

// GoldenDigest is Golden for outputs too large to keep: the file holds the SHA-256 and length.
func GoldenDigest(t testing.TB, name string, got []byte) {
	t.Helper()
	sum := sha256.Sum256(got)
	Golden(t, name, []byte(fmt.Sprintf("sha256:%s %d\n", hex.EncodeToString(sum[:]), len(got))))
}
