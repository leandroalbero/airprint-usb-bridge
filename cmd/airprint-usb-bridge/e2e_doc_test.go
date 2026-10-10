package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// testDoc is a document driver registered for these tests only: it writes one line per page
// naming the sheet it was given, and passes PDF through.
type testDoc struct{ caps driver.Capabilities }

func init() {
	driver.Register(driver.Info{
		Name: "test-doc",
		New: func(c driver.Config) (driver.Driver, error) {
			media, err := driver.Sheets([]string{driver.MediaA4, driver.MediaLetter}, driver.Margins{Top: 500, Bottom: 500, Left: 500, Right: 500}, "stationery", "main")
			if err != nil {
				return nil, err
			}
			media[0].Ready, media[1].Ready = true, true
			return testDoc{driver.Capabilities{
				MakeModel: "Test Document Printer", Kind: []string{"document"}, Duplex: true,
				Resolutions: []driver.Resolution{{X: 300, Y: 300}}, Qualities: []int{4},
				URF: []string{"V1.4", "W8", "DM1", "CP9", "RS300"}, PWGTypes: []string{"sgray_8"},
				Formats: []string{"application/pdf"}, MaxCopies: 9, Media: media,
			}}, nil
		},
	})
}

func (d testDoc) Name() string                      { return "test-doc" }
func (d testDoc) Capabilities() driver.Capabilities { return d.caps }
func (d testDoc) Render(_ context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	var b strings.Builder
	for i, p := range pages {
		fmt.Fprintf(&b, "page %d %dx%d on %s %s\n", i+1, p.Width, p.Height, job.Media.Name, job.Sides)
	}
	return []driver.Unit{{Data: []byte(b.String()), Label: "document", Impressions: len(pages)}}, nil
}
func (d testDoc) RenderDocument(_ context.Context, job driver.JobInfo, format string, data []byte) ([]driver.Unit, error) {
	return []driver.Unit{{Data: append([]byte(format+"\n"), data...)}}, nil
}

func TestEndToEndDocumentDriver(t *testing.T) {
	ts := startService(t, func(o *options) { o.driver = "test-doc" })
	doc := testutil.EncodePWG(
		testutil.GrayPage(2550, 3300, 300, grayPage(2550, 3300)),
		testutil.GrayPage(2550, 3300, 300, grayPage(2550, 3300)),
	)
	size := goipp.Collection{
		goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(21590)),
		goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(27940)),
	}
	m := ts.request(goipp.OpPrintJob, "ipp", formatAttr("image/pwg-raster"))
	m.Job.Add(goipp.MakeAttribute("media-col", goipp.TagBeginCollection,
		goipp.Collection{goipp.MakeAttribute("media-size", goipp.TagBeginCollection, size)}))
	m.Job.Add(goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(2)))
	m.Job.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("two-sided-long-edge")))
	resp := ts.do(t, "ipp", m, doc)
	job := ts.waitJob(t, "ipp", attrInt(t, resp.Job, "job-id"))
	if st := attrInt(t, job, "job-state"); st != 9 {
		t.Fatalf("job-state %d, reasons %v", st, attrStrings(job, "job-state-reasons"))
	}
	if n := attrInt(t, job, "job-impressions-completed"); n != 4 {
		t.Fatalf("job-impressions-completed %d, want 2 pages x 2 copies", n)
	}
	want := "page 1 2550x3300 on na_letter_8.5x11in two-sided-long-edge\npage 2 2550x3300 on na_letter_8.5x11in two-sided-long-edge\n"
	for _, n := range []int{1, 2} {
		if got := string(readBin(t, ts.dryDir, n)); got != want {
			t.Fatalf("job-%d.bin = %q", n, got)
		}
	}

	pdf := []byte("%PDF-1.7\n%%EOF\n")
	resp = ts.do(t, "ipp", ts.request(goipp.OpPrintJob, "ipp", formatAttr("application/octet-stream")), pdf)
	job = ts.waitJob(t, "ipp", attrInt(t, resp.Job, "job-id"))
	if st := attrInt(t, job, "job-state"); st != 9 {
		t.Fatalf("pdf job-state %d", st)
	}
	if got := string(readBin(t, ts.dryDir, 3)); got != "application/pdf\n"+string(pdf) {
		t.Fatalf("job-3.bin = %q", got)
	}
	if _, err := os.Stat(ts.dryDir + "/job-3.txt"); err == nil {
		t.Fatal("a driver without an Inspector got a .txt")
	}
}
