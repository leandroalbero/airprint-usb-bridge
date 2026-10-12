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
// gray, and label documents are a few MB at most. A label driver gets every page of
// a job decoded before the first label prints, so for it the gray cap bounds the
// decoded job (about 150 labels of 62x100 mm at 300 dpi). A streaming (document)
// driver holds one page at a time, so for it the gray cap is per page and the
// document only costs spool space. What a driver renders is bounded separately by
// driver.MaxSheetBytes, because a tiny page still becomes a whole sheet.
// packaging/airprint-usb-bridge.service sets MemoryMax and GOMEMLIMIT to match.
const (
	maxDocumentBytes       = 64 << 20
	maxStreamDocumentBytes = 512 << 20
	maxStreamSpoolBytes    = 1 << 30
	maxGrayBytes           = 128 << 20
	maxPages               = 10000
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
	maxGray  int // the decoder's budget, maxGrayBytes unless a test lowers it
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
	colorFlag, err := parseColor(o.color)
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
		Color:        colorFlag,
		Options:      opts,
		DeviceID:     dev.DeviceID,
		USBVendorID:  dev.VendorID,
		USBProductID: dev.ProductID,
		Logf:         logf,
	})
	if err != nil {
		return nil, err
	}
	if colorFlag == "on" && !d.Capabilities().Color {
		logf("warning: --color on: the %s driver prints in black only", name)
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
		maxGray:  maxGrayBytes,
	}, nil
}

// streamer is the driver and sender pair that prints a raster job page by page, if the driver
// supports it.
func (p *printer) streamer() (driver.Streamer, driver.StreamSender, bool) {
	s, ok := p.drv.(driver.Streamer)
	ss, sok := p.send.(driver.StreamSender)
	return s, ss, ok && sok
}

// streams reports whether documents go to the printer as they are read, so a document may be
// as large as the spool allows.
func (p *printer) streams() bool {
	_, _, ok := p.streamer()
	return ok
}

func (p *printer) maxDocumentBytes() int64 {
	if p.streams() {
		return maxStreamDocumentBytes
	}
	return maxDocumentBytes
}

func (p *printer) maxSpoolBytes() int64 {
	if p.streams() {
		return maxStreamSpoolBytes
	}
	return 0
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
	info.Color = p.caps.UseColor(info.ColorMode)
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

// limits are the decoder's caps for the job; MaxGrayBytes is per page for a streaming driver
// and per job for the others. A colour job decodes to RGB.
func (p *printer) limits(info driver.JobInfo) raster.Limits {
	lim := raster.Limits{MaxGrayBytes: p.maxGray, MaxPages: maxPages, RGB: info.Color}
	if pl, ok := p.drv.(driver.PageLimiter); ok {
		l := pl.PageLimits(info)
		lim.WidthMin, lim.WidthMax = l.WidthMin, l.WidthMax
	}
	return lim
}

// renderError marks a driver's document error so the job gets document-format-error.
func renderError(err error) error {
	if errors.Is(err, driver.ErrDocument) {
		return documentFormatError(err)
	}
	return err
}

// render converts the whole document before anything is sent, so a bad page never leaves a
// half-printed job.
func (p *printer) render(ctx context.Context, doc *io.SectionReader, info driver.JobInfo) ([]driver.Unit, error) {
	var units []driver.Unit
	var err error
	if isRaster(info.Format) {
		if info.Color {
			info.Color = colored(doc)
		}
		if info.Color {
			units, err = p.renderRaster(ctx, doc, info)
			if errors.Is(err, raster.ErrTooLarge) || errors.Is(err, driver.ErrJobTooLarge) {
				info.Log("printing in gray: not possible in colour: %v", err)
				info.Color = false
			}
		}
		if !info.Color {
			units, err = p.renderRaster(ctx, doc, info)
		}
		if err != nil || units == nil {
			return nil, err
		}
	} else {
		dr, ok := p.drv.(driver.DocumentRenderer)
		if !ok {
			return nil, documentFormatError(fmt.Errorf("driver %s cannot print %s", p.drv.Name(), info.Format))
		}
		data := make([]byte, doc.Size())
		if _, err := doc.ReadAt(data, 0); err != nil && err != io.EOF {
			return nil, err
		}
		units, err = dr.RenderDocument(ctx, info, info.Format, data)
	}
	if err != nil {
		return nil, renderError(err)
	}
	if !p.caps.CopiesNative {
		units = driver.RepeatUnits(units, info.Copies)
	}
	return units, nil
}

// renderRaster decodes the whole document and renders it.
func (p *printer) renderRaster(ctx context.Context, doc *io.SectionReader, info driver.JobInfo) ([]driver.Unit, error) {
	pages, err := raster.DecodeFrom(io.NewSectionReader(doc, 0, doc.Size()), p.limits(info))
	if errors.Is(err, raster.ErrNoPages) {
		info.Log("document has no pages; nothing to print")
		return nil, nil
	}
	if err != nil {
		return nil, documentFormatError(err)
	}
	for i, pg := range pages {
		info.Log("page %d: %dx%d @%dx%ddpi", i+1, pg.Width, pg.Height, pg.XDPI, pg.YDPI)
	}
	units, err := p.drv.Render(ctx, info, pages)
	if err != nil {
		return nil, renderError(err)
	}
	return units, nil
}

// colored reports whether a raster document has a colour page, so a colour job of a gray
// document prints as the gray job it always was. Errors are left to the decode that follows.
func colored(doc *io.SectionReader) bool {
	rd := raster.NewReader(io.NewSectionReader(doc, 0, doc.Size()), raster.Limits{})
	for {
		if _, err := rd.Skip(); err != nil {
			return rd.Colored()
		}
	}
}

// sendFunc delivers one unit.
type sendFunc func(ctx context.Context, u driver.Unit) error

// sendUnits sends each unit as its own device job, retrying only while nothing was sent.
// onUnit gets the running impression count after each unit. It returns that count.
func (p *printer) sendUnits(ctx context.Context, units []driver.Unit, logf logFunc, onUnit func(int), send sendFunc) (int, error) {
	if send == nil {
		send = p.send.Send
	}
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
		if err := backend.Retry(ctx, func(ctx context.Context) error { return send(ctx, u) }, p.retry, retryLog); err != nil {
			return impressions, fmt.Errorf("%s: %w", desc, renderError(err))
		}
		impressions += max(u.Impressions, 1)
		logf("%s printed", desc)
		if onUnit != nil {
			onUnit(impressions)
		}
	}
	return impressions, nil
}

// printDocument prints a spooled document: streamed page by page when the driver can, else
// rendered whole and then sent unit by unit.
func (p *printer) printDocument(ctx context.Context, doc *io.SectionReader, info driver.JobInfo, onUnit func(int)) (int, error) {
	if s, ss, ok := p.streamer(); ok && isRaster(info.Format) {
		return p.streamRaster(ctx, s, ss, doc, info, onUnit)
	}
	if ds, ok := p.drv.(driver.DocumentStreamer); ok && !isRaster(info.Format) {
		if ss, ok := p.send.(driver.StreamSender); ok {
			return p.streamDocument(ctx, ds, ss, doc, info, onUnit)
		}
	}
	units, err := p.render(ctx, doc, info)
	if err != nil {
		return 0, err
	}
	return p.sendUnits(ctx, units, info.Logf, onUnit, nil)
}

// streamRaster checks every page of the document first, without keeping any, so a bad page is
// refused before anything is sent. Then each unit reads the document again from the start and
// goes to the printer as its pages are rendered.
//
// The check pass reads the pages as gray, as a gray job always did. A colour job whose document
// has no colour pixel prints as a gray one, and so does one that does not fit in colour (a page
// over the decode budget as RGB, or a sheet over the job's budget): it prints as it did before
// the bridge had colour, rather than not at all.
func (p *printer) streamRaster(ctx context.Context, s driver.Streamer, ss driver.StreamSender, doc *io.SectionReader, info driver.JobInfo, onUnit func(int)) (int, error) {
	color := info.Color
	info.Color = false
	lim := p.limits(info)
	rd := raster.NewReader(io.NewSectionReader(doc, 0, doc.Size()), lim)
	pages, largest := 0, 0
	rgbTooLarge := error(nil)
	var checked []raster.Page // the pages' sizes, checked again as RGB if the job is in colour
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		pg, err := rd.Skip()
		if err == io.EOF {
			break
		}
		if errors.Is(err, raster.ErrNoPages) {
			info.Log("document has no pages; nothing to print")
			return 0, nil
		}
		if err != nil {
			return 0, documentFormatError(err)
		}
		pages++
		largest = max(largest, pg.Width*pg.Height)
		info.Log("page %d: %dx%d @%dx%ddpi", pages, pg.Width, pg.Height, pg.XDPI, pg.YDPI)
		if color && rgbTooLarge == nil && int64(pg.Width)*int64(pg.Height)*3 > int64(lim.MaxGrayBytes) {
			rgbTooLarge = fmt.Errorf("%w: page %d needs %d bytes of RGB, over the %d byte budget",
				raster.ErrTooLarge, pages, int64(pg.Width)*int64(pg.Height)*3, lim.MaxGrayBytes)
		}
		if err := s.CheckPage(info, pages, pg); err != nil {
			return 0, renderError(err)
		}
		if color {
			checked = append(checked, pg)
		}
	}
	var units []driver.Unit
	if color && rd.Colored() {
		ci := info
		ci.Color = true
		var err error
		units, err = colorPlan(s, ci, checked, rgbTooLarge)
		if err != nil {
			info.Log("printing in gray: not possible in colour: %v", err)
		} else {
			info = ci
			lim = p.limits(info)
			info.Log("printing in colour")
		}
	}
	if units == nil {
		var err error
		if units, err = s.Plan(info, pages); err != nil {
			return 0, renderError(err)
		}
	}
	if !p.caps.CopiesNative {
		units = driver.RepeatUnits(units, info.Copies)
	}
	return p.sendUnits(ctx, units, info.Logf, onUnit, func(ctx context.Context, u driver.Unit) error {
		return ss.SendStream(ctx, u, func(w io.Writer) error {
			pages := raster.NewReader(io.NewSectionReader(doc, 0, doc.Size()), lim)
			if info.Color {
				pages.Reserve(3 * largest)
			} else {
				pages.Reserve(largest)
			}
			return s.Stream(ctx, info, u, pages, w)
		})
	})
}

// colorPlan checks the pages for a colour job and plans it, or says why it cannot be printed in
// colour.
func colorPlan(s driver.Streamer, info driver.JobInfo, pages []raster.Page, rgbTooLarge error) ([]driver.Unit, error) {
	if rgbTooLarge != nil {
		return nil, rgbTooLarge
	}
	for i, pg := range pages {
		if err := s.CheckPage(info, i+1, pg); err != nil {
			return nil, err
		}
	}
	return s.Plan(info, len(pages))
}

// streamDocument sends a pass-through document straight from the spool file.
func (p *printer) streamDocument(ctx context.Context, ds driver.DocumentStreamer, ss driver.StreamSender, doc *io.SectionReader, info driver.JobInfo, onUnit func(int)) (int, error) {
	units, err := ds.PlanDocument(info, info.Format, doc)
	if err != nil {
		return 0, renderError(err)
	}
	if !p.caps.CopiesNative {
		units = driver.RepeatUnits(units, info.Copies)
	}
	return p.sendUnits(ctx, units, info.Logf, onUnit, func(ctx context.Context, u driver.Unit) error {
		return ss.SendStream(ctx, u, func(w io.Writer) error {
			return ds.StreamDocument(ctx, info, u, doc, w)
		})
	})
}

func (p *printer) processor() ipp.Processor {
	return func(ctx context.Context, job *ipp.Job, doc io.Reader, format string) error {
		jlog := func(f string, args ...any) {
			p.logf("job %d: "+f, append([]any{job.ID}, args...)...)
		}
		sec, err := docSection(doc)
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
			job.Name, job.User, format, sec.Size(), a.Media, mediaCol, a.PrintQuality, a.Copies, a.Sides)
		prefix := fmt.Sprintf("job%d", job.ID)
		if p.debugDir != "" {
			if err := copyFile(filepath.Join(p.debugDir, prefix+"-in"+extension(format)), sec); err != nil {
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
		n, err := p.printDocument(ctx, sec, info, func(n int) {
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

// docSection reads a spooled document in place: the processor gets the spool file. Anything
// else is read into memory.
func docSection(doc io.Reader) (*io.SectionReader, error) {
	if ra, ok := doc.(io.ReaderAt); ok {
		if f, ok := doc.(interface{ Stat() (os.FileInfo, error) }); ok {
			if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
				return io.NewSectionReader(ra, 0, st.Size()), nil
			}
		}
	}
	data, err := io.ReadAll(doc)
	if err != nil {
		return nil, err
	}
	return io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data))), nil
}

// copyFile writes the whole of src to a new private file at path.
func copyFile(path string, src *io.SectionReader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.NewSectionReader(src, 0, src.Size()))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
