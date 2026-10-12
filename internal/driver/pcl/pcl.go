// Package pcl is the pcl driver: PCL 5 lasers. Each page is placed on the sheet, binarized
// and sent as PCL raster graphics (compression mode 2 or 3) inside a PJL job. With --color on
// colour jobs go out as PCL 5c 24-bit RGB raster (Configure Image Data, direct by pixel).
package pcl

import (
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

// paper is one supported size: its PCL page size code (ESC &l#A) and the portrait logical page
// at 600 dpi. The logical page, not the sheet, is where PCL can print: X 0 is its left edge and
// anything outside it is clipped. Vertically it is the whole sheet.
type paper struct {
	name   string
	alias  string
	code   int
	offset int // left edge of the logical page, dots at 600 dpi
	width  int // logical page width, dots at 600 dpi
}

var papers = []paper{
	{driver.MediaA4, "a4", 26, 142, 4676},
	{driver.MediaLetter, "letter", 2, 150, 4800},
	{driver.MediaLegal, "legal", 3, 150, 4800},
	{driver.MediaA5, "a5", 25, 142, 3196},
	{driver.MediaExecutive, "executive", 1, 150, 4050},
}

func paperFor(name string) (paper, bool) {
	for _, p := range papers {
		if p.name == name {
			return p, true
		}
	}
	return paper{}, false
}

// logical returns the logical page's left offset and width in dots at dpi.
func (p paper) logical(dpi int) (offset, width int) {
	return p.offset * dpi / 600, p.width * dpi / 600
}

// margins are the IPP margins of p: PCL's 1/6 inch unprintable band at the top and bottom, and the
// logical page edges at the sides (wider than the band, but raster cannot reach past them).
func (p paper) margins(sheetWidth int) driver.Margins {
	phys := sheetWidth * 600 / 2540 // A4 is 4960.6 dots; the table's sheet is 4960
	up := func(dots int) int { return (dots*2540 + 599) / 600 }
	return driver.Margins{Top: 423, Bottom: 423, Left: up(p.offset), Right: up(phys - p.offset - p.width)}
}

func mediaHelp() string {
	var s []string
	for _, p := range papers {
		s = append(s, p.name+" ("+p.alias+")")
	}
	return strings.Join(s, ", ")
}

// Driver renders jobs for a PCL 5 laser, mono or (PCL 5c) colour.
type Driver struct {
	caps        driver.Capabilities
	dpis        []int // offered resolutions; [0] is the default
	compression int
	pjl         bool
	dither      string
	color       bool
}

// New builds the driver from the bridge settings and the --driver-opt keys dpi, duplex,
// compression and pjl.
func New(cfg driver.Config) (driver.Driver, error) {
	dpis, err := parseDPI(cfg.Option("dpi", "300"))
	if err != nil {
		return nil, err
	}
	if cfg.HighRes && !slices.Contains(dpis, 600) {
		dpis = append(dpis, 600)
	}
	duplex, err := cfg.BoolOption("duplex", false)
	if err != nil {
		return nil, err
	}
	compression, err := cfg.IntOption("compression", 2)
	if err != nil {
		return nil, err
	}
	if compression != 2 && compression != 3 {
		return nil, fmt.Errorf("option compression=%d: want 2 (TIFF PackBits) or 3 (delta row)", compression)
	}
	pjl, err := cfg.BoolOption("pjl", true)
	if err != nil {
		return nil, err
	}
	switch cfg.Dither {
	case "", imaging.DitherAuto, imaging.DitherOn, imaging.DitherOff:
	default:
		return nil, fmt.Errorf("dither %q: want auto, on or off", cfg.Dither)
	}
	var color bool
	switch cfg.Color {
	case "", "auto", "off":
	case "on":
		color = true
	default:
		return nil, fmt.Errorf("color %q: want auto, on or off", cfg.Color)
	}

	def, err := pickMedia(cfg.Media)
	if err != nil {
		return nil, err
	}
	var media []driver.Media
	for _, p := range papers {
		m, err := driver.Sheets([]string{p.name}, driver.Margins{}, "stationery", "main")
		if err != nil {
			return nil, err
		}
		m[0].Margins = p.margins(m[0].Width)
		m[0].Ready = p.name == def
		media = append(media, m[0])
	}

	var res []driver.Resolution
	rs := "RS"
	for i, d := range dpis {
		res = append(res, driver.Resolution{X: d, Y: d})
		if i > 0 {
			rs += "-"
		}
		rs += strconv.Itoa(d)
	}
	if len(dpis) == 2 && dpis[0] == 600 {
		rs = "RS300-600"
	}
	qualities := []int{driver.QualityNormal}
	if len(dpis) == 2 {
		qualities = []int{driver.QualityDraft, driver.QualityNormal, driver.QualityHigh}
	}
	urf := []string{"V1.4", "W8"}
	pwg := []string{"sgray_8"}
	if color {
		urf = append(urf, "SRGB24")
		pwg = append(pwg, "srgb_8")
	}
	urf = append(urf, "CP99")
	if duplex {
		urf = append(urf, "DM1")
	}
	urf = append(urf, "IS1", "MT1", rs)

	return &Driver{
		caps: driver.Capabilities{
			MakeModel:    makeModel(cfg),
			Kind:         []string{"document"},
			Color:        color,
			Duplex:       duplex,
			Resolutions:  res,
			Qualities:    qualities,
			URF:          urf,
			PWGTypes:     pwg,
			MaxCopies:    99,
			CopiesNative: true,
			Media:        media,
			DefaultMedia: def,
		},
		dpis:        dpis,
		compression: compression,
		pjl:         pjl,
		dither:      cfg.Dither,
		color:       color,
	}, nil
}

func parseDPI(v string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || (n != 300 && n != 600) || slices.Contains(out, n) {
			return nil, fmt.Errorf("option dpi=%q: want 300, 600, 300,600 or 600,300", v)
		}
		out = append(out, n)
	}
	return out, nil
}

func pickMedia(name string) (string, error) {
	if name == "" {
		return driver.MediaA4, nil
	}
	for _, p := range papers {
		if strings.EqualFold(name, p.alias) || name == p.name {
			return p.name, nil
		}
	}
	return "", fmt.Errorf("media %q: want one of %s", name, mediaHelp())
}

func makeModel(cfg driver.Config) string {
	mfg, mdl := strings.TrimSpace(cfg.DeviceID.Manufacturer()), strings.TrimSpace(cfg.DeviceID.Model())
	switch {
	case mdl == "":
		return "Generic PCL 5 Laser"
	case mfg == "" || strings.HasPrefix(strings.ToLower(mdl), strings.ToLower(mfg)):
		return mdl
	default:
		return mfg + " " + mdl
	}
}

func (d *Driver) Name() string                      { return "pcl" }
func (d *Driver) Capabilities() driver.Capabilities { return d.caps }

// resolution picks the job's dpi: the printer-resolution it asked for, else by print-quality
// when both 300 and 600 are offered (draft 300, high 600), else the default.
func (d *Driver) resolution(job driver.JobInfo) int {
	if r := job.Resolution; r.X == r.Y && slices.Contains(d.dpis, r.X) {
		return r.X
	}
	if len(d.dpis) == 2 {
		switch job.Quality {
		case driver.QualityDraft:
			return 300
		case driver.QualityHigh:
			return 600
		}
	}
	return d.dpis[0]
}

// Render is the job rendered in memory; the bridge streams it instead (Stream).
func (d *Driver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	return driver.RenderStream(ctx, d, job, pages)
}

func (d *Driver) placeOptions(job driver.JobInfo, dpi int) imaging.PlaceOptions {
	return imaging.PlaceOptions{XDPI: dpi, YDPI: dpi, Orientation: job.Orientation, Scaling: job.Scaling, Budget: job.Budget, Color: d.colorJob(job)}
}

// colorJob reports whether job goes out as 24-bit colour raster.
func (d *Driver) colorJob(job driver.JobInfo) bool {
	return d.color && job.Color
}

// CheckPage checks that page n can be placed on the job's sheet.
func (d *Driver) CheckPage(job driver.JobInfo, n int, pg raster.Page) error {
	if err := imaging.CheckPlace(pg, job.Media, d.placeOptions(job, d.resolution(job))); err != nil {
		return driver.DocumentError(fmt.Errorf("pcl: page %d: %w", n, err))
	}
	return nil
}

// Plan makes one PJL job of every page. Each copy is that job sent again: copies stay collated
// and start on a new sheet.
func (d *Driver) Plan(job driver.JobInfo, pages int) ([]driver.Unit, error) {
	if pages == 0 {
		return nil, driver.DocumentErrorf("pcl: no pages")
	}
	p, ok := paperFor(job.Media.Name)
	if !ok {
		return nil, driver.DocumentErrorf("pcl: media %q is not supported (have %s)", job.Media.Name, mediaHelp())
	}
	dpi := d.resolution(job)
	if off, lw := p.logical(dpi); off+lw > dots(job.Media.Width, dpi) {
		return nil, fmt.Errorf("pcl: logical page %d+%d dots wider than the %d-dot sheet", off, lw, dots(job.Media.Width, dpi))
	}
	label := fmt.Sprintf("PCL job, %d page(s)", pages)
	copies := max(job.Copies, 1)
	units := make([]driver.Unit, copies)
	for c := range units {
		units[c] = driver.Unit{Label: label, Impressions: pages}
		if copies > 1 {
			units[c].Label = fmt.Sprintf("%s, copy %d of %d", label, c+1, copies)
		}
	}
	return units, nil
}

func dots(hmm, dpi int) int {
	w, _ := imaging.SheetDots(driver.Media{Width: hmm}, dpi, dpi)
	return w
}

// Stream places, binarizes and encodes one page at a time inside one PJL job. The prologue goes
// out with the first page, so a job that fails before it sends nothing; one that fails later
// still gets the job ending.
func (d *Driver) Stream(ctx context.Context, job driver.JobInfo, _ driver.Unit, pages driver.PageReader, w io.Writer) error {
	out := driver.NewOutput(ctx, w)
	p, ok := paperFor(job.Media.Name)
	if !ok {
		return driver.DocumentErrorf("pcl: media %q is not supported (have %s)", job.Media.Name, mediaHelp())
	}
	dpi := d.resolution(job)
	duplex := 0
	if d.caps.Duplex {
		switch job.Sides {
		case "two-sided-long-edge":
			duplex = 1
		case "two-sided-short-edge":
			duplex = 2
		}
	}
	h := jobHeader{
		name: pjlName(job), pjl: d.pjl, dpi: dpi, code: p.code,
		duplex: duplex, duplexCapable: d.caps.Duplex, color: d.colorJob(job),
	}
	if h.color {
		return d.streamColor(job, pages, p, dpi, out, h)
	}
	end := epilogue(h)
	enc := &writer{compression: d.compression}
	for n := 1; ; n++ {
		if err := out.Err(); err != nil {
			return out.Abort(err, end)
		}
		pg, err := pages.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out.Abort(err, end)
		}
		bm, err := d.page(job, pg, p, dpi, n)
		if err != nil {
			return out.Abort(err, end)
		}
		data := enc.page(bm)
		if n == 1 {
			data = append(prologue(h), data...)
		}
		if _, err := out.Write(data); err != nil {
			return out.Abort(err, nil)
		}
		job.Log("page %d: %s %dx%d dots at %d dpi, %d bytes", n, p.alias, bm.W, bm.H, dpi, len(enc.out))
	}
	if out.Written() == 0 {
		return driver.DocumentErrorf("pcl: no pages")
	}
	if _, err := out.Write(end); err != nil {
		return out.Abort(err, nil)
	}
	return nil
}

// page returns page n as a bitmap of the logical page: the sheet placed at dpi, binarized and
// cropped to the logical page's columns.
func (d *Driver) page(job driver.JobInfo, pg raster.Page, p paper, dpi, n int) (*imaging.Bitmap, error) {
	pg = imaging.GrayFromRGB(pg)
	o := d.placeOptions(job, dpi)
	o.InPlace = true
	sheet, err := imaging.Place(pg, job.Media, o)
	if err != nil {
		return nil, driver.DocumentError(fmt.Errorf("pcl: page %d: %w", n, err))
	}
	defer job.Budget.Release(int64(sheet.Width) * int64(sheet.Height))
	off, lw := p.logical(dpi)
	if off+lw > sheet.Width {
		return nil, fmt.Errorf("pcl: page %d: logical page %d+%d dots wider than the %d-dot sheet", n, off, lw, sheet.Width)
	}
	// Crop in place: each row moves to an earlier or equal index, so a forward copy is safe.
	for y := 0; y < sheet.Height; y++ {
		copy(sheet.Gray[y*lw:(y+1)*lw], sheet.Gray[y*sheet.Width+off:y*sheet.Width+off+lw])
	}
	sheet.Gray, sheet.Width = sheet.Gray[:lw*sheet.Height], lw
	bm, dithered, err := imaging.Binarize(sheet, d.dither)
	if err != nil {
		return nil, fmt.Errorf("pcl: page %d: %w", n, err)
	}
	if dithered {
		job.Log("page %d: dithered", n)
	}
	if job.Debug != nil {
		job.DebugImage(fmt.Sprintf("p%d", n), bm.Image())
	}
	return bm, nil
}

// pjlName makes job-name safe inside @PJL JOB NAME="...": printable ASCII without quotes, so a
// client cannot end the string or start a new PJL line.
func pjlName(job driver.JobInfo) string {
	var b strings.Builder
	for _, r := range job.Name {
		if r >= 0x20 && r < 0x7f && r != '"' && b.Len() < 64 {
			b.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		return s
	}
	return fmt.Sprintf("job %d", job.ID)
}
