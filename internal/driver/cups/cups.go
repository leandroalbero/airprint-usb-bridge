// Package cups is the cups driver: any CUPS raster driver run without cupsd. It reads the PPD
// given with --ppd, writes each job as CUPS raster v3 for the PPD's options, and runs the PPD's
// raster filter (rastertohp, rastertolabel, a vendor filter, ...) on it the way cupsd would.
package cups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// Option defaults, also shown by register.go. The filter's output streams to the printer, so
// max-output only stops a runaway filter; it is not held in memory.
const (
	defaultTimeout   = 5 * time.Minute
	defaultMaxOutput = "16G"
	defaultDeviceURI = "file:///dev/null"
)

// Driver runs a CUPS raster filter.
type Driver struct {
	ppd      *PPD
	ppdPath  string
	caps     driver.Capabilities
	filter   string // absolute path; "" sends the raster itself (*cupsFilter program "-")
	final    string // FINAL_CONTENT_TYPE
	dither   string
	fullPage bool
	duplex   bool
	backSide string
	uri      string
	timeout  time.Duration
	maxOut   int64

	sizes      []pageSize     // advertised, in Capabilities.Media order
	byName     map[string]int // Media.Name -> sizes index
	res        []resChoice    // advertised, default first
	userOpts   map[string]string
	colorModel string // ColorModel choice picked for gray input; "" keeps the PPD's
}

type pageSize struct {
	choice        string     // PPD PageSize keyword
	width, length float64    // points
	area          [4]float64 // ImageableArea: left, bottom, right, top in points
	media         driver.Media
}

type resChoice struct {
	choice string // PPD Resolution keyword; "" when the PPD has no Resolution option
	res    driver.Resolution
}

// pwgSizes are the names given to PPD page sizes that match them within driver.SizeTolerance;
// other sizes get a custom om_ name.
// errSheetTooLarge marks a page size and resolution whose single sheet is over
// driver.MaxSheetBytes.
var errSheetTooLarge = errors.New("sheet too large")

var pwgSizes = []string{
	driver.MediaLetter, driver.MediaLegal, driver.MediaExecutive, driver.MediaA4, driver.MediaA5,
	driver.MediaA6, driver.Media4x6, "iso_a3_297x420mm", "na_ledger_11x17in", "na_invoice_5.5x8.5in",
	"na_index-3x5_3x5in", "na_5x7_5x7in", "na_index-5x8_5x8in", "na_govt-letter_8x10in",
	"na_foolscap_8.5x13in", "na_number-10_4.125x9.5in", "na_monarch_3.875x7.5in",
	"iso_b5_176x250mm", "jis_b5_182x257mm", "iso_dl_110x220mm", "iso_c5_162x229mm",
	"iso_c6_114x162mm", "jpn_hagaki_100x148mm", "oe_photo-l_3.5x5in",
}

// New builds the driver from cfg.PPD and the driver options.
func New(cfg driver.Config) (driver.Driver, error) {
	if cfg.PPD == "" {
		return nil, errors.New("the cups driver needs --ppd FILE")
	}
	path, err := filepath.Abs(cfg.PPD)
	if err != nil {
		return nil, err
	}
	ppd, err := LoadPPD(path)
	if err != nil {
		return nil, err
	}
	d := &Driver{ppd: ppd, ppdPath: path, dither: cfg.Dither, userOpts: map[string]string{}}

	if d.timeout, err = time.ParseDuration(cfg.Option("timeout", defaultTimeout.String())); err != nil || d.timeout <= 0 {
		return nil, fmt.Errorf("option timeout=%q: want a positive duration such as 90s or 5m", cfg.Option("timeout", ""))
	}
	if d.maxOut, err = parseSize(cfg.Option("max-output", defaultMaxOutput)); err != nil {
		return nil, fmt.Errorf("option max-output: %w", err)
	}
	if d.fullPage, err = cfg.BoolOption("full-page", false); err != nil {
		return nil, err
	}
	d.uri = cfg.Option("device-uri", defaultDeviceURI)
	if err := d.parseUserOptions(cfg.Option("options", "")); err != nil {
		return nil, err
	}
	if err := d.findFilter(cfg); err != nil {
		return nil, err
	}
	d.backSide = "Normal"
	if v, ok := ppd.Attr("cupsBackSide", ""); ok {
		d.backSide = v
	} else if v, ok := ppd.Attr("cupsFlipDuplex", ""); ok && strings.EqualFold(v, "true") {
		d.backSide = "Rotated"
	}

	kind := cfg.Option("kind", "auto")
	label := false
	switch kind {
	case "auto":
		model, _ := ppd.Attr("ModelName", "")
		label = strings.Contains(filepath.Base(d.filter), "label") || strings.Contains(strings.ToLower(model), "label")
	case "label":
		label = true
	case "document":
	default:
		return nil, fmt.Errorf("option kind=%q: want auto, document or label", kind)
	}
	if err := d.loadSizes(label); err != nil {
		return nil, err
	}
	def, err := d.defaultSize(cfg.Media)
	if err != nil {
		return nil, err
	}
	if err := d.loadResolutions(); err != nil {
		return nil, err
	}
	d.pickColorModel(cfg)
	if def, err = d.checkFormats(cfg, def); err != nil {
		return nil, err
	}

	switch v := cfg.Option("duplex", "auto"); v {
	case "auto":
		o := ppd.Option("Duplex")
		d.duplex = o.Choice("DuplexNoTumble") != nil || o.Choice("DuplexTumble") != nil
	default:
		if d.duplex, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("option duplex=%q: want auto, true or false", v)
		}
		if d.duplex && ppd.Option("Duplex") == nil {
			return nil, errors.New("option duplex=true: the PPD has no Duplex option")
		}
	}

	d.caps = d.capabilities(cfg, label, def)
	cfg.Log("cups: %s, filter %s, %d page sizes, %v dpi", d.caps.MakeModel, d.filterName(), len(d.sizes), d.caps.Resolutions)
	return d, nil
}

func (d *Driver) Name() string                      { return "cups" }
func (d *Driver) Capabilities() driver.Capabilities { return d.caps }

func (d *Driver) filterName() string {
	if d.filter == "" {
		return "- (raw CUPS raster)"
	}
	return filepath.Base(d.filter)
}

// parseUserOptions reads --driver-opt options="Key=Choice ...", PPD options that stay fixed for
// every job. The page size and duplex mode come from each job instead.
func (d *Driver) parseUserOptions(s string) error {
	for _, kv := range strings.Fields(s) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || v == "" {
			return fmt.Errorf("option options: %q is not Key=Choice", kv)
		}
		o := d.ppd.Option(k)
		if o == nil {
			for _, oo := range d.ppd.Options {
				if strings.EqualFold(oo.Keyword, k) {
					o = oo
				}
			}
		}
		if o == nil {
			var keys []string
			for _, oo := range d.ppd.Options {
				keys = append(keys, oo.Keyword)
			}
			return fmt.Errorf("option options: the PPD has no option %s (have %s)", k, strings.Join(keys, ", "))
		}
		switch o.Keyword {
		case "PageSize", "PageRegion":
			return fmt.Errorf("option options: set the page size with --media, not %s", o.Keyword)
		case "Duplex":
			return errors.New("option options: Duplex follows each job's sides; use --driver-opt duplex=false to turn it off")
		}
		c := o.Choice(v)
		if c == nil {
			for _, cc := range o.Choices {
				if strings.EqualFold(cc.Name, v) {
					c = cc
				}
			}
		}
		if c == nil {
			var names []string
			for _, cc := range o.Choices {
				names = append(names, cc.Name)
			}
			return fmt.Errorf("option options: %s has no choice %s (have %s)", o.Keyword, v, strings.Join(names, ", "))
		}
		d.userOpts[o.Keyword] = c.Name
	}
	return nil
}

func (d *Driver) findFilter(cfg driver.Config) error {
	program := cfg.Option("filter", "")
	d.final = "printer/" + printerName
	if program == "" {
		f, err := d.ppd.RasterFilter()
		if err != nil {
			return err
		}
		program = f.Program
		if f.Dest != "" {
			d.final = f.Dest
		}
	}
	if program == "-" {
		d.filter = ""
		return nil
	}
	dirs := DefaultFilterDirs
	if v := cfg.Option("filter-dir", ""); v != "" {
		dirs = filepath.SplitList(v)
	}
	path, err := findFilter(program, dirs)
	if err != nil {
		return err
	}
	d.filter = path
	return nil
}

// loadSizes turns the PageSize choices into media. Sizes without dimensions are skipped, and
// so are later sizes with the same name (Letter.Fullbleed after Letter).
func (d *Driver) loadSizes(label bool) error {
	o := d.ppd.Option("PageSize")
	if o == nil || len(o.Choices) == 0 {
		return errors.New("PPD has no PageSize option")
	}
	mediaType := "stationery"
	if label {
		mediaType = "labels"
	}
	d.byName = map[string]int{}
	choices := append([]*Choice(nil), o.Choices...)
	sort.SliceStable(choices, func(a, b int) bool {
		return !strings.Contains(choices[a].Name, ".") && strings.Contains(choices[b].Name, ".")
	})
	for _, c := range choices {
		w, l, ok := d.paperDimension(c)
		if !ok || w < 1 || l < 1 {
			continue
		}
		area := [4]float64{0, 0, w, l}
		if v, ok := d.ppd.Attr("ImageableArea", c.Name); ok {
			if n, ok := numbers(v, 4); ok && n[0] >= 0 && n[1] >= 0 && n[2] > n[0] && n[3] > n[1] {
				area = [4]float64{n[0], n[1], math.Min(n[2], w), math.Min(n[3], l)}
			}
		}
		wh, lh := ptToHmm(w), ptToHmm(l)
		name := driver.MediaName(wh, lh)
		for _, pwg := range pwgSizes {
			if x, y, _ := driver.ParseMediaName(pwg); abs(x-wh) <= driver.SizeTolerance && abs(y-lh) <= driver.SizeTolerance {
				name, wh, lh = pwg, x, y
				break
			}
		}
		if _, dup := d.byName[name]; dup {
			continue
		}
		m := driver.Media{
			Name: name, Width: wh, Length: lh, Type: mediaType, Source: "main",
			Margins: driver.Margins{
				Left: ptToHmm(area[0]), Bottom: ptToHmm(area[1]),
				Right: ptToHmm(w - area[2]), Top: ptToHmm(l - area[3]),
			},
		}
		if m.Margins.Left+m.Margins.Right >= m.Width || m.Margins.Top+m.Margins.Bottom >= m.Length {
			m.Margins = driver.Margins{}
		}
		d.byName[name] = len(d.sizes)
		d.sizes = append(d.sizes, pageSize{choice: c.Name, width: w, length: l, area: area, media: m})
	}
	if len(d.sizes) == 0 {
		return errors.New("PPD has no PageSize with a PaperDimension")
	}
	return nil
}

// paperDimension is the size of a PageSize choice: its *PaperDimension, else the PageSize
// array in its code.
func (d *Driver) paperDimension(c *Choice) (w, l float64, ok bool) {
	if v, ok := d.ppd.Attr("PaperDimension", c.Name); ok {
		if n, ok := numbers(v, 2); ok {
			return n[0], n[1], true
		}
	}
	h := Header{}
	if applyCode(&h, c.Code) == nil && h.PageSize[0] > 0 && h.PageSize[1] > 0 {
		return float64(h.PageSize[0]), float64(h.PageSize[1]), true
	}
	return 0, 0, false
}

// defaultSize marks the --media size (a PPD keyword or a PWG name), else the PPD default, as
// the loaded medium.
func (d *Driver) defaultSize(want string) (int, error) {
	find := func(s string) (int, bool) {
		if i, ok := d.byName[s]; ok {
			return i, true
		}
		for i, z := range d.sizes {
			if strings.EqualFold(z.choice, s) {
				return i, true
			}
		}
		// A size dropped as a duplicate maps to the one kept.
		if c := d.ppd.Option("PageSize").Choice(s); c != nil {
			if w, l, ok := d.paperDimension(c); ok {
				for i, z := range d.sizes {
					if z.width == w && z.length == l {
						return i, true
					}
				}
			}
		}
		return 0, false
	}
	if want != "" {
		i, ok := find(want)
		if !ok {
			return 0, fmt.Errorf("media %q is not a PageSize of the PPD (have %s)", want, strings.Join(d.sizeNames(), ", "))
		}
		return i, nil
	}
	if i, ok := find(d.ppd.Option("PageSize").Default); ok {
		return i, nil
	}
	return 0, nil
}

func (d *Driver) sizeNames() []string {
	var out []string
	for _, z := range d.sizes {
		out = append(out, z.choice)
	}
	return out
}

// loadResolutions reads the Resolution choices, default first. HWResolution in a choice's code
// wins over its name.
func (d *Driver) loadResolutions() error {
	o := d.ppd.Option("Resolution")
	if o == nil || len(o.Choices) == 0 {
		v, _ := d.ppd.Attr("DefaultResolution", "")
		x, y, ok := parseResolution(v)
		if !ok {
			h := d.headerFor(nil)
			if h.HWResolution == defaultHeader().HWResolution {
				return errors.New("PPD has no Resolution option or *DefaultResolution")
			}
			x, y = int(h.HWResolution[0]), int(h.HWResolution[1])
		}
		d.res = []resChoice{{res: driver.Resolution{X: x, Y: y}}}
		return nil
	}
	pinned := d.userOpts["Resolution"]
	seen := map[driver.Resolution]bool{}
	for _, c := range o.Choices {
		if pinned != "" && c.Name != pinned {
			continue
		}
		x, y, ok := parseResolution(c.Name)
		h := Header{}
		if applyCode(&h, c.Code) == nil && h.HWResolution[0] > 0 && h.HWResolution[1] > 0 {
			x, y, ok = int(h.HWResolution[0]), int(h.HWResolution[1]), true
		}
		r := driver.Resolution{X: x, Y: y}
		if !ok || seen[r] {
			continue
		}
		seen[r] = true
		rc := resChoice{choice: c.Name, res: r}
		if c.Name == o.Default {
			d.res = append([]resChoice{rc}, d.res...)
		} else {
			d.res = append(d.res, rc)
		}
	}
	if len(d.res) == 0 {
		return errors.New("PPD Resolution choices give no usable resolution")
	}
	return nil
}

// pickColorModel prefers a gray ColorModel choice when the PPD default is colour: the input is
// gray, and printing it with black ink alone is sharper and cheaper.
func (d *Driver) pickColorModel(cfg driver.Config) {
	o := d.ppd.Option("ColorModel")
	if o == nil || d.userOpts["ColorModel"] != "" {
		return
	}
	if h := d.headerFor(nil); h.gray() {
		return
	}
	for _, c := range o.Choices {
		h := d.headerFor(map[string]string{"ColorModel": c.Name})
		if h.gray() {
			d.colorModel = c.Name
			cfg.Log("cups: using ColorModel %s for gray input", c.Name)
			return
		}
	}
}

// checkFormats drops resolutions whose raster format the encoder cannot write or whose default
// sheet is too large, and fails when none is left. It then drops page sizes too large even at the
// lowest resolution left, and returns the default size's new index.
func (d *Driver) checkFormats(cfg driver.Config, def int) (int, error) {
	var keep []resChoice
	var errs []string
	for _, r := range d.res {
		if _, _, err := d.header(d.sizes[def], r, ""); err != nil {
			errs = append(errs, fmt.Sprintf("%dx%ddpi: %v", r.res.X, r.res.Y, err))
			continue
		}
		keep = append(keep, r)
	}
	if len(keep) == 0 {
		return 0, fmt.Errorf("PPD raster format not supported: %s", strings.Join(errs, "; "))
	}
	for _, e := range errs {
		cfg.Log("cups: skipping resolution %s", e)
	}
	d.res = keep
	low := d.res[0]
	for _, r := range d.res[1:] {
		if r.res.X*r.res.Y < low.res.X*low.res.Y {
			low = r
		}
	}
	defName := d.sizes[def].media.Name
	var sizes []pageSize
	byName := map[string]int{}
	for _, z := range d.sizes {
		if _, _, err := d.header(z, low, ""); err != nil {
			cfg.Log("cups: skipping page size %s: %v", z.choice, err)
			continue
		}
		byName[z.media.Name] = len(sizes)
		sizes = append(sizes, z)
	}
	d.sizes, d.byName = sizes, byName
	return byName[defName], nil
}

// fittingResolution is r, or when a sheet of size is too large at r, the highest resolution
// below it that fits.
func (d *Driver) fittingResolution(size pageSize, r resChoice, sides string) resChoice {
	if _, _, err := d.header(size, r, sides); !errors.Is(err, errSheetTooLarge) {
		return r
	}
	best, found := r, false
	for _, c := range d.res {
		if a := c.res.X * c.res.Y; a < r.res.X*r.res.Y && (!found || a > best.res.X*best.res.Y) {
			if _, _, err := d.header(size, c, sides); err == nil {
				best, found = c, true
			}
		}
	}
	return best
}

func (d *Driver) capabilities(cfg driver.Config, label bool, def int) driver.Capabilities {
	model, ok := d.ppd.Attr("ModelName", "")
	if !ok || model == "" {
		model, _ = d.ppd.Attr("NickName", "")
	}
	if model == "" {
		model = "CUPS raster printer"
	}
	media := make([]driver.Media, len(d.sizes))
	for i, z := range d.sizes {
		media[i] = z.media
	}
	media[def].Ready = true

	var res []driver.Resolution
	var dpis []int
	// Clients send pages at the first advertised resolution, and every page is decoded and
	// converted at it, so the lowest resolution of at least 300 dpi goes first. A job that
	// names no resolution still prints at the PPD default.
	first := 0
	for i, r := range d.res {
		if r.res.X >= 300 && r.res.Y >= 300 && (d.res[first].res.X < 300 || d.res[first].res.Y < 300 ||
			r.res.X*r.res.Y < d.res[first].res.X*d.res[first].res.Y) {
			first = i
		}
	}
	res = append(res, d.res[first].res)
	for i, r := range d.res {
		if i != first {
			res = append(res, r.res)
		}
	}
	for _, r := range d.res {
		for _, v := range []int{r.res.X, r.res.Y} {
			if !slices.Contains(dpis, v) {
				dpis = append(dpis, v)
			}
		}
	}
	slices.Sort(dpis)
	rs := "RS"
	for i, v := range dpis {
		if i > 0 {
			rs += "-"
		}
		rs += strconv.Itoa(v)
	}
	urf := []string{"V1.4", "W8", "CP99"}
	if d.duplex {
		urf = append(urf, "DM1")
	}
	urf = append(urf, "IS1", "MT1", rs)

	qualities := []int{driver.QualityNormal}
	if cfg.HighRes && len(d.res) > 1 {
		qualities = append(qualities, driver.QualityHigh)
	}
	kind := []string{"document"}
	if label {
		kind = []string{"label"}
	}
	return driver.Capabilities{
		MakeModel:    model,
		Kind:         kind,
		Duplex:       d.duplex,
		Resolutions:  res,
		Qualities:    qualities,
		URF:          urf,
		PWGTypes:     []string{"sgray_8"},
		MaxCopies:    99,
		CopiesNative: false,
		Media:        media,
		DefaultMedia: media[def].Name,
	}
}

// choices are the PPD option choices for one job: the PPD defaults, then --driver-opt options,
// then what the job decides.
func (d *Driver) choices(size pageSize, r resChoice, sides string) map[string]string {
	sel := map[string]string{}
	for k, v := range d.userOpts {
		sel[k] = v
	}
	if d.colorModel != "" {
		sel["ColorModel"] = d.colorModel
	}
	sel["PageSize"] = size.choice
	if d.ppd.Option("PageRegion").Choice(size.choice) != nil {
		sel["PageRegion"] = size.choice
	}
	if r.choice != "" {
		sel["Resolution"] = r.choice
	}
	if o := d.ppd.Option("Duplex"); o != nil {
		choice := "None"
		if d.duplex {
			switch sides {
			case "two-sided-long-edge":
				choice = "DuplexNoTumble"
			case "two-sided-short-edge":
				choice = "DuplexTumble"
			}
		}
		if o.Choice(choice) != nil {
			sel["Duplex"] = choice
		}
	}
	return sel
}

// headerFor applies the code of every option's selected choice, in OrderDependency order, to
// the CUPS default header, as cupsRasterInterpretPPD does.
func (d *Driver) headerFor(sel map[string]string) Header {
	h := defaultHeader()
	for _, o := range d.ppd.ordered() {
		name := o.Default
		if v, ok := d.userOpts[o.Keyword]; ok {
			name = v
		}
		if v, ok := sel[o.Keyword]; ok {
			name = v
		}
		if c := o.Choice(name); c != nil {
			// Code that is not a parsable setpagedevice dictionary (JCL, procedures) does
			// not touch the raster header.
			_ = applyCode(&h, c.Code)
		}
	}
	return h
}

// header is the page header for a job's size, resolution and sides, with the geometry of the
// imageable area (or the whole page with full-page=true).
func (d *Driver) header(size pageSize, r resChoice, sides string) (Header, format, error) {
	h := d.headerFor(d.choices(size, r, sides))
	// loadResolutions read r from the choice's HWResolution when it has one, else from its name.
	h.HWResolution = [2]uint32{uint32(r.res.X), uint32(r.res.Y)}
	area := size.area
	if d.fullPage {
		area = [4]float64{0, 0, size.width, size.length}
	}
	// cupsRasterInterpretPPD truncates here (841.89 pt gives 841), but rastertohp and similar
	// filters pick the paper by exact point size and would not recognise A4.
	h.PageSize = [2]uint32{uint32(size.width + 0.5), uint32(size.length + 0.5)}
	h.CupsPageSize = [2]float32{float32(size.width), float32(size.length)}
	for i, v := range area {
		h.ImagingBoundingBox[i] = uint32(v)
		h.CupsImagingBBox[i] = float32(v)
	}
	h.Margins = [2]uint32{uint32(area[0]), uint32(area[1])}
	h.CupsWidth = uint32((area[2]-area[0])*float64(r.res.X)/72 + 0.5)
	h.CupsHeight = uint32((area[3]-area[1])*float64(r.res.Y)/72 + 0.5)
	h.CupsPageSizeName = size.choice
	h.NumCopies = 1
	if h.CupsBorderlessScalingFactor == 0 {
		h.CupsBorderlessScalingFactor = 1
	}
	if h.CupsWidth == 0 || h.CupsHeight == 0 || h.CupsWidth > 100000 || h.CupsHeight > 1000000 {
		return Header{}, format{}, fmt.Errorf("page %s at %dx%d dpi is %dx%d pixels", size.choice, r.res.X, r.res.Y, h.CupsWidth, h.CupsHeight)
	}
	f, err := layout(&h, int(h.CupsWidth), int(h.CupsHeight))
	if err != nil {
		return Header{}, format{}, err
	}
	sw, sh := imaging.SheetDots(size.sheet(), int(r.res.X), int(r.res.Y))
	if sheet, page := int64(sw)*int64(sh), int64(f.bytesPerRow)*int64(f.rows); sheet > driver.MaxSheetBytes || page > 2*driver.MaxSheetBytes {
		return Header{}, format{}, fmt.Errorf("%w: page %s at %dx%d dpi needs %d MiB of gray and %d MiB of raster, over the %d MiB a job may use",
			errSheetTooLarge, size.choice, r.res.X, r.res.Y, sheet>>20, page>>20, driver.MaxSheetBytes>>20)
	}
	h.CupsNumColors = uint32(f.colors)
	h.CupsBitsPerPixel = uint32(f.bitsPerPix)
	h.CupsBytesPerLine = uint32(f.bytesPerRow)
	return h, f, nil
}

func (d *Driver) sizeFor(m driver.Media) pageSize {
	if i, ok := d.byName[m.Name]; ok {
		return d.sizes[i]
	}
	for _, z := range d.sizes {
		if abs(z.media.Width-m.Width) <= driver.SizeTolerance && abs(z.media.Length-m.Length) <= driver.SizeTolerance {
			return z
		}
	}
	return d.sizes[d.byName[d.caps.DefaultMedia]]
}

func (d *Driver) resolutionFor(job driver.JobInfo) resChoice {
	if job.Resolution.X > 0 {
		for _, r := range d.res {
			if r.res == job.Resolution {
				return r
			}
		}
	}
	if job.Quality == driver.QualityHigh && slices.Contains(d.caps.Qualities, driver.QualityHigh) {
		best := d.res[0]
		for _, r := range d.res[1:] {
			if r.res.X*r.res.Y > best.res.X*best.res.Y {
				best = r
			}
		}
		return best
	}
	return d.res[0]
}

// Render is the job rendered in memory; the bridge streams it instead (Stream).
func (d *Driver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	return driver.RenderStream(ctx, d, job, pages)
}

// jobPlan is the page setup of one job.
type jobPlan struct {
	size  pageSize
	r     resChoice
	sides string
	h     Header
	f     format
	// fitted is the resolution asked for when it was too large for the size.
	fitted *resChoice
}

func (d *Driver) plan(job driver.JobInfo) (jobPlan, error) {
	pl := jobPlan{size: d.sizeFor(job.Media), r: d.resolutionFor(job), sides: job.Sides}
	if !d.duplex {
		pl.sides = "one-sided"
	}
	if fit := d.fittingResolution(pl.size, pl.r, pl.sides); fit != pl.r {
		asked := pl.r
		pl.fitted, pl.r = &asked, fit
	}
	var err error
	pl.h, pl.f, err = d.header(pl.size, pl.r, pl.sides)
	return pl, err
}

func (d *Driver) placeOptions(job driver.JobInfo, h Header) imaging.PlaceOptions {
	return imaging.PlaceOptions{
		XDPI: int(h.HWResolution[0]), YDPI: int(h.HWResolution[1]),
		Scaling: job.Scaling, Orientation: job.Orientation, Budget: job.Budget,
	}
}

// CheckPage checks that page n is well formed and can be placed on the job's sheet.
func (d *Driver) CheckPage(job driver.JobInfo, n int, p raster.Page) error {
	if p.Width <= 0 || p.Height <= 0 || (p.Gray != nil && len(p.Gray) != p.Width*p.Height) || p.XDPI <= 0 || p.YDPI <= 0 {
		return driver.DocumentErrorf("page %d: malformed %dx%d page at %dx%d dpi", n, p.Width, p.Height, p.XDPI, p.YDPI)
	}
	pl, err := d.plan(job)
	if err != nil {
		return nil // Plan refuses the job
	}
	if err := imaging.CheckPlace(p, pl.size.sheet(), d.placeOptions(job, pl.h)); err != nil {
		return driver.DocumentError(fmt.Errorf("page %d: %w", n, err))
	}
	return nil
}

// Plan makes one unit of the whole job: the filter's output for every page.
func (d *Driver) Plan(job driver.JobInfo, pages int) ([]driver.Unit, error) {
	if pages == 0 {
		return nil, driver.DocumentErrorf("no pages")
	}
	pl, err := d.plan(job)
	if pl.fitted != nil {
		job.Log("cups: %s is too large at %dx%d dpi; using %dx%d dpi", pl.size.choice, pl.fitted.res.X, pl.fitted.res.Y, pl.r.res.X, pl.r.res.Y)
	}
	if err != nil {
		return nil, err
	}
	h := pl.h
	job.Log("cups: %s at %dx%d dpi, %d pages, %d-bit colour space %d, %dx%d pixels",
		pl.size.choice, pl.r.res.X, pl.r.res.Y, pages, h.CupsBitsPerColor, h.CupsColorSpace, h.CupsWidth, h.CupsHeight)
	label := fmt.Sprintf("%s job (%d pages)", d.filterName(), pages)
	if pages == 1 {
		label = d.filterName() + " job (1 page)"
	}
	return []driver.Unit{{Label: label, Impressions: pages, Meta: pl}}, nil
}

// Stream writes the pages as CUPS raster, converting each when the filter reads that far, and
// passes the filter's output on as it comes. When a page fails or the job is cancelled, the
// filter's input ends there, so the filter can still finish the device job for the pages before it.
func (d *Driver) Stream(ctx context.Context, job driver.JobInfo, u driver.Unit, pages driver.PageReader, w io.Writer) error {
	pl, ok := u.Meta.(jobPlan)
	if !ok {
		return fmt.Errorf("cups: unit %q was not planned by this driver", u.Label)
	}
	out := driver.NewOutput(ctx, w)
	var buf []byte
	stream := &rasterStream{ctx: ctx, next: func(n int) ([]byte, error) {
		p, err := pages.Next()
		if err != nil {
			return nil, err
		}
		buf, err = d.page(job, p, n, pl, buf[:0])
		return buf, err
	}}
	var err error
	if d.filter == "" {
		cw := &outWriter{w: out, max: d.maxOut, cancel: func() {}, act: &activity{}}
		_, err = io.Copy(cw, stream)
		if _, _, over := cw.result(); over {
			err = fmt.Errorf("raster exceeds %d bytes (raise --driver-opt max-output)", d.maxOut)
		}
	} else {
		sel := d.choices(pl.size, pl.r, pl.sides)
		keys := make([]string, 0, len(sel))
		for k := range sel {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		run := filterRun{
			path: d.filter,
			args: []string{
				strconv.Itoa(job.ID), argText(job.User, "anonymous"), argText(job.Name, "untitled"),
				"1", optionString(keys, sel),
			},
			env:       filterEnv(d.ppdPath, d.final, d.uri, d.filter),
			timeout:   d.timeout,
			maxOutput: d.maxOut,
			grace:     cancelGrace,
			logf:      job.Logf,
			message:   job.StateMessage,
		}
		_, err = run.run(ctx, stream, out)
		if serr := stream.failure(); serr != nil {
			err = serr
		}
	}
	if err != nil {
		return out.Abort(err, nil)
	}
	return nil
}

// sheet is the media Place lays pages on: the exact paper size, not the advertised one.
func (z pageSize) sheet() driver.Media {
	m := z.media
	m.Width, m.Length = ptToHmm(z.width), ptToHmm(z.length)
	return m
}

// page converts page n (from 1) to its raster header and data, appended to dst.
func (d *Driver) page(job driver.JobInfo, pg raster.Page, n int, pl jobPlan, dst []byte) ([]byte, error) {
	h, f, size := pl.h, pl.f, pl.size
	xres, yres := int(h.HWResolution[0]), int(h.HWResolution[1])
	o := d.placeOptions(job, h)
	o.InPlace = true
	sheet, err := imaging.Place(pg, size.sheet(), o)
	if err != nil {
		return nil, driver.DocumentError(fmt.Errorf("page %d: %w", n, err))
	}
	defer job.Budget.Release(int64(sheet.Width) * int64(sheet.Height))
	area := size.area
	if d.fullPage {
		area = [4]float64{0, 0, size.width, size.length}
	}
	x0 := int(math.Round(area[0] * float64(xres) / 72))
	y0 := int(math.Round((size.length - area[3]) * float64(yres) / 72))
	if h.Duplex && n%2 == 0 {
		// The printer applies the front's margins to the back in its own frame, so the
		// whole sheet is flipped before cropping: with unequal margins, flipping the cropped
		// area instead shifts the back page by their difference.
		fx, fy := backSideFlip(d.backSide, h.Tumble)
		flip(sheet, fx, fy)
	}
	p := cropInPlace(sheet, x0, y0, int(h.CupsWidth), int(h.CupsHeight))
	var bm *imaging.Bitmap
	if h.CupsBitsPerColor == 1 {
		var dithered bool
		bm, dithered, err = imaging.Binarize(p, d.dither)
		if err != nil {
			return nil, err
		}
		job.Log("page %d: %dx%d pixels, dithered %v", n, p.Width, p.Height, dithered)
		if job.Debug != nil {
			job.DebugImage(fmt.Sprintf("p%d", n), bm.Image())
		}
	} else {
		job.Log("page %d: %dx%d pixels", n, p.Width, p.Height)
		if job.Debug != nil {
			job.DebugImage(fmt.Sprintf("p%d", n), imaging.GrayImage(p))
		}
	}
	if need := HeaderSize + int(h.CupsBytesPerLine)*f.rows; cap(dst) < need {
		dst = make([]byte, 0, need)
	}
	dst = append(dst, h.Marshal(byteOrder)...)
	return encodePage(dst, &h, f, p, bm), nil
}

// backSideFlip is how a duplex back page is pre-transformed for a PPD's *cupsBackSide, so the
// printer's own turn of the sheet leaves it upright.
func backSideFlip(backSide string, tumble bool) (x, y bool) {
	switch strings.ToLower(backSide) {
	case "rotated":
		return !tumble, !tumble
	case "flipped":
		return tumble, !tumble
	case "manualtumble":
		return tumble, tumble
	}
	return false, false
}

// rasterStream is the filter's stdin: the sync word, then each page converted when the filter
// reads that far, so only one page of raster is held in memory. next gets the page number.
type rasterStream struct {
	ctx  context.Context
	next func(n int) ([]byte, error)

	mu      sync.Mutex
	n       int
	started bool
	buf     []byte
	err     error
}

func (r *rasterStream) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.buf) == 0 {
		if r.err != nil {
			if r.err == io.EOF {
				return 0, io.EOF
			}
			return 0, r.err
		}
		if !r.started {
			r.started = true
			r.buf = syncV3()
			break
		}
		if err := r.ctx.Err(); err != nil {
			r.err = err
			return 0, err
		}
		r.n++
		b, err := r.next(r.n)
		if err != nil {
			r.err = err
			continue
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// failure is the error that stopped the stream early, if any.
func (r *rasterStream) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == io.EOF {
		return nil
	}
	return r.err
}

func ptToHmm(pt float64) int { return int(math.Round(pt * 2540 / 72)) }

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// parseSize reads a byte count with an optional K, M or G suffix (binary units).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'k', 'K':
			mult, s = 1<<10, s[:n-1]
		case 'm', 'M':
			mult, s = 1<<20, s[:n-1]
		case 'g', 'G':
			mult, s = 1<<30, s[:n-1]
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v > (1<<40)/mult {
		return 0, fmt.Errorf("%q: want a positive size such as 96M", s)
	}
	return v * mult, nil
}
