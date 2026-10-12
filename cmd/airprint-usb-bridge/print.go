package main

import (
	"context"
	"fmt"
	"image"
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
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	doc, err := docSection(f)
	if err != nil {
		return err
	}
	head := make([]byte, 8)
	n, _ := doc.ReadAt(head, 0)
	p, err := newPrinter(o, logf)
	if err != nil {
		return err
	}
	defer p.send.Close()

	prefix := "print-" + time.Now().Format("20060102-150405")
	info := driver.JobInfo{Name: filepath.Base(path), User: "local", Format: sniff(head[:n])}
	if o.highRes {
		info.Quality = driver.QualityHigh
	}
	info = p.jobInfo(info, logf, prefix)
	noun := p.noun()

	if info.Format == "image/png" {
		img, err := png.Decode(io.NewSectionReader(doc, 0, doc.Size()))
		if err != nil {
			return fmt.Errorf("png: %w", err)
		}
		var units []driver.Unit
		if ir, ok := p.drv.(driver.ImageRenderer); ok {
			units, err = ir.RenderImage(ctx, info, img)
		} else {
			info.Format = "image/urf"
			res := p.caps.Resolutions[0]
			page := imaging.FromImage(img, res.X)
			color := false
			switch img.(type) {
			case *image.Gray, *image.Gray16:
			default:
				if info.Color {
					if rgb := imaging.FromImageRGB(img, res.X); !grayRGB(rgb.RGB) {
						page, color = rgb, true
					}
				}
			}
			// As for raster documents, an image without a colour pixel is the gray job.
			info.Color = color
			units, err = p.drv.Render(ctx, info, []raster.Page{page})
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, err := p.sendUnits(ctx, units, logf, nil, nil); err != nil {
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
		if err := copyFile(filepath.Join(p.debugDir, prefix+"-in"+ext), doc); err != nil {
			logf("debug: %v", err)
		}
	}
	printed, err := p.printDocument(ctx, doc, info, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "printed %d %s(s)\n", printed, noun)
	return nil
}

// grayRGB reports whether every RGB pixel has R, G and B equal.
func grayRGB(rgb []byte) bool {
	for i := 0; i+2 < len(rgb); i += 3 {
		if rgb[i] != rgb[i+1] || rgb[i+1] != rgb[i+2] {
			return false
		}
	}
	return true
}
