package pcl

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// A job that fails after pages were sent still ends with ESC E and the PJL end of job, so the
// printer finishes the sheet and leaves the job.
func TestStreamFailureEndsJob(t *testing.T) {
	d := newDriver(t, driver.Config{})
	job := jobFor(t, d, "")
	job.Name = "broken"
	page := func() raster.Page {
		return drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 300, Y0: 300, X1: 900, Y1: 400})
	}
	units, err := d.Plan(job, 3)
	if err != nil || len(units) != 1 || units[0].Impressions != 3 {
		t.Fatalf("plan %+v %v", units, err)
	}
	var buf bytes.Buffer
	gone := errors.New("spool: read error")
	err = d.Stream(context.Background(), job, units[0], drivertest.FailingPages(gone, page(), page()), &buf)
	if !errors.Is(err, gone) || !errors.Is(err, backend.ErrPartial) {
		t.Fatalf("err %v", err)
	}
	out := buf.Bytes()
	if n := bytes.Count(out, []byte("\x1b*r1A")); n != 2 {
		t.Errorf("%d pages sent, want 2", n)
	}
	if end := "\x1b*rB\f\x1bE" + uel + "@PJL EOJ NAME=\"broken\"\r\n" + uel; !bytes.HasSuffix(out, []byte(end)) {
		t.Errorf("job ends % x", out[max(0, len(out)-40):])
	}

	// Failing before the first page sends nothing at all.
	buf.Reset()
	err = d.Stream(context.Background(), job, units[0], drivertest.FailingPages(gone), &buf)
	if !errors.Is(err, gone) || errors.Is(err, backend.ErrPartial) || buf.Len() != 0 {
		t.Fatalf("first page: %v, %d bytes", err, buf.Len())
	}
}
