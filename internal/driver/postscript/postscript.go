// Package postscript is the postscript driver: PostScript level 2/3 printers. Each page is placed
// on the sheet and sent as one LanguageLevel 2 image (RunLengthDecode, or FlateDecode at level 3,
// ASCII85-wrapped by default), with PageSize, Duplex and NumCopies set through setpagedevice.
// application/postscript documents can optionally be passed through unchanged.
package postscript

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
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

func repeatUnits(n, impressions int, what string, meta any) []driver.Unit {
	units := make([]driver.Unit, n)
	for i := range units {
		label := what
		if n > 1 {
			label = fmt.Sprintf("%s, copy %d of %d", what, i+1, n)
		}
		units[i] = driver.Unit{Label: label, Impressions: impressions, Meta: meta}
	}
	return units
}

// Render is the job rendered in memory; the bridge streams it instead (Stream).
func (d *Driver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	return driver.RenderStream(ctx, d, job, pages)
}

func (d *Driver) placeOptions(job driver.JobInfo, dpi int) imaging.PlaceOptions {
	return imaging.PlaceOptions{XDPI: dpi, YDPI: dpi, Scaling: job.Scaling, Orientation: job.Orientation, Budget: job.Budget}
}

// CheckPage checks that page n can be placed on the job's sheet.
func (d *Driver) CheckPage(job driver.JobInfo, n int, p raster.Page) error {
	if job.Media.Width <= 0 || job.Media.Length <= 0 {
		return nil // Plan refuses the job
	}
	if err := imaging.CheckPlace(p, job.Media, d.placeOptions(job, d.outputDPI(p))); err != nil {
		return driver.DocumentError(fmt.Errorf("postscript: page %d: %w", n, err))
	}
	return nil
}

// jobPlan is what Plan tells Stream: the page count goes in the document header.
type jobPlan struct{ pages, numCopies int }

// Plan makes one document of every page, repeated per copy unless /NumCopies does it.
func (d *Driver) Plan(job driver.JobInfo, pages int) ([]driver.Unit, error) {
	if pages == 0 {
		return nil, nil
	}
	media := job.Media
	if media.Width <= 0 || media.Length <= 0 {
		return nil, driver.DocumentErrorf("postscript: media %q has no fixed size", media.Name)
	}
	if job.Sides != "" && job.Sides != "one-sided" && !d.set.Duplex {
		job.Log("sides %s ignored: printer has no duplexer (driver option duplex=true)", job.Sides)
	}
	numCopies, repeat := copyPlan(job.Copies, pages)
	return repeatUnits(repeat, pages*numCopies, fmt.Sprintf("job (%d pages)", pages), jobPlan{pages, numCopies}), nil
}

// Stream places every page on the job's sheet and encodes it as it goes. Output is buffered
// until a page is complete enough to be worth a write; a job that fails before its first write
// sends nothing, and one that fails later still gets the document's ending.
func (d *Driver) Stream(ctx context.Context, job driver.JobInfo, u driver.Unit, pages driver.PageReader, w io.Writer) error {
	plan, ok := u.Meta.(jobPlan)
	if !ok {
		return fmt.Errorf("postscript: unit %q was not planned by this driver", u.Label)
	}
	media := job.Media
	out := driver.NewOutput(ctx, w)
	bw := bufio.NewWriterSize(out, 64<<10)
	doc := newDocWriter(bw, d.set, docInfo{
		Title: job.Name, Width: media.Width, Length: media.Length, Sides: job.Sides, NumCopies: plan.numCopies,
	}, plan.pages)
	fail := func(err error) error {
		if out.Written() == 0 {
			return err
		}
		_ = bw.Flush()
		return out.Abort(err, doc.ending())
	}
	for n := 1; ; n++ {
		if err := out.Err(); err != nil {
			return fail(err)
		}
		p, err := pages.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		if err := d.page(job, doc, p, n); err != nil {
			return fail(err)
		}
	}
	if err := doc.finish(); err != nil {
		return fail(err)
	}
	if err := bw.Flush(); err != nil {
		return out.Abort(err, nil)
	}
	return nil
}

// page places page n on the sheet and writes it to doc.
func (d *Driver) page(job driver.JobInfo, doc *docWriter, p raster.Page, n int) error {
	media := job.Media
	dpi := d.outputDPI(p)
	o := d.placeOptions(job, dpi)
	o.InPlace = true
	sheet, err := imaging.Place(p, media, o)
	if err != nil {
		return driver.DocumentError(fmt.Errorf("postscript: page %d: %w", n, err))
	}
	defer job.Budget.Release(int64(sheet.Width) * int64(sheet.Height))
	img := pageImage{W: sheet.Width, H: sheet.Height, XDPI: dpi, YDPI: dpi, Depth: d.set.Depth}
	if d.set.Depth == 8 {
		img.Data = sheet.Gray
		job.Log("page %d: %dx%d gray at %d dpi on %s", n, img.W, img.H, dpi, media.Name)
		if job.Debug != nil {
			job.DebugImage(fmt.Sprintf("p%d", n), imaging.GrayImage(sheet))
		}
	} else {
		bm, dithered, err := imaging.Binarize(sheet, d.dither)
		if err != nil {
			return driver.DocumentError(fmt.Errorf("postscript: page %d: %w", n, err))
		}
		img.Data = bm.Pix
		job.Log("page %d: %dx%d dots at %d dpi on %s, dithered %v", n, img.W, img.H, dpi, media.Name, dithered)
		if job.Debug != nil {
			job.DebugImage(fmt.Sprintf("p%d", n), bm.Image())
		}
	}
	return doc.page(img)
}

// RenderDocument is the pass-through document rendered in memory; the bridge streams it instead
// (StreamDocument).
func (d *Driver) RenderDocument(ctx context.Context, job driver.JobInfo, format string, data []byte) ([]driver.Unit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	doc := io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))
	units, err := d.PlanDocument(job, format, doc)
	if err != nil || len(units) == 0 {
		return units, err
	}
	var buf bytes.Buffer
	if err := d.StreamDocument(ctx, job, units[0], doc, &buf); err != nil {
		return nil, err
	}
	for i := range units {
		units[i].Data = buf.Bytes()
	}
	return units, nil
}

// docPlan is what PlanDocument tells StreamDocument.
type docPlan struct {
	wrapped bool  // the document brings its own PJL job
	skip    int64 // leading ^D bytes left out
}

// PlanDocument checks an application/postscript document and counts its pages. Copies are whole
// repeated jobs: rewriting /NumCopies inside a client's document is not safe.
func (d *Driver) PlanDocument(job driver.JobInfo, format string, doc *io.SectionReader) ([]driver.Unit, error) {
	if !d.passthrough || format != formatPS {
		return nil, driver.DocumentErrorf("postscript: cannot print %s (driver option passthrough=true accepts %s)", format, formatPS)
	}
	br := bufio.NewReader(io.NewSectionReader(doc, 0, doc.Size()))
	head, _ := br.Peek(len(uel))
	plan := docPlan{wrapped: bytes.HasPrefix(head, []byte(uel))}
	var ctrlD int64
	for {
		b, err := br.ReadByte()
		if err != nil || b != 0x04 {
			break
		}
		ctrlD++
	}
	start := make([]byte, 2)
	n, _ := io.ReadFull(io.NewSectionReader(doc, ctrlD, 2), start)
	if !plan.wrapped && !bytes.HasPrefix(start[:n], []byte("%!")) {
		return nil, driver.DocumentErrorf("postscript: document does not start with %%!")
	}
	size := doc.Size()
	if d.set.Duplex && !plan.wrapped {
		// ^D ends a job on some channels; left in front of the document it would reset the
		// Duplex request made just before it.
		plan.skip = ctrlD
		size -= ctrlD
	}
	pages, err := dscPagesFrom(io.NewSectionReader(doc, 0, doc.Size()))
	if err != nil {
		return nil, err
	}
	job.Log("PostScript pass-through: %d bytes, %d pages", size, pages)
	return repeatUnits(max(job.Copies, 1), pages, fmt.Sprintf("PostScript document (%d pages)", pages), plan), nil
}

// StreamDocument sends the document as it is, after an optional Duplex request (only on a duplex
// printer) and inside the optional PJL wrapper.
func (d *Driver) StreamDocument(ctx context.Context, job driver.JobInfo, u driver.Unit, doc *io.SectionReader, w io.Writer) error {
	plan, ok := u.Meta.(docPlan)
	if !ok {
		return fmt.Errorf("postscript: unit %q was not planned by this driver", u.Label)
	}
	out := driver.NewOutput(ctx, w)
	var head, end bytes.Buffer
	if d.set.PJL && !plan.wrapped {
		head.WriteString(uel + "@PJL JOB\r\n@PJL ENTER LANGUAGE = POSTSCRIPT\r\n")
		end.WriteString(uel + "@PJL EOJ\r\n" + uel)
	}
	if d.set.Duplex && !plan.wrapped {
		head.WriteString("%!PS\n")
		switch job.Sides {
		case "two-sided-long-edge":
			feature(&head, "/Duplex true /Tumble false")
		case "two-sided-short-edge":
			feature(&head, "/Duplex true /Tumble true")
		default:
			feature(&head, "/Duplex false")
		}
	}
	body := io.NewSectionReader(doc, plan.skip, doc.Size()-plan.skip)
	buf := make([]byte, 64<<10)
	n := copy(buf, head.Bytes())
	for {
		if err := out.Err(); err != nil {
			return out.Abort(err, end.Bytes())
		}
		m, err := io.ReadFull(body, buf[n:])
		n += m
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return out.Abort(werr, nil)
			}
			n = 0
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return out.Abort(err, end.Bytes())
		}
	}
	if _, err := out.Write(end.Bytes()); err != nil {
		return out.Abort(err, nil)
	}
	return nil
}

// dscPages reads %%Pages from the document's comments: the first number, or the trailer's when
// the header says (atend). Documents without it count as one impression.
func dscPages(data []byte) int {
	n, _ := dscPagesFrom(bytes.NewReader(data))
	return n
}

// dscPagesFrom is dscPages over a stream. A %%Pages comment counts only at the start of a line
// (after CR or LF); its value is the first field of the rest of the line.
func dscPagesFrom(r io.Reader) (int, error) {
	const keep = 4096
	key := []byte("%%Pages:")
	first, last := 0, 0
	atend := false
	value := func(v []byte) {
		f := strings.Fields(string(v))
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
	var line []byte // the start of the current line while it may be a %%Pages comment
	maybe := true
	add := func(seg []byte) {
		if !maybe {
			return
		}
		line = append(line, seg[:min(len(seg), keep+len(key)-len(line))]...)
		k := min(len(line), len(key))
		maybe = bytes.Equal(line[:k], key[:k])
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		chunk := buf[:n]
		for len(chunk) > 0 {
			i := bytes.IndexAny(chunk, "\r\n")
			if i < 0 {
				add(chunk)
				break
			}
			add(chunk[:i])
			if maybe && len(line) >= len(key) {
				value(line[len(key):])
			}
			line, maybe = line[:0], true
			chunk = chunk[i+1:]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if maybe && len(line) >= len(key) {
		value(line[len(key):])
	}
	n := first
	if atend || n == 0 {
		n = last
	}
	return max(n, 1), nil
}
