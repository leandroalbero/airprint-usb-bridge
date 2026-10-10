package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ipp"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// One label may wait out a cooling pause after a TCP write of up to 2 min; a
// stop must not kill the process before that label is cut.
func TestShutdownTimeoutCoversOneLabel(t *testing.T) {
	tm := ql.DefaultTimeouts()
	oneMetre := tm.CompletedBase + 1000*tm.CompletedPerMM
	if worst := 2*time.Minute + tm.Cooling + oneMetre; shutdownTimeout <= worst {
		t.Fatalf("shutdownTimeout %s, one label may take %s", shutdownTimeout, worst)
	}
}

// A tiny document that expands to hundreds of MiB must be refused from its
// page header, before anything is decoded.
func TestPrintDocumentRefusesWrongWidthBeforeDecoding(t *testing.T) {
	o := defaultOptions(t)
	o.dryRun = t.TempDir()
	p, err := newPrinter(&o, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.send.Close()
	data := append([]byte("UNIRAST\x00\x00\x00\x00\x01"),
		testutil.URFPageHeader(testutil.Page{Width: 20000, Height: 26000, XDPI: 300, BPP: 8})...)
	for y := 0; y < 26000; y += 256 {
		data = append(data, 0xff, 0x80)
	}
	n, err := p.printDocument(context.Background(), data, p.jobInfo(driver.JobInfo{}, t.Logf, "bomb"), nil)
	if n != 0 || !errors.Is(err, ipp.ErrDocumentFormat) || !errors.Is(err, raster.ErrWidth) {
		t.Fatalf("printed %d, err %v", n, err)
	}
}

// A small document of tiny low-resolution pages becomes a whole sheet per page at the printer's
// resolution; the job must be refused once its sheets pass the budget, before anything is sent.
func TestPrintDocumentBoundsRenderedSheets(t *testing.T) {
	var pages []testutil.Page
	for i := 0; i < 800; i++ {
		g := make([]byte, 9*12)
		for j := range g {
			g[j] = byte(j * 2)
		}
		pages = append(pages, testutil.GrayPage(9, 12, 1, g))
	}
	data := testutil.EncodeURF(pages...)
	for _, drv := range []string{"pcl", "postscript", "zpl", "dymo"} {
		t.Run(drv, func(t *testing.T) {
			o := defaultOptions(t)
			o.driver, o.dryRun = drv, t.TempDir()
			p, err := newPrinter(&o, func(string, ...any) {})
			if err != nil {
				t.Fatal(err)
			}
			defer p.send.Close()
			n, err := p.printDocument(context.Background(), data, p.jobInfo(driver.JobInfo{}, func(string, ...any) {}, "big"), nil)
			if n != 0 || !errors.Is(err, driver.ErrJobTooLarge) || !errors.Is(err, ipp.ErrDocumentFormat) {
				t.Fatalf("printed %d, err %v", n, err)
			}
			if ents, _ := os.ReadDir(o.dryRun); len(ents) != 0 {
				t.Fatalf("%d files sent", len(ents))
			}
		})
	}
}

func TestPrintDocumentPageCap(t *testing.T) {
	o := defaultOptions(t)
	o.driver, o.dryRun = "pcl", t.TempDir()
	p, err := newPrinter(&o, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.send.Close()
	page := testutil.GrayPage(1, 1, 300, []byte{255})
	pages := make([]testutil.Page, maxPages+1)
	for i := range pages {
		pages[i] = page
	}
	_, err = p.printDocument(context.Background(), testutil.EncodeURF(pages...), p.jobInfo(driver.JobInfo{}, t.Logf, "many"), nil)
	if !errors.Is(err, raster.ErrTooLarge) || !errors.Is(err, ipp.ErrDocumentFormat) {
		t.Fatalf("err %v", err)
	}
}

func TestDebugOutputIsPrivate(t *testing.T) {
	o := defaultOptions(t)
	o.dryRun = filepath.Join(t.TempDir(), "dry")
	o.debugDir = filepath.Join(t.TempDir(), "debug")
	p, err := newPrinter(&o, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.send.Close()
	g := make([]byte, 732*40)
	for i := range g {
		g[i] = 255
	}
	for i := 732 * 10; i < 732*20; i++ {
		g[i] = 0
	}
	if _, err := p.printDocument(context.Background(), testutil.EncodeURF(testutil.GrayPage(732, 40, 300, g)), p.jobInfo(driver.JobInfo{}, t.Logf, "job1"), nil); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{o.dryRun, o.debugDir} {
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", dir, st.Mode(), err)
		}
		ents, _ := os.ReadDir(dir)
		if len(ents) == 0 {
			t.Errorf("%s is empty", dir)
		}
		for _, e := range ents {
			if st, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v %v", e.Name(), st.Mode(), err)
			}
		}
	}
}
