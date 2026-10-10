// Package zpl is the Zebra ZPL II driver: each page becomes one label format (^XA … ^XZ) holding
// the label as ^GFA graphic fields, Z64 (zlib + base64 + CRC) or ZPL's compressed ASCII hex. It
// follows Zebra's public ZPL II programming guide.
package zpl

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

const Name = "zpl"

// Media tracking (^MN) for labels.
const (
	TrackGap  = "gap"
	TrackMark = "mark"
)

// ^GFA data encodings.
const (
	EncodingZ64 = "z64"
	EncodingHex = "hex"
)

// MaxDots is the largest ^LL (label length) and ^FO position ZPL accepts.
const MaxDots = 32000

// headDots is the print width of the usual 4 inch head at each resolution.
var headDots = map[int]int{203: 832, 300: 1248, 600: 2496}

// Options configure the driver.
type Options struct {
	DPI        int    // 203, 300 or 600
	Darkness   int    // ~SD 0-30; < 0 leaves the printer's setting
	Tracking   string // TrackGap or TrackMark (die-cut media); continuous media is ^MNN
	Encoding   string // EncodingZ64 or EncodingHex
	PrintWidth int    // widest label in dots; 0 = the 4 inch head at DPI
	Media      string // --media id, see Info.Media; "" = 4x6
	Dither     string // "auto", "on", "off" or ""
	MakeModel  string // "" = "Zebra ZPL Label Printer"
}

type Driver struct {
	opts       Options
	continuous bool
	caps       driver.Capabilities
}

// NewFromConfig builds the driver from bridge settings and the options listed in its Info.
func NewFromConfig(c driver.Config) (*Driver, error) {
	dpi, err := c.IntOption("dpi", 203)
	if err != nil {
		return nil, err
	}
	darkness := -1
	if v := strings.TrimSpace(c.Option("darkness", "")); v != "" {
		if darkness, err = strconv.Atoi(v); err != nil || darkness < 0 || darkness > 30 {
			return nil, fmt.Errorf("option darkness=%q: want 0-30", v)
		}
	}
	width, err := c.IntOption("print-width", 0)
	if err != nil {
		return nil, err
	}
	if width < 0 {
		return nil, fmt.Errorf("option print-width=%d: want a positive number of dots", width)
	}
	o := Options{
		DPI:        dpi,
		Darkness:   darkness,
		Tracking:   c.Option("tracking", TrackGap),
		Encoding:   c.Option("encoding", EncodingZ64),
		PrintWidth: width,
		Media:      c.Media,
		Dither:     c.Dither,
	}
	if id := c.DeviceID; !id.Empty() {
		if mdl := id.Model(); mdl != "" {
			mfg := id.Manufacturer()
			if strings.Contains(strings.ToLower(mfg), "zebra") || mfg == "" {
				mfg = "Zebra"
			}
			o.MakeModel = mfg + " " + mdl
		}
		if len(id.CommandSet()) > 0 && !id.HasCommand(func(t string) bool { return strings.Contains(t, "ZPL") }) {
			c.Log("zpl: the printer's command set %v does not list ZPL; EPL-only models will print the commands as text", id.CommandSet())
		}
	}
	return New(o)
}

// New builds the driver.
func New(o Options) (*Driver, error) {
	if _, ok := headDots[o.DPI]; !ok {
		return nil, fmt.Errorf("dpi %d: want 203, 300 or 600", o.DPI)
	}
	if o.Darkness > 30 {
		return nil, fmt.Errorf("darkness %d: want 0-30", o.Darkness)
	}
	switch o.Tracking {
	case "":
		o.Tracking = TrackGap
	case TrackGap, TrackMark:
	default:
		return nil, fmt.Errorf("tracking %q: want gap or mark", o.Tracking)
	}
	switch o.Encoding {
	case "":
		o.Encoding = EncodingZ64
	case EncodingZ64, EncodingHex:
	default:
		return nil, fmt.Errorf("encoding %q: want z64 or hex", o.Encoding)
	}
	switch o.Dither {
	case "", imaging.DitherAuto, imaging.DitherOn, imaging.DitherOff:
	default:
		return nil, fmt.Errorf("dither %q: want auto, on or off", o.Dither)
	}
	if o.PrintWidth == 0 {
		o.PrintWidth = headDots[o.DPI]
	}
	if o.MakeModel == "" {
		o.MakeModel = "Zebra ZPL Label Printer"
	}
	continuous, w, l, err := ParseMedia(o.Media)
	if err != nil {
		return nil, err
	}
	if dw := dots(w, o.DPI); dw > o.PrintWidth {
		return nil, fmt.Errorf("media %q is %d dots wide at %d dpi, wider than the print width %d (set --driver-opt print-width)",
			o.Media, dw, o.DPI, o.PrintWidth)
	}
	if !continuous && dots(l, o.DPI) > MaxDots {
		return nil, fmt.Errorf("media %q is longer than ZPL's %d dots", o.Media, MaxDots)
	}
	d := &Driver{opts: o, continuous: continuous}
	if d.caps, err = d.buildCaps(w, l); err != nil {
		return nil, err
	}
	if err := d.caps.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Driver) Name() string                      { return Name }
func (d *Driver) Capabilities() driver.Capabilities { return d.caps }
func (d *Driver) Options() Options                  { return d.opts }

// Common label sizes, all PWG self-describing names.
var commonSizes = []string{
	driver.Media4x6,
	"oe_4x4-label_4x4in",
	"oe_4x3-label_4x3in",
	"oe_4x2-label_4x2in",
	"oe_4x1-label_4x1in",
	"oe_3x2-label_3x2in",
	"oe_3x1-label_3x1in",
	"oe_2.25x1.25-label_2.25x1.25in",
	"oe_2x1-label_2x1in",
}

func (d *Driver) buildCaps(w, l int) (driver.Capabilities, error) {
	dpi := d.opts.DPI
	c := driver.Capabilities{
		MakeModel:   d.opts.MakeModel,
		Kind:        []string{"label"},
		Resolutions: []driver.Resolution{{X: dpi, Y: dpi}},
		Qualities:   []int{driver.QualityNormal},
		URF:         []string{"V1.4", "W8", "CP99", "RS" + strconv.Itoa(dpi)},
		PWGTypes:    []string{"sgray_8"},
		MaxCopies:   99,
	}
	mediaType := "labels"
	if d.continuous {
		mediaType = "continuous"
	}
	all, err := driver.Sheets(commonSizes, driver.Margins{}, mediaType, "main")
	if err != nil {
		return c, err
	}
	if d.continuous {
		c.Kind = []string{"label", "roll"}
		c.Roll = &driver.Roll{
			Width:         w,
			MinLength:     1000,
			MaxLength:     100000,
			DefaultLength: min(max(w*3/2, 1000), 100000),
			Type:          mediaType,
			Source:        "main",
		}
		for _, m := range all {
			if near(m.Width, w) {
				c.Media = append(c.Media, m)
			}
		}
		return c, nil
	}
	for _, m := range all {
		if dots(m.Width, dpi) > d.opts.PrintWidth {
			continue
		}
		if c.DefaultMedia == "" && near(m.Width, w) && near(m.Length, l) {
			m.Ready = true
			c.DefaultMedia = m.Name
		}
		c.Media = append(c.Media, m)
	}
	if c.DefaultMedia == "" {
		m := driver.Media{Name: driver.MediaName(w, l), Width: w, Length: l, Type: mediaType, Source: "main", Ready: true}
		c.Media = append(c.Media, m)
		c.DefaultMedia = m.Name
	}
	return c, nil
}

func near(a, b int) bool { return a >= b-driver.SizeTolerance && a <= b+driver.SizeTolerance }

// dots converts hundredths of a millimetre to dots, as imaging.Place sizes the sheet.
func dots(hmm, dpi int) int { return int(math.Round(float64(hmm) * float64(dpi) / 2540)) }

var sizeRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)x([0-9]+(?:\.[0-9]+)?)(in|mm)?$`)
var widthRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(in|mm)?$`)

// ParseMedia reads a --media id: "WxH" in inches (or with an in/mm suffix) for labels, or
// "continuous" / "continuous-W[in|mm]" for a roll. Sizes are hundredths of a millimetre; length
// is 0 for a roll.
func ParseMedia(id string) (continuous bool, width, length int, err error) {
	s := strings.ToLower(strings.TrimSpace(id))
	if s == "" {
		s = "4x6"
	}
	bad := fmt.Errorf("media %q: want a label size such as 4x6, 2x1, 100x150mm, or continuous, continuous-4in, continuous-57mm", id)
	if s == "continuous" {
		return true, 10160, 0, nil
	}
	if rest, ok := strings.CutPrefix(s, "continuous-"); ok {
		m := widthRE.FindStringSubmatch(rest)
		if m == nil {
			return false, 0, 0, bad
		}
		width = toHMM(m[1], m[2])
		if width < 500 {
			return false, 0, 0, bad
		}
		return true, width, 0, nil
	}
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return false, 0, 0, bad
	}
	width, length = toHMM(m[1], m[3]), toHMM(m[2], m[3])
	if width < 500 || length < 500 {
		return false, 0, 0, bad
	}
	return false, width, length, nil
}

func toHMM(v, unit string) int {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return -1
	}
	if unit != "mm" {
		f *= 25.4
	}
	return int(math.Round(f * 100))
}

// Render places each page on its label at the head resolution, binarizes it and encodes one ZPL
// format per label.
func (d *Driver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	media := job.Media
	if media.Width == 0 {
		media = d.caps.Default()
	}
	dpi := d.opts.DPI
	if w := dots(media.Width, dpi); w > d.opts.PrintWidth {
		return nil, driver.DocumentErrorf("media %s is %d dots wide, wider than the print width %d", media.Name, w, d.opts.PrintWidth)
	}
	units := make([]driver.Unit, 0, len(pages))
	for i, p := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sheet, err := imaging.Place(p, media, imaging.PlaceOptions{XDPI: dpi, YDPI: dpi, Scaling: job.Scaling, Orientation: job.Orientation, Budget: job.Budget})
		if err != nil {
			return nil, driver.DocumentError(fmt.Errorf("page %d: %w", i+1, err))
		}
		pages[i].Gray = nil
		if sheet.Height > MaxDots {
			return nil, driver.DocumentErrorf("page %d: label is %d dots long, more than ZPL's %d", i+1, sheet.Height, MaxDots)
		}
		bm, dithered, err := imaging.Binarize(sheet, d.opts.Dither)
		if err != nil {
			return nil, err
		}
		job.Log("page %d: %dx%d dots at %d dpi, dithered %v", i+1, bm.W, bm.H, dpi, dithered)
		if job.Debug != nil {
			job.DebugImage(fmt.Sprintf("p%d", i+1), bm.Image())
		}
		units = append(units, driver.Unit{
			Data:        d.Encode(bm),
			Label:       fmt.Sprintf("label %d of %d", i+1, len(pages)),
			Impressions: 1,
		})
	}
	return units, nil
}
