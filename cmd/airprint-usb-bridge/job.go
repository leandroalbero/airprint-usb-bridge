package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/brotherql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ipp"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

type logFunc func(format string, args ...any)

// Memory caps sized for a 1 GB board: 62 mm x 1 m at 600x600 dpi is 35 MB of
// gray, and label documents are a few MB at most. Every page of a job is decoded
// before the first label prints, so the gray cap bounds the decoded job (about 150
// labels of 62x100 mm at 300 dpi). What a driver renders is bounded separately by
// driver.MaxSheetBytes, because a tiny page still becomes a whole sheet.
// packaging/airprint-usb-bridge.service sets MemoryMax and GOMEMLIMIT to match.
const (
	maxDocumentBytes = 64 << 20
	maxGrayBytes     = 128 << 20
	maxPages         = 10000
)

// printer renders documents with a driver and sends the units.
type printer struct {
	drv      driver.Driver
	caps     driver.Capabilities
	send     driver.Sender
	target   backend.Target
	retry    time.Duration
	debugDir string
	logf     logFunc
}

func newPrinter(o *options, logf logFunc) (*printer, error) {
	target, err := backend.ParseTarget(o.device, o.dryRun, o.match)
	if err != nil {
		return nil, err
	}
	dither, err := parseDither(o.dither)
	if err != nil {
		return nil, err
	}
	opts, err := parseDriverOpts(o.driverOpts)
	if err != nil {
		return nil, err
	}
	name, dev, err := chooseDriver(o, target, logf)
	if err != nil {
		return nil, err
	}
	target = pinTarget(target, dev)
	if name != brotherql.Name && o.profile != "" {
		logf("warning: --profile applies to the brother-ql driver only; the %s driver ignores it", name)
	}
	if name == brotherql.Name {
		// The QL-specific flags predate --driver-opt; an explicit --driver-opt wins.
		for k, v := range map[string]string{"profile": o.profile, "media-policy": o.mediaPolicy, "presets": o.presets, "trim": o.trim} {
			if _, set := opts[k]; !set && v != "" {
				opts[k] = v
			}
		}
	}
	d, err := driver.New(name, driver.Config{
		Media:        o.media,
		Dither:       dither,
		HighRes:      o.highRes,
		PPD:          o.ppd,
		Options:      opts,
		DeviceID:     dev.DeviceID,
		USBVendorID:  dev.VendorID,
		USBProductID: dev.ProductID,
		Logf:         logf,
	})
	if err != nil {
		return nil, err
	}
	send, err := driver.NewSender(d, target)
	if err != nil {
		return nil, err
	}
	if o.debugDir != "" {
		if err := os.MkdirAll(o.debugDir, 0o700); err != nil {
			send.Close()
			return nil, err
		}
	}
	return &printer{
		drv:      d,
		caps:     d.Capabilities(),
		send:     send,
		target:   target,
		retry:    o.retryDeadline,
		debugDir: o.debugDir,
		logf:     logf,
	}, nil
}

// noun names what one impression is, for messages.
func (p *printer) noun() string {
	if slices.Contains(p.caps.Kind, "label") {
		return "label"
	}
	return "page"
}

// jobInfo fills the parts of a JobInfo that do not come from IPP.
func (p *printer) jobInfo(info driver.JobInfo, logf logFunc, debugPrefix string) driver.JobInfo {
	if info.Copies < 1 {
		info.Copies = 1
	}
	if info.Quality == 0 {
		info.Quality = p.caps.DefaultQuality()
	}
	if info.ColorMode == "" {
		info.ColorMode = p.caps.DefaultColorMode()
	}
	if info.Sides == "" {
		info.Sides = "one-sided"
	}
	if info.Media.Width == 0 {
		info.Media = p.caps.Default()
	}
	info.Logf = logf
	if info.Budget == nil {
		info.Budget = driver.NewBudget(driver.MaxSheetBytes)
	}
	if p.debugDir != "" {
		info.Debug = func(name string, img image.Image) {
			path := filepath.Join(p.debugDir, fmt.Sprintf("%s-%s.png", debugPrefix, name))
			if err := writePNG(path, img); err != nil {
				logf("debug: %v", err)
			}
		}
	}
	return info
}

// documentFormatError keeps the message and adds ipp.ErrDocumentFormat, so the job gets
// job-state-reasons document-format-error.
func documentFormatError(err error) error {
	return fmt.Errorf("%w: %w", ipp.ErrDocumentFormat, err)
}

func isRaster(format string) bool {
	switch format {
	case "", "image/urf", "image/pwg-raster", "application/octet-stream":
		return true
	}
	return false
}

// render converts the whole document before anything is sent, so a bad page never leaves a
// half-printed job.
func (p *printer) render(ctx context.Context, data []byte, info driver.JobInfo) ([]driver.Unit, error) {
	var units []driver.Unit
	var err error
	if isRaster(info.Format) {
		lim := raster.Limits{MaxGrayBytes: maxGrayBytes, MaxPages: maxPages}
		if pl, ok := p.drv.(driver.PageLimiter); ok {
			l := pl.PageLimits(info)
			lim.WidthMin, lim.WidthMax = l.WidthMin, l.WidthMax
		}
		pages, derr := raster.DecodeLimited(data, lim)
		if errors.Is(derr, raster.ErrNoPages) {
			info.Log("document has no pages; nothing to print")
			return nil, nil
		}
		if derr != nil {
			return nil, documentFormatError(derr)
		}
		for i, pg := range pages {
			info.Log("page %d: %dx%d @%dx%ddpi", i+1, pg.Width, pg.Height, pg.XDPI, pg.YDPI)
		}
		units, err = p.drv.Render(ctx, info, pages)
	} else {
		dr, ok := p.drv.(driver.DocumentRenderer)
		if !ok {
			return nil, documentFormatError(fmt.Errorf("driver %s cannot print %s", p.drv.Name(), info.Format))
		}
		units, err = dr.RenderDocument(ctx, info, info.Format, data)
	}
	if err != nil {
		if errors.Is(err, driver.ErrDocument) {
			return nil, documentFormatError(err)
		}
		return nil, err
	}
	if !p.caps.CopiesNative {
		units = driver.RepeatUnits(units, info.Copies)
	}
	return units, nil
}

// sendUnits sends each unit as its own device job, retrying only while nothing was sent.
// onUnit gets the running impression count after each unit. It returns that count.
func (p *printer) sendUnits(ctx context.Context, units []driver.Unit, logf logFunc, onUnit func(int)) (int, error) {
	impressions := 0
	for i, u := range units {
		if err := ctx.Err(); err != nil {
			return impressions, err
		}
		desc := u.Label
		if desc == "" {
			desc = fmt.Sprintf("unit %d of %d", i+1, len(units))
		}
		retryLog := func(s string) { logf("%s: %s", desc, s) }
		if err := backend.Retry(ctx, func(ctx context.Context) error { return p.send.Send(ctx, u) }, p.retry, retryLog); err != nil {
			return impressions, fmt.Errorf("%s: %w", desc, err)
		}
		impressions += max(u.Impressions, 1)
		logf("%s printed", desc)
		if onUnit != nil {
			onUnit(impressions)
		}
	}
	return impressions, nil
}

// printDocument renders a document, then sends it unit by unit.
func (p *printer) printDocument(ctx context.Context, data []byte, info driver.JobInfo, onUnit func(int)) (int, error) {
	units, err := p.render(ctx, data, info)
	if err != nil {
		return 0, err
	}
	return p.sendUnits(ctx, units, info.Logf, onUnit)
}

func (p *printer) processor() ipp.Processor {
	return func(ctx context.Context, job *ipp.Job, doc io.Reader, format string) error {
		jlog := func(f string, args ...any) {
			p.logf("job %d: "+f, append([]any{job.ID}, args...)...)
		}
		data, err := readDoc(doc)
		if err != nil {
			return err
		}
		a := job.Attrs
		mediaCol := "-"
		if mc := a.MediaCol; mc != nil {
			mediaCol = fmt.Sprintf("%dx%d", mc.XDimension, mc.YDimension)
			if mc.SizeName != "" {
				mediaCol += " " + mc.SizeName
			}
		}
		jlog("%q from %s: %s, %d bytes, media=%q media-col=%s print-quality=%d copies=%d sides=%s",
			job.Name, job.User, format, len(data), a.Media, mediaCol, a.PrintQuality, a.Copies, a.Sides)
		prefix := fmt.Sprintf("job%d", job.ID)
		if p.debugDir != "" {
			if err := os.WriteFile(filepath.Join(p.debugDir, prefix+"-in"+extension(format)), data, 0o600); err != nil {
				jlog("debug: %v", err)
			}
		}
		info := p.jobInfo(driver.JobInfo{
			ID: job.ID, Name: job.Name, User: job.User, Format: format,
			Copies: a.Copies, Media: a.Resolved, Quality: a.PrintQuality, ColorMode: a.ColorMode,
			Sides: a.Sides, Orientation: a.Orientation, Scaling: a.Scaling,
			Resolution:   driver.Resolution{X: a.XDPI, Y: a.YDPI},
			StateMessage: job.SetStateMessage,
		}, jlog, prefix)
		noun := p.noun()
		job.SetStateMessage("Converting.")
		n, err := p.printDocument(ctx, data, info, func(n int) {
			job.SetImpressions(n)
			job.SetStateMessage(fmt.Sprintf("Printed %d %s(s).", n, noun))
		})
		if err != nil {
			jlog("failed after %d %s(s): %v", n, noun, err)
			job.SetStateMessage(err.Error())
			return err
		}
		jlog("completed, %d %s(s)", n, noun)
		return nil
	}
}

func extension(format string) string {
	switch format {
	case "image/pwg-raster":
		return ".pwg"
	case "application/pdf":
		return ".pdf"
	case "application/postscript":
		return ".ps"
	}
	return ".urf"
}

// sniff names the format of a file given to the print command.
func sniff(data []byte) string {
	switch {
	case isPNG(data):
		return "image/png"
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return "application/pdf"
	case bytes.HasPrefix(data, []byte("%!")):
		return "application/postscript"
	case bytes.HasPrefix(data, []byte("RaS2")):
		return "image/pwg-raster"
	}
	return "image/urf"
}

// readDoc reads a spooled document in one allocation when its size is known;
// io.ReadAll would grow to twice a large document's size.
func readDoc(doc io.Reader) ([]byte, error) {
	if f, ok := doc.(interface{ Stat() (os.FileInfo, error) }); ok {
		if st, err := f.Stat(); err == nil && st.Mode().IsRegular() && st.Size() <= maxDocumentBytes {
			buf := make([]byte, st.Size())
			if _, err := io.ReadFull(doc, buf); err != nil {
				return nil, err
			}
			return buf, nil
		}
	}
	return io.ReadAll(doc)
}
