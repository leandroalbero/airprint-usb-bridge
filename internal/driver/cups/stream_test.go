package cups

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// When a page fails mid-job, the filter's input ends after the pages before it: the filter
// finishes the device job for them, its output still reaches the printer, and the job fails.
func TestStreamFailureClosesFilterInput(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: fakePPD(t, "")})
	job := drivertest.Job(t, d.Capabilities())
	units, err := d.Plan(job, 3)
	if err != nil || len(units) != 1 {
		t.Fatalf("plan %+v %v", units, err)
	}
	page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 100, Y0: 100, X1: 400, Y1: 200})
	var buf bytes.Buffer
	gone := errors.New("spool: read error")
	start := time.Now()
	err = d.Stream(context.Background(), job, units[0], drivertest.FailingPages(gone, page), &buf)
	if !errors.Is(err, gone) || !errors.Is(err, backend.ErrPartial) || time.Since(start) > 20*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	_, _, ras := splitEcho(t, buf.Bytes())
	h, _, err := d.header(d.sizeFor(job.Media), d.res[0], "one-sided")
	if err != nil {
		t.Fatal(err)
	}
	if want := 4 + HeaderSize + int(h.CupsBytesPerLine*h.CupsHeight); len(ras) != want || !bytes.Equal(ras[:4], syncV3()) {
		t.Fatalf("filter passed on %d bytes of raster, want one page of %d", len(ras), want)
	}

	// Even with no page, the filter ends its (empty) job; the error is still the page's.
	buf.Reset()
	if err := d.Stream(context.Background(), job, units[0], drivertest.FailingPages(gone), &buf); !errors.Is(err, gone) {
		t.Fatalf("no page: %v", err)
	}
}

// Cancelling a job ends the filter's input at a page boundary instead of killing the filter, so
// the device job ending it writes after its input still reaches the printer.
func TestStreamCancelLetsFilterEndJob(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: fakePPD(t, ""), Options: map[string]string{"options": "TestMode=trailer"}})
	job := drivertest.Job(t, d.Capabilities())
	units, err := d.Plan(job, 3)
	if err != nil || len(units) != 1 {
		t.Fatalf("plan %+v %v", units, err)
	}
	page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 100, Y0: 100, X1: 400, Y1: 200})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &syncWriter{}
	pages := &cancelPages{PageReader: drivertest.FailingPages(io.EOF, page, page, page), w: w, cancel: cancel}
	err = d.Stream(ctx, job, units[0], pages, w)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, backend.ErrPartial) {
		t.Fatalf("err %v", err)
	}
	out := w.bytes()
	ras, ok := bytes.CutSuffix(out, []byte("JOB-END"))
	if !ok {
		t.Fatalf("the filter's job ending was not passed on after the cancel (%d bytes)", len(out))
	}
	h, _, err := d.header(d.sizeFor(job.Media), d.res[0], "one-sided")
	if err != nil {
		t.Fatal(err)
	}
	if want := 4 + 2*(HeaderSize+int(h.CupsBytesPerLine*h.CupsHeight)); len(ras) != want {
		t.Fatalf("filter got %d bytes of raster, want the two pages read before the cancel (%d)", len(ras), want)
	}
}

type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	n   atomic.Int64
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n.Add(int64(len(p)))
	return w.buf.Write(p)
}

func (w *syncWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buf.Bytes())
}

// cancelPages cancels the job while page 2 is being read, once output has reached w.
type cancelPages struct {
	driver.PageReader
	w      *syncWriter
	cancel context.CancelFunc
	read   int
}

func (p *cancelPages) Next() (raster.Page, error) {
	p.read++
	if p.read == 2 {
		deadline := time.Now().Add(10 * time.Second)
		for p.w.n.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		p.cancel()
	}
	return p.PageReader.Next()
}

// The filter's output reaches the printer while the filter is still reading pages.
func TestStreamPassesOutputOn(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: fakePPD(t, ""), Options: map[string]string{"filter": "-"}})
	job := drivertest.Job(t, d.Capabilities())
	units, err := d.Plan(job, 2)
	if err != nil {
		t.Fatal(err)
	}
	page := drivertest.SheetPage(job.Media, 300, 0)
	w := &firstWrite{}
	pages := &watchPages{PageReader: drivertest.FailingPages(io.EOF, page, page), w: w}
	if err := d.Stream(context.Background(), job, units[0], pages, w); err != nil || w.n == 0 {
		t.Fatalf("err %v, %d bytes", err, w.n)
	}
	if !pages.sawOutput {
		t.Fatal("page 2 was read before anything was written")
	}
}

type firstWrite struct{ n int }

func (f *firstWrite) Write(p []byte) (int, error) { f.n += len(p); return len(p), nil }

type watchPages struct {
	driver.PageReader
	w         *firstWrite
	read      int
	sawOutput bool
}

func (p *watchPages) Next() (rp raster.Page, err error) {
	p.read++
	if p.read == 2 && p.w.n > 0 {
		p.sawOutput = true
	}
	return p.PageReader.Next()
}

// The timeout is for a filter that makes no progress: one that keeps writing may take longer.
func TestFilterTimeoutIsIdleTime(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: fakePPD(t, ""), Options: map[string]string{"options": "TestMode=trickle", "timeout": "1500ms"}})
	job := drivertest.Job(t, d.Capabilities())
	start := time.Now()
	units, err := d.Render(context.Background(), job, []raster.Page{drivertest.SheetPage(job.Media, 100, 0)})
	if err != nil || len(units) != 1 || string(units[0].Data) != "xxxxxxxxxx" {
		t.Fatalf("%v %+v", err, units)
	}
	if time.Since(start) < 1900*time.Millisecond {
		t.Fatalf("finished in %v", time.Since(start))
	}
}
