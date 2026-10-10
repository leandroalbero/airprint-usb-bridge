package postscript

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// A document that fails after pages were sent still gets its trailer and the PJL end of job.
func TestStreamFailureEndsDocument(t *testing.T) {
	d := newDriver(t, map[string]string{"pjl": "true"})
	job := drivertest.Job(t, d.Capabilities())
	page := func() raster.Page {
		// A gradient is dithered: a page of it fills the output buffer.
		return drivertest.Page(2480, 3508, 300, func(x, y int) byte { return byte((x/10 + y/14) % 256) })
	}
	units, err := d.Plan(job, 3)
	if err != nil || len(units) != 1 {
		t.Fatalf("plan %+v %v", units, err)
	}
	var buf bytes.Buffer
	gone := errors.New("spool: read error")
	err = d.Stream(context.Background(), job, units[0], drivertest.FailingPages(gone, page(), page()), &buf)
	if !errors.Is(err, gone) || !errors.Is(err, backend.ErrPartial) {
		t.Fatalf("err %v", err)
	}
	out := buf.Bytes()
	if n := bytes.Count(out, []byte("showpage\n")); n != 2 {
		t.Errorf("%d pages sent, want 2", n)
	}
	if end := "showpage\n%%Trailer\n%%EOF\n" + uel + "@PJL EOJ\r\n" + uel; !bytes.HasSuffix(out, []byte(end)) {
		t.Errorf("document ends %q", out[max(0, len(out)-60):])
	}

	// A failure while everything is still buffered sends nothing.
	buf.Reset()
	small := drivertest.SheetPage(job.Media, 300, 0)
	err = d.Stream(context.Background(), job, units[0], drivertest.FailingPages(gone, small), &buf)
	if !errors.Is(err, gone) || errors.Is(err, backend.ErrPartial) || buf.Len() != 0 {
		t.Fatalf("buffered: %v, %d bytes", err, buf.Len())
	}
}

// The streamed pass-through equals the document with its wrapper, read in pieces from the file.
func TestStreamDocumentLarge(t *testing.T) {
	d := newDriver(t, map[string]string{"passthrough": "true", "pjl": "true", "duplex": "true"})
	job := drivertest.Job(t, d.Capabilities())
	job.Copies = 2
	body := bytes.Repeat([]byte("0 0 moveto (x) show\n"), 20000)
	src := append([]byte("\x04\x04%!PS-Adobe-3.0\n%%Pages: 7\n"), body...)
	units, err := d.RenderDocument(context.Background(), job, formatPS, src)
	if err != nil || len(units) != 2 || units[0].Impressions != 7 {
		t.Fatalf("units %d %v", len(units), err)
	}
	want := uel + "@PJL JOB\r\n@PJL ENTER LANGUAGE = POSTSCRIPT\r\n%!PS\n[{ << /Duplex false >> setpagedevice } stopped cleartomark\n" +
		string(src[2:]) + uel + "@PJL EOJ\r\n" + uel
	if string(units[0].Data) != want || string(units[1].Data) != want {
		t.Fatalf("document differs: %d bytes, want %d", len(units[0].Data), len(want))
	}
}
