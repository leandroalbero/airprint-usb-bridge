package main

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// runPrint sends one file straight to the printer. URF and PWG go through the same pipeline as
// IPP jobs; a PNG goes to the driver's ImageRenderer (brother-ql prints it as one label,
// left-aligned on the roll) or else is rendered as one page at the default resolution.
func runPrint(ctx context.Context, o *options, path string, out io.Writer, logf logFunc) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	p, err := newPrinter(o, logf)
	if err != nil {
		return err
	}
	defer p.send.Close()

	prefix := "print-" + time.Now().Format("20060102-150405")
	info := driver.JobInfo{Name: filepath.Base(path), User: "local", Format: sniff(data)}
	if o.highRes {
		info.Quality = driver.QualityHigh
	}
	info = p.jobInfo(info, logf, prefix)
	noun := p.noun()

	if info.Format == "image/png" {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("png: %w", err)
		}
		var units []driver.Unit
		if ir, ok := p.drv.(driver.ImageRenderer); ok {
			units, err = ir.RenderImage(ctx, info, img)
		} else {
			info.Format = "image/urf"
			res := p.caps.Resolutions[0]
			units, err = p.drv.Render(ctx, info, []raster.Page{imaging.FromImage(img, res.X)})
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, err := p.sendUnits(ctx, units, logf, nil); err != nil {
			return err
		}
		if len(units) == 1 && units[0].Label != "" {
			fmt.Fprintf(out, "printed 1 %s (%s)\n", noun, units[0].Label)
		} else {
			fmt.Fprintf(out, "printed %d %s(s)\n", len(units), noun)
		}
		return nil
	}

	if p.debugDir != "" {
		ext := strings.ToLower(filepath.Ext(path))
		if err := os.WriteFile(filepath.Join(p.debugDir, prefix+"-in"+ext), data, 0o600); err != nil {
			logf("debug: %v", err)
		}
	}
	n, err := p.printDocument(ctx, data, info, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "printed %d %s(s)\n", n, noun)
	return nil
}
