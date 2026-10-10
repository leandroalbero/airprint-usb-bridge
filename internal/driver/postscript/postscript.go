// Package postscript is the postscript driver: PostScript level 2/3 printers. Each page is placed
// on the sheet and sent as one LanguageLevel 2 image (RunLengthDecode, or FlateDecode at level 3,
// ASCII85-wrapped by default), with PageSize, Duplex and NumCopies set through setpagedevice.
// application/postscript documents can optionally be passed through unchanged.
package postscript

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

const (
	formatPS     = "application/postscript"
	maxCopies    = 99
	marginAround = 423 // 4.23 mm, the usual unprintable border of office lasers
)

var mediaIDs = []struct{ id, name string }{
	{"a4", driver.MediaA4},
	{"letter", driver.MediaLetter},
	{"legal", driver.MediaLegal},
	{"a5", driver.MediaA5},
	{"executive", driver.MediaExecutive},
}

// Driver renders raster pages as a PostScript document.
type Driver struct {
	caps        driver.Capabilities
	set         settings
	dither      string
	passthrough bool
}

// New builds the driver from the bridge configuration and its --driver-opt options.
func New(cfg driver.Config) (driver.Driver, error) {
	var d Driver
	var err error
	if d.set.Level, err = cfg.IntOption("level", 2); err != nil {
		return nil, err
	}
	if d.set.Level != 2 && d.set.Level != 3 {
		return nil, fmt.Errorf("option level=%d: want 2 or 3", d.set.Level)
	}
	if d.set.Depth, err = cfg.IntOption("depth", 1); err != nil {
		return nil, err
	}
	if d.set.Depth != 1 && d.set.Depth != 8 {
		return nil, fmt.Errorf("option depth=%d: want 1 or 8", d.set.Depth)
	}
	switch enc := cfg.Option("encoding", "ascii85"); enc {
	case "ascii85":
	case "binary":
		d.set.Binary = true
	default:
		return nil, fmt.Errorf("option encoding=%q: want ascii85 or binary", enc)
	}
	dpi, err := cfg.IntOption("resolution", 300)
	if err != nil {
		return nil, err
	}
	if dpi != 300 && dpi != 600 {
		return nil, fmt.Errorf("option resolution=%d: want 300 or 600", dpi)
	}
	if d.set.Duplex, err = cfg.BoolOption("duplex", false); err != nil {
		return nil, err
	}
	if d.passthrough, err = cfg.BoolOption("passthrough", false); err != nil {
		return nil, err
	}
	if d.set.PJL, err = cfg.BoolOption("pjl", false); err != nil {
		return nil, err
	}
	switch cfg.Dither {
	case "", imaging.DitherAuto, imaging.DitherOn, imaging.DitherOff:
		d.dither = cfg.Dither
	default:
		return nil, fmt.Errorf("dither %q: want auto, on or off", cfg.Dither)
	}

	names := make([]string, len(mediaIDs))
	for i, m := range mediaIDs {
		names[i] = m.name
	}
	media, err := driver.Sheets(names, driver.Margins{Top: marginAround, Bottom: marginAround, Left: marginAround, Right: marginAround}, "stationery", "main")
	if err != nil {
		return nil, err
	}
	def, err := pickMedia(cfg.Media)
	if err != nil {
		return nil, err
	}
	for i := range media {
		media[i].Ready = media[i].Name == def
	}

	res := []driver.Resolution{{X: 300, Y: 300}}
	rs := "RS300"
	if dpi == 600 {
		res = []driver.Resolution{{X: 600, Y: 600}, {X: 300, Y: 300}}
		rs = "RS300-600"
	}
	urf := []string{"V1.4", "W8", "CP" + strconv.Itoa(maxCopies)}
	if d.set.Duplex {
		urf = append(urf, "DM1")
	}
	urf = append(urf, "IS1", "MT1", rs)
	var formats []string
	if d.passthrough {
		formats = []string{formatPS}
	}
	d.caps = driver.Capabilities{
		MakeModel:    makeModel(cfg),
		Kind:         []string{"document"},
		Duplex:       d.set.Duplex,
		Resolutions:  res,
		Qualities:    []int{driver.QualityNormal},
		URF:          urf,
		PWGTypes:     []string{"sgray_8"},
		Formats:      formats,
		MaxCopies:    maxCopies,
		CopiesNative: true,
		Media:        media,
		DefaultMedia: def,
	}
	return &d, nil
}

func pickMedia(id string) (string, error) {
	if id == "" {
		return driver.MediaA4, nil
	}
	var valid []string
	for _, m := range mediaIDs {
		if strings.EqualFold(id, m.id) || id == m.name {
			return m.name, nil
		}
		valid = append(valid, m.id)
	}
	return "", fmt.Errorf("media %q: want one of %s (or their PWG names)", id, strings.Join(valid, ", "))
}

// makeModel names the printer after its IEEE 1284 ID when the bridge has one. DeviceID stays
// empty so the bridge derives one listing the formats it accepts, not the printer's own CMD.
func makeModel(cfg driver.Config) string {
	mfg, mdl := cfg.DeviceID.Manufacturer(), cfg.DeviceID.Model()
	switch {
	case mdl == "":
		return "Generic PostScript Printer"
	case mfg == "" || strings.HasPrefix(strings.ToLower(mdl), strings.ToLower(mfg)):
		return mdl
	}
	return mfg + " " + mdl
}

func (d *Driver) Name() string                      { return "postscript" }
func (d *Driver) Capabilities() driver.Capabilities { return d.caps }

// outputDPI keeps the client's resolution when the printer is offered it, so pages are copied
// dot for dot instead of resampled.
func (d *Driver) outputDPI(p raster.Page) int {
	if p.XDPI == p.YDPI && slices.Contains(d.caps.Resolutions, driver.Resolution{X: p.XDPI, Y: p.YDPI}) {
		return p.XDPI
	}
	return d.caps.Resolutions[0].X
}

// copyPlan splits copies between /NumCopies (one-page jobs, where it cannot uncollate anything)
// and repeated units (each a whole collated job, which also keeps duplex sheets paired).
func copyPlan(copies, pages int) (numCopies, repeat int) {
	copies = max(copies, 1)
	if pages == 1 {
		return copies, 1
	}
	return 1, copies
}

func repeatUnits(data []byte, n, impressions int, what string) []driver.Unit {
	units := make([]driver.Unit, n)
	for i := range units {
		label := what
		if n > 1 {
			label = fmt.Sprintf("%s, copy %d of %d", what, i+1, n)
		}
		units[i] = driver.Unit{Data: data, Label: label, Impressions: impressions}
	}
	return units
}

// Render places every page on the job's sheet and encodes it. The whole document is built before
// it returns; nothing is sent from here.
func (d *Driver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	if len(pages) == 0 {
		return nil, nil
	}
	media := job.Media
	if media.Width <= 0 || media.Length <= 0 {
		return nil, driver.DocumentErrorf("postscript: media %q has no fixed size", media.Name)
	}
	if job.Sides != "" && job.Sides != "one-sided" && !d.set.Duplex {
		job.Log("sides %s ignored: printer has no duplexer (driver option duplex=true)", job.Sides)
	}
	numCopies, repeat := copyPlan(job.Copies, len(pages))
	var buf bytes.Buffer
	w := newDocWriter(&buf, d.set, docInfo{
		Title: job.Name, Width: media.Width, Length: media.Length, Sides: job.Sides, NumCopies: numCopies,
	}, len(pages))
	for i, p := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dpi := d.outputDPI(p)
		sheet, err := imaging.Place(p, media, imaging.PlaceOptions{
			XDPI: dpi, YDPI: dpi, Scaling: job.Scaling, Orientation: job.Orientation, Budget: job.Budget,
		})
		if err != nil {
			return nil, driver.DocumentError(fmt.Errorf("postscript: page %d: %w", i+1, err))
		}
		pages[i].Gray = nil
		img := pageImage{W: sheet.Width, H: sheet.Height, XDPI: dpi, YDPI: dpi, Depth: d.set.Depth}
		if d.set.Depth == 8 {
			img.Data = sheet.Gray
			job.Log("page %d: %dx%d gray at %d dpi on %s", i+1, img.W, img.H, dpi, media.Name)
			job.DebugImage(fmt.Sprintf("p%d", i+1), imaging.GrayImage(sheet))
		} else {
			bm, dithered, err := imaging.Binarize(sheet, d.dither)
			if err != nil {
				return nil, driver.DocumentError(fmt.Errorf("postscript: page %d: %w", i+1, err))
			}
			img.Data = bm.Pix
			job.Log("page %d: %dx%d dots at %d dpi on %s, dithered %v", i+1, img.W, img.H, dpi, media.Name, dithered)
			job.DebugImage(fmt.Sprintf("p%d", i+1), bm.Image())
		}
		if err := w.page(img); err != nil {
			return nil, err
		}
	}
	if err := w.finish(); err != nil {
		return nil, err
	}
	return repeatUnits(buf.Bytes(), repeat, len(pages)*numCopies, fmt.Sprintf("job (%d pages)", len(pages))), nil
}

// RenderDocument sends an application/postscript document as it is, after an optional Duplex
// request (only on a duplex printer) and the optional PJL wrapper. Copies are whole repeated jobs:
// rewriting /NumCopies inside a client's document is not safe.
func (d *Driver) RenderDocument(ctx context.Context, job driver.JobInfo, format string, data []byte) ([]driver.Unit, error) {
	if !d.passthrough || format != formatPS {
		return nil, driver.DocumentErrorf("postscript: cannot print %s (driver option passthrough=true accepts %s)", format, formatPS)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wrapped := bytes.HasPrefix(data, []byte(uel))
	if !wrapped && !bytes.HasPrefix(bytes.TrimLeft(data, "\x04"), []byte("%!")) {
		return nil, driver.DocumentErrorf("postscript: document does not start with %%!")
	}
	pages := dscPages(data)
	var buf bytes.Buffer
	if d.set.PJL && !wrapped {
		buf.WriteString(uel + "@PJL JOB\r\n@PJL ENTER LANGUAGE = POSTSCRIPT\r\n")
	}
	if d.set.Duplex && !wrapped {
		// ^D ends a job on some channels; left in front of the document it would reset the
		// Duplex request made just before it.
		data = bytes.TrimLeft(data, "\x04")
		buf.WriteString("%!PS\n")
		var setup bytes.Buffer
		switch job.Sides {
		case "two-sided-long-edge":
			feature(&setup, "/Duplex true /Tumble false")
		case "two-sided-short-edge":
			feature(&setup, "/Duplex true /Tumble true")
		default:
			feature(&setup, "/Duplex false")
		}
		buf.Write(setup.Bytes())
	}
	buf.Write(data)
	if d.set.PJL && !wrapped {
		buf.WriteString(uel + "@PJL EOJ\r\n" + uel)
	}
	job.Log("PostScript pass-through: %d bytes, %d pages", len(data), pages)
	return repeatUnits(buf.Bytes(), max(job.Copies, 1), pages, fmt.Sprintf("PostScript document (%d pages)", pages)), nil
}

// dscPages reads %%Pages from the document's comments: the first number, or the trailer's when
// the header says (atend). Documents without it count as one impression.
func dscPages(data []byte) int {
	first, last := 0, 0
	atend := false
	key := []byte("%%Pages:")
	for i := 0; i < len(data); {
		j := bytes.Index(data[i:], key)
		if j < 0 {
			break
		}
		at := i + j
		i = at + len(key)
		if at > 0 && data[at-1] != '\n' && data[at-1] != '\r' {
			continue
		}
		end := bytes.IndexAny(data[i:], "\r\n")
		if end < 0 {
			end = len(data) - i
		}
		f := strings.Fields(string(data[i : i+end]))
		switch {
		case len(f) == 0:
		case f[0] == "(atend)":
			atend = true
		default:
			if n, err := strconv.Atoi(f[0]); err == nil && n >= 0 {
				if first == 0 && !atend {
					first = n
				}
				last = n
			}
		}
	}
	n := first
	if atend || n == 0 {
		n = last
	}
	return max(n, 1)
}
