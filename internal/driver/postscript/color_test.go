package postscript

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

func writePPD(t *testing.T, colorDevice string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "printer.ppd")
	text := "*PPD-Adobe: \"4.3\"\n*ModelName: \"Test\"\n"
	if colorDevice != "" {
		text += "*ColorDevice: " + colorDevice + "\n"
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestColorOption(t *testing.T) {
	cases := []struct {
		color, ppd string
		want       bool
	}{
		{"", "", false},
		{"auto", "", false},
		{"off", "", false},
		{"on", "", true},
		{"auto", "True", true},
		{"", "True", true},
		{"auto", "False", false},
		{"auto", "none", false},
		{"off", "True", false},
	}
	for _, c := range cases {
		cfg := driver.Config{Color: c.color}
		if c.ppd != "" {
			cd := c.ppd
			if cd == "none" {
				cd = ""
			}
			cfg.PPD = writePPD(t, cd)
		}
		caps := newDriver(t, nil, cfg).Capabilities()
		drivertest.CheckCapabilities(t, caps)
		if caps.Color != c.want {
			t.Errorf("--color %q, ColorDevice %q: colour %v", c.color, c.ppd, caps.Color)
		}
		if c.want && (strings.Join(caps.URF, ",") != "V1.4,W8,SRGB24,CP99,IS1,MT1,RS300" || strings.Join(caps.PWGTypes, ",") != "sgray_8,srgb_8") {
			t.Errorf("colour URF %v PWG %v", caps.URF, caps.PWGTypes)
		}
	}
	if _, err := driver.New("postscript", driver.Config{Color: "maybe"}); err == nil {
		t.Error("--color maybe accepted")
	}
	if _, err := driver.New("postscript", driver.Config{PPD: filepath.Join(t.TempDir(), "missing.ppd")}); err == nil {
		t.Error("unreadable --ppd accepted")
	}
}

func rgbChart(m driver.Media, dpi int, landscape bool) raster.Page {
	w, h := (m.Width*dpi+1270)/2540, (m.Length*dpi+1270)/2540
	if landscape {
		w, h = h, w
	}
	return drivertest.RGBPage(w, h, dpi, func(x, y int) (byte, byte, byte) {
		switch {
		case y < h/4 && x < w/2:
			return 255, 0, 0
		case y < h/2:
			return byte(x * 255 / w), byte(y * 255 / h), 128
		case x < w/3:
			return 0, 0, 0
		}
		return 255, 255, 255
	})
}

// TestColorRoundTrip decodes colour documents: RGB pages are DeviceRGB images of the placed
// sheet, gray pages in the same document keep the gray image procedure.
func TestColorRoundTrip(t *testing.T) {
	for _, opts := range []map[string]string{nil, {"level": "3"}, {"encoding": "binary"}, {"depth": "8", "level": "3", "encoding": "binary"}} {
		d := newDriver(t, opts, driver.Config{Color: "on", Media: "a5"})
		job := drivertest.Job(t, d.Capabilities())
		if !job.Color {
			t.Fatal("default job not in colour")
		}
		pages := []raster.Page{rgbChart(job.Media, 300, false), rgbChart(job.Media, 300, true), textPage(job.Media)}
		var want [][]byte
		for _, p := range pages {
			sheet, err := imaging.Place(p, job.Media, imaging.PlaceOptions{XDPI: 300, YDPI: 300})
			if err != nil {
				t.Fatal(err)
			}
			if sheet.RGB != nil {
				want = append(want, sheet.RGB)
			} else if d.set.Depth == 8 {
				want = append(want, sheet.Gray)
			} else {
				bm, _, _ := imaging.Binarize(sheet, "")
				want = append(want, bm.Pix)
			}
		}
		res := drivertest.Run(t, d, job, pages...)
		def := "/aubrgb { % width height aubrgb -, rows of 8-bit R, G and B samples follow\n"
		if d.set.Level == 3 {
			def = "/aubrgb { % width height aubrgb -, 8-bit RGB samples follow\n"
		}
		for _, s := range []string{
			def,
			"  /DeviceRGB setcolorspace\n",
			"  << /ImageType 1 /Width w /Height h /BitsPerComponent 8 /Decode [0 1 0 1 0 1]\n",
			"\n1748 2480 aubrgb\n",
		} {
			if !bytes.Contains(res.Data, []byte(s)) {
				t.Errorf("%v: no %q", opts, s)
			}
		}
		doc, err := decodeDoc(res.Data)
		if err != nil {
			t.Fatalf("%v: %v", opts, err)
		}
		if len(doc.Images) != 3 {
			t.Fatalf("%v: %d images", opts, len(doc.Images))
		}
		for i, im := range doc.Images {
			if im.RGB != (i < 2) || im.W != 1748 || im.H != 2480 || !bytes.Equal(im.Data, want[i]) {
				t.Errorf("%v: page %d: %dx%d rgb %v depth %d, samples equal %v", opts, i+1, im.W, im.H, im.RGB, im.Depth, bytes.Equal(im.Data, want[i]))
			}
		}
		if !strings.Contains(res.Inspect[0], "page 1: image 1748x2480 depth 8 scaled") || !strings.Contains(res.Inspect[0], ", RGB\n") {
			t.Errorf("inspect %q", res.Inspect[0])
		}
		if opts == nil {
			drivertest.GoldenDigest(t, "a5-color.sha256", res.Data)
		}
	}
}

// A colour-enabled driver prints a job that is not in colour exactly as the gray driver does.
func TestColorDriverMonochromeUnchanged(t *testing.T) {
	for _, opts := range []map[string]string{nil, {"depth": "8", "duplex": "true", "pjl": "true"}} {
		gray := newDriver(t, opts)
		color := newDriver(t, opts, driver.Config{Color: "on"})
		job := drivertest.Job(t, color.Capabilities())
		job.ColorMode, job.Color = "monochrome", false
		job.Sides, job.Copies = "two-sided-long-edge", 3
		want := drivertest.Run(t, gray, job, textPage(job.Media), photoPage(job.Media)).Data
		got := drivertest.Run(t, color, job, textPage(job.Media), photoPage(job.Media)).Data
		if !bytes.Equal(got, want) {
			t.Errorf("%v: monochrome job differs from the gray driver", opts)
		}
		if bytes.Contains(got, []byte(procRGB)) {
			t.Errorf("%v: monochrome job defines the RGB procedure", opts)
		}
	}
}

// Every colour sheet is released once written, turned and scaled ones included.
func TestColorReleasesBudget(t *testing.T) {
	d := newDriver(t, nil, driver.Config{Color: "on"})
	job := drivertest.Job(t, d.Capabilities())
	job.Budget = driver.NewBudget(driver.MaxSheetBytes)
	drivertest.Run(t, d, job, rgbChart(job.Media, 300, false), rgbChart(job.Media, 300, true), rgbChart(job.Media, 150, false))
	if err := job.Budget.Charge(driver.MaxSheetBytes); err != nil {
		t.Errorf("budget not released: %v", err)
	}
}

// Flat colour compresses at level 2 as well as gray does: RunLengthDecode gets each row as an R,
// a G and a B row, since interleaved samples of a non-gray colour never repeat.
func TestColorLevel2FlatColourCompresses(t *testing.T) {
	d := newDriver(t, nil, driver.Config{Color: "on", Media: "a5"})
	job := drivertest.Job(t, d.Capabilities())
	w, h := (job.Media.Width*300+1270)/2540, (job.Media.Length*300+1270)/2540
	flat := drivertest.RGBPage(w, h, 300, func(x, y int) (byte, byte, byte) {
		if y < h/2 {
			return 200, 30, 90
		}
		return 10, 120, 240
	})
	sheet, err := imaging.Place(flat, job.Media, imaging.PlaceOptions{XDPI: 300, YDPI: 300})
	if err != nil {
		t.Fatal(err)
	}
	res := drivertest.Run(t, d, job, flat)
	if raw := 3 * w * h; len(res.Data) > raw/40 {
		t.Errorf("%d bytes for a flat colour page of %d bytes of RGB", len(res.Data), raw)
	}
	doc, err := decodeDoc(res.Data)
	if err != nil {
		t.Fatal(err)
	}
	if im := doc.Images[0]; !im.RGB || !bytes.Equal(im.Data, sheet.RGB) {
		t.Error("samples differ")
	}
}

// A dry run's Inspect checks every image but keeps none of their samples, only the summary.
func TestInspectKeepsNoSamples(t *testing.T) {
	d := newDriver(t, nil, driver.Config{Color: "on", Media: "a5"})
	job := drivertest.Job(t, d.Capabilities())
	res := drivertest.Run(t, d, job, rgbChart(job.Media, 300, false), textPage(job.Media))
	kept, err := decodeDoc(res.Data)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeImages(res.Data, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, im := range doc.Images {
		if im.Data != nil || im.Ink != kept.Images[i].Ink || im.Encoded != kept.Images[i].Encoded {
			t.Errorf("image %d: %d samples kept, ink %d (%d)", i+1, len(im.Data), im.Ink, kept.Images[i].Ink)
		}
	}
	bm := &imaging.Bitmap{W: kept.Images[1].W, H: kept.Images[1].H, Stride: (kept.Images[1].W + 7) / 8, Pix: kept.Images[1].Data}
	if kept.Images[1].Ink == 0 || kept.Images[1].Ink != bm.Ink() {
		t.Errorf("ink %d, bitmap has %d", kept.Images[1].Ink, bm.Ink())
	}
}
