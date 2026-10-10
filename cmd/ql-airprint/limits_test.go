package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	defer p.be.Close()
	data := append([]byte("UNIRAST\x00\x00\x00\x00\x01"),
		testutil.URFPageHeader(testutil.Page{Width: 20000, Height: 26000, XDPI: 300, BPP: 8})...)
	for y := 0; y < 26000; y += 256 {
		data = append(data, 0xff, 0x80)
	}
	n, err := p.printDocument(context.Background(), data, false, t.Logf, "bomb", nil)
	if n != 0 || !errors.Is(err, ipp.ErrDocumentFormat) || !errors.Is(err, raster.ErrWidth) {
		t.Fatalf("printed %d, err %v", n, err)
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
	defer p.be.Close()
	g := make([]byte, 732*40)
	for i := range g {
		g[i] = 255
	}
	for i := 732 * 10; i < 732*20; i++ {
		g[i] = 0
	}
	if _, err := p.printDocument(context.Background(), testutil.EncodeURF(testutil.GrayPage(732, 40, 300, g)), false, t.Logf, "job1", nil); err != nil {
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
