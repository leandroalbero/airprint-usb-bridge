package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ipp"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// a4Page is an A4 sheet at dpi with a few black blocks, as an iPhone would send it.
func a4Page(dpi int) testutil.Page {
	w, h := 2480*dpi/300, 3508*dpi/300
	g := bytes.Repeat([]byte{0xff}, w*h)
	for y := h / 10; y < h*9/10; y += h / 12 {
		for yy := y; yy < y+h/40; yy++ {
			for x := w / 8; x < w*7/8; x++ {
				if (x/(dpi/10))%3 != 0 {
					g[yy*w+x] = 0
				}
			}
		}
	}
	return testutil.GrayPage(w, h, dpi, g)
}

// photoPage is an A4 gradient at 300 dpi: dithered, so it compresses badly.
func photoPage() testutil.Page {
	w, h := 2480, 3508
	g := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g[y*w+x] = byte((x*255/w + y*255/h) / 2)
		}
	}
	return testutil.GrayPage(w, h, 300, g)
}

// writeURF writes a URF document of n copies of page to path, encoding the page once.
func writeURF(t *testing.T, path string, page testutil.Page, n int) {
	t.Helper()
	one := testutil.EncodeURF(page)[12:]
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hdr := []byte("UNIRAST\x00\x00\x00\x00\x00")
	binary.BigEndian.PutUint32(hdr[8:], uint32(n))
	if _, err := f.Write(hdr); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := f.Write(one); err != nil {
			t.Fatal(err)
		}
	}
}

func openDoc(t *testing.T, path string) *io.SectionReader {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	sec, err := docSection(f)
	if err != nil {
		t.Fatal(err)
	}
	return sec
}

// TestStreamLongJobBoundedMemory prints 200 A4 pages at 600 dpi (35 MB of gray each, 7 GB in
// all) through pcl in a dry run: the heap must stay around two pages.
func TestStreamLongJobBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("prints 200 pages at 600 dpi")
	}
	const pages = 200
	dir := t.TempDir()
	path := filepath.Join(dir, "long.urf")
	writeURF(t, path, a4Page(600), pages)

	o := defaultOptions(t)
	o.driver, o.dryRun = "pcl", filepath.Join(dir, "dry")
	o.driverOpts = []string{"dpi=600"}
	p, err := newPrinter(&o, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.send.Close()
	doc := openDoc(t, path)

	runtime.GC()
	var peak uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapInuse)
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	n, err := p.printDocument(context.Background(), doc, p.jobInfo(driver.JobInfo{Name: "long", Resolution: driver.Resolution{X: 600, Y: 600}}, func(string, ...any) {}, "long"), nil)
	close(stop)
	wg.Wait()
	if err != nil || n != pages {
		t.Fatalf("printed %d, %v", n, err)
	}
	t.Logf("%d pages in %v, peak heap in use %d MiB", n, time.Since(start).Round(time.Millisecond), peak>>20)
	if peak > 150<<20 {
		t.Errorf("peak heap in use %d MiB, want under 150 MiB", peak>>20)
	}
	out, err := os.ReadFile(filepath.Join(o.dryRun, "job-1.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(out, []byte("\x1b*r1A")); got != pages {
		t.Errorf("%d raster pages in the job, want %d", got, pages)
	}
	if !bytes.HasSuffix(out, []byte("\x1bE\x1b%-12345X@PJL EOJ NAME=\"long\"\r\n\x1b%-12345X")) {
		t.Errorf("job does not end with the PJL epilogue: % x", out[len(out)-32:])
	}
}

// A page that cannot be decoded refuses the whole streamed job before anything is sent, even
// when it is the last one.
func TestStreamRefusesBadPageBeforeSending(t *testing.T) {
	good := a4Page(300)
	doc := testutil.EncodeURF(good, good, good)
	doc = doc[:len(doc)-100]
	for _, drv := range []string{"pcl", "postscript"} {
		t.Run(drv, func(t *testing.T) {
			o := defaultOptions(t)
			o.driver, o.dryRun = drv, t.TempDir()
			p, err := newPrinter(&o, t.Logf)
			if err != nil {
				t.Fatal(err)
			}
			defer p.send.Close()
			n, err := p.printDocument(context.Background(), docOf(doc), p.jobInfo(driver.JobInfo{}, t.Logf, "bad"), nil)
			if n != 0 || !errors.Is(err, raster.ErrTruncated) || !errors.Is(err, ipp.ErrDocumentFormat) || !strings.Contains(err.Error(), "page 3") {
				t.Fatalf("printed %d, err %v", n, err)
			}
			if ents, _ := os.ReadDir(o.dryRun); len(ents) != 0 {
				t.Fatalf("%d files sent", len(ents))
			}
		})
	}
}

// failingSender fails the stream after limit bytes, the way a printer unplugged mid-job does.
type failingSender struct {
	driver.Sender
	limit    int
	attempts int
	got      bytes.Buffer
}

func (f *failingSender) SendStream(ctx context.Context, u driver.Unit, write func(io.Writer) error) error {
	f.attempts++
	f.got.Reset()
	return write(&limitWriter{f: f})
}

type limitWriter struct{ f *failingSender }

func (l *limitWriter) Write(p []byte) (int, error) {
	room := l.f.limit - l.f.got.Len()
	if len(p) <= room {
		return l.f.got.Write(p)
	}
	l.f.got.Write(p[:room])
	// Retryable on purpose: once bytes are out, even such an error must not cause a retry.
	return room, backend.Retryable(errors.New("write: no such device"))
}

// A failure after bytes reached the printer is never retried: the job aborts.
func TestStreamMidJobFailureIsNotRetried(t *testing.T) {
	o := defaultOptions(t)
	o.driver, o.dryRun = "pcl", t.TempDir()
	o.retryDeadline = time.Minute
	p, err := newPrinter(&o, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.send.Close()
	fs := &failingSender{Sender: p.send, limit: 5000}
	p.send = fs
	pg := a4Page(300)
	doc := testutil.EncodeURF(pg, pg, pg)
	start := time.Now()
	n, err := p.printDocument(context.Background(), docOf(doc), p.jobInfo(driver.JobInfo{}, t.Logf, "unplugged"), nil)
	if n != 0 || err == nil || fs.attempts != 1 || time.Since(start) > 20*time.Second {
		t.Fatalf("printed %d after %d attempts: %v", n, fs.attempts, err)
	}
	if !strings.Contains(err.Error(), "may have printed partially") {
		t.Errorf("err %v", err)
	}
}

// A job cancelled between pages still ends the device job, so the printer is not left waiting.
func TestStreamCancelEndsDeviceJob(t *testing.T) {
	cases := []struct {
		driver string
		opts   []string
		end    string
	}{
		{"pcl", nil, "\x1bE\x1b%-12345X@PJL EOJ NAME=\"cancel\"\r\n\x1b%-12345X"},
		{"postscript", []string{"pjl=true"}, "%%Trailer\n%%EOF\n\x1b%-12345X@PJL EOJ\r\n\x1b%-12345X"},
	}
	for _, c := range cases {
		t.Run(c.driver, func(t *testing.T) {
			o := defaultOptions(t)
			o.driver, o.dryRun, o.driverOpts = c.driver, t.TempDir(), c.opts
			p, err := newPrinter(&o, t.Logf)
			if err != nil {
				t.Fatal(err)
			}
			defer p.send.Close()
			pg := photoPage()
			doc := testutil.EncodeURF(pg, pg, pg, pg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			info := p.jobInfo(driver.JobInfo{Name: "cancel"}, t.Logf, "cancel")
			info.Logf = func(f string, a ...any) {
				if strings.HasPrefix(f, "page %d: ") && len(a) > 0 && a[0] == 2 && strings.Contains(f, "dots") {
					cancel()
				}
			}
			n, err := p.printDocument(ctx, docOf(doc), info, nil)
			if n != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("printed %d, %v", n, err)
			}
			out, rerr := os.ReadFile(filepath.Join(o.dryRun, "job-1.bin"))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !bytes.HasSuffix(out, []byte(c.end)) {
				t.Errorf("job ends % x", out[max(0, len(out)-48):])
			}
			if got := bytes.Count(out, []byte("showpage")) + bytes.Count(out, []byte("\x1b*rB\f")); got >= 4 {
				t.Errorf("all %d pages were sent", got)
			}
		})
	}
}

// Streaming printers take documents up to 512 MiB; label printers keep the 64 MiB cap (and the
// attributes that go with it).
func TestDocumentCapByDriver(t *testing.T) {
	for drv, want := range map[string][2]int64{
		"pcl":        {maxStreamDocumentBytes, maxStreamSpoolBytes},
		"postscript": {maxStreamDocumentBytes, maxStreamSpoolBytes},
		"brother-ql": {maxDocumentBytes, 0},
		"zpl":        {maxDocumentBytes, 0},
	} {
		o := defaultOptions(t)
		o.driver, o.dryRun, o.stateDir, o.mdns = drv, t.TempDir(), t.TempDir(), "off"
		svc, err := newService(&o, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		if got := [2]int64{svc.cfg.MaxDocumentBytes, svc.cfg.MaxSpoolBytes}; got != want {
			t.Errorf("%s: document and spool caps %v, want %v", drv, got, want)
		}
		svc.p.send.Close()
	}
}
