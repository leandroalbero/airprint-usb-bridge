package drivertest_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// pbmDriver is the smallest useful document driver: each page is placed on the job's sheet at
// 300 dpi, binarized, and written as a binary PBM image. Real drivers have the same shape.
type pbmDriver struct{ caps driver.Capabilities }

func newPBM(t *testing.T) *pbmDriver {
	margins := driver.Margins{Top: 423, Bottom: 423, Left: 423, Right: 423}
	media, err := driver.Sheets([]string{driver.MediaA4, driver.MediaLetter, driver.MediaA5}, margins, "stationery", "main")
	if err != nil {
		t.Fatal(err)
	}
	media[0].Ready = true
	return &pbmDriver{driver.Capabilities{
		MakeModel:   "Example PBM",
		Kind:        []string{"document"},
		Resolutions: []driver.Resolution{{X: 300, Y: 300}},
		Qualities:   []int{driver.QualityNormal},
		URF:         []string{"V1.4", "W8", "CP1", "IS1", "MT1", "RS300"},
		PWGTypes:    []string{"sgray_8"},
		MaxCopies:   9,
		Media:       media,
	}}
}

func (d *pbmDriver) Name() string                      { return "pbm" }
func (d *pbmDriver) Capabilities() driver.Capabilities { return d.caps }
func (d *pbmDriver) Inspect(data []byte) (string, error) {
	return fmt.Sprintf("%d PBM pages", bytes.Count(data, []byte("P4\n"))), nil
}

func (d *pbmDriver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	var out bytes.Buffer
	for i, p := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sheet, err := imaging.Place(p, job.Media, imaging.PlaceOptions{XDPI: 300, YDPI: 300, Scaling: job.Scaling, Orientation: job.Orientation})
		if err != nil {
			return nil, driver.DocumentError(fmt.Errorf("page %d: %w", i+1, err))
		}
		pages[i].Gray = nil
		bm, dithered, err := imaging.Binarize(sheet, imaging.DitherAuto)
		if err != nil {
			return nil, err
		}
		job.Log("page %d: %dx%d dots, dithered %v", i+1, bm.W, bm.H, dithered)
		job.DebugImage(fmt.Sprintf("p%d", i+1), bm.Image())
		fmt.Fprintf(&out, "P4\n%d %d\n", bm.W, bm.H)
		out.Write(bm.Pix)
	}
	return []driver.Unit{{Data: out.Bytes(), Label: "job", Impressions: len(pages)}}, nil
}

func TestExampleDocumentDriver(t *testing.T) {
	d := newPBM(t)
	job := drivertest.Job(t, d.Capabilities())
	if job.Media.Name != driver.MediaA4 {
		t.Fatalf("default media %+v", job.Media)
	}
	pages := []raster.Page{
		drivertest.SheetPage(job.Media, 150, 0, drivertest.Rect{X0: 100, Y0: 100, X1: 400, Y1: 140}),
		drivertest.SheetPage(job.Media, 300, 0),
	}
	res := drivertest.Run(t, d, job, pages...)
	if len(res.Units) != 1 || res.Units[0].Impressions != 2 || res.Inspect[0] != "2 PBM pages" {
		t.Fatalf("units %+v inspect %v", len(res.Units), res.Inspect)
	}
	if !strings.HasPrefix(string(res.Data), "P4\n2480 3508\n") {
		t.Fatalf("data starts %q", res.Data[:16])
	}
	drivertest.GoldenDigest(t, "example-pbm.sha256", res.Data)
	drivertest.Golden(t, "example-attributes.golden", []byte(res.Attributes))
	if res.TXT["URF"] != "V1.4,W8,CP1,IS1,MT1,RS300" || res.TXT["pdl"] != "image/pwg-raster,image/urf" {
		t.Fatalf("TXT %v", res.TXT)
	}
	if !strings.Contains(res.Attributes, "printer-kind (keyword) = document\n") ||
		!strings.Contains(res.Attributes, "media-ready (keyword) = iso_a4_210x297mm\n") {
		t.Fatalf("attributes:\n%s", res.Attributes)
	}
}

type captureT struct {
	testing.TB
	errs []string
}

func (c *captureT) Errorf(f string, a ...any) { c.errs = append(c.errs, fmt.Sprintf(f, a...)) }
func (c *captureT) Error(a ...any)            { c.errs = append(c.errs, fmt.Sprint(a...)) }
func (c *captureT) Helper()                   {}

func TestCheckCapabilitiesComplains(t *testing.T) {
	caps := newPBM(t).Capabilities()
	caps.URF = []string{"V1.4", "W8", "RS600"}
	caps.Color, caps.Duplex = true, true
	caps.Media[0].Ready = false
	c := &captureT{TB: t}
	drivertest.CheckCapabilities(c, caps)
	got := strings.Join(c.errs, "\n")
	for _, want := range []string{"CP<n>", "300x300", "SRGB24", "DM", "no ready media"} {
		if !strings.Contains(got, want) {
			t.Errorf("no complaint about %s in:\n%s", want, got)
		}
	}

	ok := newPBM(t).Capabilities()
	ok.URF = []string{"V1.4", "W8", "CP99", "DM1", "RS300"}
	ok.Duplex = true
	c = &captureT{TB: t}
	drivertest.CheckCapabilities(c, ok)
	if len(c.errs) != 0 {
		t.Errorf("CP99 duplex caps rejected: %v", c.errs)
	}
}
