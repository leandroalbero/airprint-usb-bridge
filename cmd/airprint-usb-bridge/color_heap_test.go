//go:build !race

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// writeURFPages writes a URF document of n pages: the given pages first, then copies of the last.
func writeURFPages(t *testing.T, path string, n int, pages ...testutil.Page) {
	t.Helper()
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
	var last []byte
	for i := 0; i < n; i++ {
		if i < len(pages) {
			last = testutil.EncodeURF(pages[i])[12:]
		}
		if _, err := f.Write(last); err != nil {
			t.Fatal(err)
		}
	}
}

// letterPage is a US Letter page at dpi: on A4 it is scaled to fit.
func letterPage(dpi int) testutil.Page {
	p := rgbA4Page(dpi, false)
	w, h := 2550*dpi/300, 3300*dpi/300
	pix := bytes.Repeat([]byte{0xff}, 3*w*h)
	for y := 0; y < min(h, p.Height); y++ {
		copy(pix[3*y*w:3*y*w+3*min(w, p.Width)], p.Pix[3*y*p.Width:])
	}
	return testutil.Page{Width: w, Height: h, XDPI: dpi, BPP: 24, ColorSpace: testutil.URFRGB, Pix: pix}
}

// TestColorLongJobBoundedMemory prints 200 colour A4 pages at 600 dpi (104 MB of RGB each, one
// of them turned and one scaled) through each colour driver in a dry run, with the service's
// GOMEMLIMIT of 384 MiB. The live heap must stay around two colour pages (the decoded page and
// one sheet), and the heap in use under the limit. It moves about 60 GB of RGB, far too slow
// under the race detector, so this file is left out of race builds.
func TestColorLongJobBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("prints 600 colour pages at 600 dpi")
	}
	const pages = 200
	dir := t.TempDir()
	path := filepath.Join(dir, "long.urf")
	writeURFPages(t, path, pages, rgbA4Page(600, true), letterPage(600), rgbA4Page(600, false))
	colorJobMemory(t, path, pages)
}

// rgbSheet is a w x h page of colour bars, as rgbA4Page.
func rgbSheet(w, h, dpi int) testutil.Page {
	pix := bytes.Repeat([]byte{0xff}, 3*w*h)
	for i, y := 0, h/10; y < h*9/10; i, y = i+1, y+h/12 {
		c := [][3]byte{{255, 0, 0}, {0, 160, 0}, {0, 0, 255}, {0, 0, 0}}[i%4]
		for yy := y; yy < y+h/40; yy++ {
			for x := w / 8; x < w*7/8; x++ {
				copy(pix[3*(yy*w+x):], c[:])
			}
		}
	}
	return testutil.Page{Width: w, Height: h, XDPI: dpi, BPP: 24, ColorSpace: testutil.URFRGB, Pix: pix}
}

// TestColorGrowingPagesBoundedMemory prints standard page sizes that grow and shrink (Letter,
// A4, Legal landscape, Letter landscape, A4 landscape, Legal at 600 dpi, twice): a larger page
// must not leave the smaller page buffer live beside it.
func TestColorGrowingPagesBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("prints 36 colour pages at 600 dpi")
	}
	sizes := [][2]int{{5100, 6600}, {4960, 7016}, {8400, 5100}, {6600, 5100}, {7016, 4960}, {5100, 8400}}
	var cycle []testutil.Page
	for _, z := range sizes {
		cycle = append(cycle, rgbSheet(z[0], z[1], 600))
	}
	path := filepath.Join(t.TempDir(), "grow.urf")
	writeURFPages(t, path, 2*len(cycle), append(cycle, cycle...)...)
	colorJobMemory(t, path, 2*len(cycle))
}

// colorJobMemory prints the colour document at path through each colour driver in a dry run, with
// the service's GOMEMLIMIT of 384 MiB. The live heap must stay around the largest decoded page
// and one sheet, and the heap in use under the limit.
func colorJobMemory(t *testing.T, path string, pages int) {
	dir := t.TempDir()
	discard := filepath.Join(dir, "discard")
	if err := os.WriteFile(discard, []byte("#!/bin/sh\ncat >/dev/null; printf done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mod  func(o *options)
	}{
		{"pcl", func(o *options) { o.driver, o.driverOpts = "pcl", []string{"dpi=600"} }},
		{"postscript", func(o *options) {
			o.driver, o.driverOpts = "postscript", []string{"resolution=600", "level=3", "encoding=binary"}
		}},
		{"cups", func(o *options) {
			o.driver, o.ppd, o.driverOpts = "cups", colorPPD(t), []string{"filter=" + discard}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := defaultOptions(t)
			o.color, o.dryRun = "on", filepath.Join(dir, c.name)
			c.mod(&o)
			p, err := newPrinter(&o, func(string, ...any) {})
			if err != nil {
				t.Fatal(err)
			}
			defer p.send.Close()
			if !p.caps.Color {
				t.Fatal("printer is not colour")
			}
			doc := openDoc(t, path)
			defer debug.SetMemoryLimit(debug.SetMemoryLimit(384 << 20))
			runtime.GC()
			var peak, live uint64
			sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				var ms runtime.MemStats
				for {
					runtime.ReadMemStats(&ms)
					peak = max(peak, ms.HeapInuse)
					metrics.Read(sample)
					live = max(live, sample[0].Value.Uint64())
					select {
					case <-stop:
						return
					case <-time.After(2 * time.Millisecond):
					}
				}
			}()
			start := time.Now()
			colour := false
			info := p.jobInfo(driver.JobInfo{Name: "long", Resolution: driver.Resolution{X: 600, Y: 600}}, func(f string, a ...any) {
				if f == "printing in colour" {
					colour = true
				}
			}, "long")
			n, err := p.printDocument(context.Background(), doc, info, nil)
			close(stop)
			wg.Wait()
			if err != nil || n != pages || !colour {
				t.Fatalf("printed %d in colour %v: %v", n, colour, err)
			}
			t.Logf("%s: %d colour pages in %v, peak live heap %d MiB, peak heap in use %d MiB",
				c.name, n, time.Since(start).Round(time.Millisecond), live>>20, peak>>20)
			if live > 240<<20 || peak > 384<<20 {
				t.Errorf("peak live heap %d MiB, in use %d MiB: want under 240 and 384 MiB", live>>20, peak>>20)
			}
			os.RemoveAll(o.dryRun)
		})
	}
}
