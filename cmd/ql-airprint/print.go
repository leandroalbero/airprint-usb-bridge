package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leandroalbero/ql-airprint/internal/backend"
)

// runPrint sends one file straight to the printer. A PNG is printed as one label, left-aligned
// on the roll; URF and PWG go through the same pipeline as IPP jobs.
func runPrint(ctx context.Context, o *options, path string, out io.Writer, logf logFunc) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	p, err := newPrinter(o, logf)
	if err != nil {
		return err
	}
	defer p.be.Close()

	if isPNG(data) {
		bmp, err := readPNG(data)
		if err != nil {
			return err
		}
		if bmp.W > p.media.PrintPins {
			return fmt.Errorf("%s is %d dots wide; %s mm media prints %d", path, bmp.W, p.media.ID, p.media.PrintPins)
		}
		job := backend.JobFor(p.prof, p.media, bmp, o.highRes)
		if err := backend.PrintWithRetry(ctx, p.be, job, p.retry, func(s string) { logf("%s", s) }); err != nil {
			return err
		}
		fmt.Fprintf(out, "printed 1 label (%dx%d)\n", bmp.W, bmp.H)
		return nil
	}

	prefix := "print-" + time.Now().Format("20060102-150405")
	if p.debugDir != "" {
		ext := strings.ToLower(filepath.Ext(path))
		if err := os.WriteFile(filepath.Join(p.debugDir, prefix+"-in"+ext), data, 0o600); err != nil {
			logf("debug: %v", err)
		}
	}
	n, err := p.printDocument(ctx, data, o.highRes, logf, prefix, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "printed %d label(s)\n", n)
	return nil
}
