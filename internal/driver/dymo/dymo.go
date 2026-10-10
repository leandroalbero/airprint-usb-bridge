// Package dymo is the driver for DYMO LabelWriter 400/450 series and 4XL label printers, from
// DYMO's public "LabelWriter 450 Series Technical Reference Manual": raster lines sent with
// <syn>, ESC commands for line width, label length, print density and form feed. It prints
// die-cut labels and continuous paper at 300x300 dpi, one device job per label.
//
// The LabelWriter 5xx series (550, 550 Turbo, 5XL) speaks a different protocol and only prints
// authenticated DYMO labels; it is refused.
package dymo

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

const Name = "dymo"

const dpi = 300

// Protocol bytes (LW 450 Technical Reference, "Printer Commands").
const (
	esc = 0x1b
	syn = 0x16
	etb = 0x17
)

// Densities map --driver-opt density to the ESC command letter (strobe time 75%, 87.5%, 100%,
// 112.5%).
var densities = map[string]byte{"light": 'c', "medium": 'd', "normal": 'e', "dark": 'g'}

type model struct {
	name     string // "LabelWriter 450 Turbo"
	xl       bool
	headDots int
	dotTab   int // ESC B: bytes skipped at the start of each head line
	maxWidth int // widest media the paper path takes, hundredths of a millimetre
}

func (m model) headBytes() int { return m.headDots / 8 }

// printableDots is how many dots of a line can carry the label image after the dot tab.
func (m model) printableDots() int { return m.headDots - m.dotTab*8 }

func (m model) printableWidth() int { return (m.printableDots()*2540 + dpi - 1) / dpi }

// headWidth is the width the head covers, hundredths of a millimetre.
func (m model) headWidth() int { return m.headDots * 2540 / dpi }

var (
	series5xx = regexp.MustCompile(`(?i)^(labelwriter\s*)?5(\d\d|xl)\b`)
	knownName = regexp.MustCompile(`(?i)^(labelwriter\s*)?(3\d\d|4\d\d|4xl)\b`)
	prefix    = regexp.MustCompile(`(?i)^labelwriter`)
	xlWord    = regexp.MustCompile(`(?i)4xl\b`)
)

func resolveModel(c driver.Config) (model, error) {
	idModel := ""
	if !c.DeviceID.Empty() {
		idModel = strings.TrimSpace(c.DeviceID.Model())
	}
	if series5xx.MatchString(idModel) {
		return model{}, fmt.Errorf("the printer reports itself as %s: the LabelWriter 5xx series uses a different protocol and authenticated labels; it is not supported", idModel)
	}
	name := idModel
	if opt := strings.TrimSpace(c.Option("model", "")); opt != "" {
		if series5xx.MatchString(opt) {
			return model{}, fmt.Errorf("model %q: the LabelWriter 5xx series is not supported", opt)
		}
		if !knownName.MatchString(opt) {
			return model{}, fmt.Errorf("model %q: want a LabelWriter 3xx/4xx model such as 450, 450 Turbo or 4XL", opt)
		}
		name = opt
	}
	if name == "" {
		name = "450"
	} else if !knownName.MatchString(name) {
		c.Log("dymo: unknown model %q; assuming the LabelWriter 450 protocol and its 672-dot head", name)
	}
	rest := strings.TrimSpace(prefix.ReplaceAllString(name, ""))
	rest = xlWord.ReplaceAllString(rest, "4XL")
	m := model{name: "LabelWriter " + rest, headDots: 672, maxWidth: 6300}
	if strings.Contains(strings.ToUpper(rest), "4XL") {
		m.xl, m.headDots, m.maxWidth = true, 1248, 10800
	}
	return m, nil
}

type Driver struct {
	model   model
	media   selection
	density string
	trim    bool
	dither  string
	caps    driver.Capabilities
}

// New builds the driver from bridge settings: --media picks the loaded labels, and the options
// model, density, dot-tab and trim are listed in register.go.
func New(c driver.Config) (driver.Driver, error) {
	d, err := newDriver(c)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func newDriver(c driver.Config) (*Driver, error) {
	m, err := resolveModel(c)
	if err != nil {
		return nil, err
	}
	m.dotTab, err = c.IntOption("dot-tab", 0)
	if err != nil {
		return nil, err
	}
	if m.dotTab < 0 || m.dotTab >= m.headBytes() {
		return nil, fmt.Errorf("option dot-tab=%d: want 0-%d bytes", m.dotTab, m.headBytes()-1)
	}
	density := strings.ToLower(c.Option("density", "normal"))
	if _, ok := densities[density]; !ok {
		return nil, fmt.Errorf("option density=%q: want light, medium, normal or dark", density)
	}
	trim, err := c.BoolOption("trim", true)
	if err != nil {
		return nil, err
	}
	switch c.Dither {
	case "", imaging.DitherAuto, imaging.DitherOn, imaging.DitherOff:
	default:
		return nil, fmt.Errorf("dither %q: want auto, on or off", c.Dither)
	}
	sel, err := m.selectMedia(c.Media)
	if err != nil {
		return nil, err
	}
	if c.HighRes {
		c.Log("dymo: --high-res ignored; labels print at 300x300 dpi")
	}
	d := &Driver{model: m, media: sel, density: density, trim: trim, dither: c.Dither}
	d.caps = d.buildCaps()
	if err := d.caps.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Driver) Name() string                      { return Name }
func (d *Driver) Capabilities() driver.Capabilities { return d.caps }

func (d *Driver) buildCaps() driver.Capabilities {
	c := driver.Capabilities{
		MakeModel:   "DYMO " + d.model.name,
		Kind:        []string{"label"},
		Resolutions: []driver.Resolution{{X: dpi, Y: dpi}},
		Qualities:   []int{driver.QualityNormal},
		URF:         []string{"V1.4", "W8", "CP99", "RS300"},
		PWGTypes:    []string{"sgray_8"},
		MaxCopies:   99,
	}
	if d.media.continuous {
		// Die-cut sizes stay out: a fixed size near the roll width would be taken as a roll
		// length, and a die-cut job on continuous paper has no top of form to align to.
		c.Kind = []string{"label", "roll"}
		c.Roll = &driver.Roll{
			Width:         d.media.width,
			MinLength:     1000,
			MaxLength:     maxContinuousLength,
			DefaultLength: 10000,
			Margins:       d.model.margins(d.media.width, true),
			Type:          "roll",
			Source:        "main",
		}
		return c
	}
	for _, l := range d.model.labels() {
		c.Media = append(c.Media, driver.Media{
			Name: l.name(), Width: l.Width, Length: l.Length,
			Margins: d.model.margins(l.Width, false),
			Type:    "labels", Source: "main",
			Ready: l.name() == d.media.label.name(),
		})
	}
	c.DefaultMedia = d.media.label.name()
	return c
}

// maxContinuousLength is the longest continuous label, hundredths of a millimetre (1 m).
const maxContinuousLength = 100000

// Render places, binarizes and encodes every page before returning. Each page with ink becomes
// one label sent as its own device job; blank pages are skipped.
func (d *Driver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	type rendered struct {
		page int
		data []byte
	}
	var out []rendered
	for i, p := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := i + 1
		media := d.jobMedia(job)
		sheet, err := imaging.Place(p, media, imaging.PlaceOptions{
			XDPI: dpi, YDPI: dpi, Scaling: d.scaling(job, p, media), Orientation: job.Orientation, Budget: job.Budget,
		})
		pages[i].Gray = nil
		if err != nil {
			return nil, driver.DocumentError(fmt.Errorf("page %d: %w; not printing", n, err))
		}
		bm, dithered, err := imaging.Binarize(sheet, d.dither)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", n, err)
		}
		if dithered {
			job.Log("page %d: dithered (photo-like content)", n)
		}
		bm, dropped := cropWidth(bm, d.model.printableDots())
		if dropped > 0 {
			job.Log("page %d: %d black dots past the %d printable head dots were dropped", n, dropped, d.model.printableDots())
		}
		top, bottom, ok := bm.InkRows()
		if !ok {
			job.Log("page %d is blank; skipped", n)
			continue
		}
		var data []byte
		if d.media.continuous {
			if !d.trim {
				top, bottom = 0, bm.H
			}
			bm = bm.Rows(top, bottom)
			if limit := dots(maxContinuousLength); bm.H > limit {
				return nil, driver.DocumentErrorf("page %d: label is %d dots (%.0f mm) long; continuous labels are at most %d mm; not printing",
					n, bm.H, float64(bm.H)*25.4/dpi, maxContinuousLength/100)
			}
			job.Log("page %d: continuous label %d dots long (%.1f mm)", n, bm.H, float64(bm.H)*25.4/dpi)
			data = d.encode(bm, bm.H, continuousLength)
		} else {
			// Leading blank rows position the image on the label; trailing ones are left to
			// the form feed, which runs to the next top-of-form hole anyway.
			job.Log("page %d: %s label, %d of %d dot lines sent", n, media.Name, bottom, bm.H)
			data = d.encode(bm, bottom, labelSearchLength(bm.H))
		}
		if job.Debug != nil {
			job.DebugImage(fmt.Sprintf("p%d-label", n), bm.Image())
		}
		out = append(out, rendered{n, data})
	}
	units := make([]driver.Unit, len(out))
	for i, r := range out {
		units[i] = driver.Unit{
			Data:        r.data,
			Label:       fmt.Sprintf("label %d of %d (page %d)", i+1, len(out), r.page),
			Impressions: 1,
		}
	}
	return units, nil
}

func (d *Driver) jobMedia(job driver.JobInfo) driver.Media {
	m := job.Media
	if d.media.continuous {
		r := d.caps.RollMedia(0)
		if m.Roll {
			r.Length = m.Length
		}
		return r
	}
	if m.Width <= 0 || m.Length <= 0 || m.Roll {
		return d.caps.Default()
	}
	return m
}

// scaling keeps a continuous-paper page as wide as the roll at 1:1. Place's auto mode only
// does that for a page of known length; for a roll of unknown length it would shrink the page
// into the printable width by a few percent, which blurs barcodes.
func (d *Driver) scaling(job driver.JobInfo, p raster.Page, media driver.Media) string {
	if !d.media.continuous || media.Length > 0 || (job.Scaling != "" && job.Scaling != "auto") || p.XDPI <= 0 {
		return job.Scaling
	}
	w := float64(p.Width) * 2540 / float64(p.XDPI)
	if math.Abs(w-float64(media.Width)) <= float64(media.Width)*0.03 {
		return "none"
	}
	return job.Scaling
}

func dots(hmm int) int { return (hmm*dpi + 1270) / 2540 }

// continuousLength is the ESC L value for continuous paper: any negative 16-bit value switches
// the printer to continuous feed, where ESC E feeds the last line past the tear bar.
const continuousLength = 0xffff

// labelSearchLength is the ESC L value for a die-cut label of h dot lines. ESC L is how far the
// printer feeds looking for the next top-of-form hole, and the manual asks for slightly more
// than the label so that the hole between labels is always reached.
func labelSearchLength(h int) int { return min(h+dpi/4, 0x7fff) }

// encode builds one label: ESC resync, line format, label length, mode and density, rows
// [0, rows) of bm as <syn> lines, then a form feed to the tear bar.
//
// There is deliberately no ESC @ (reset): it acts at once and discards whatever the previous
// label, sent as its own device job a moment earlier, still has in the print buffer. Every
// setting the reset would restore is sent explicitly instead.
func (d *Driver) encode(bm *imaging.Bitmap, rows, length int) []byte {
	bpl := bm.Stride
	buf := make([]byte, 0, d.model.headBytes()+16+rows*(bpl+1))
	// One more ESC than the longest raster line ends any line a cancelled job left half sent,
	// so the printer is parsing commands again (manual: "Data Synchronization").
	for i := 0; i <= d.model.headBytes(); i++ {
		buf = append(buf, esc)
	}
	buf = append(buf,
		esc, 'B', byte(d.model.dotTab),
		esc, 'D', byte(bpl),
		esc, 'L', byte(length>>8), byte(length),
		esc, 'h',
		esc, densities[d.density],
	)
	for y := 0; y < rows; y++ {
		buf = append(buf, syn)
		buf = append(buf, bm.Row(y)...)
	}
	return append(buf, esc, 'E')
}

// cropWidth cuts bm to at most w dots and counts the black dots it removed.
func cropWidth(bm *imaging.Bitmap, w int) (*imaging.Bitmap, int) {
	if bm.W <= w {
		return bm, 0
	}
	out := imaging.NewBitmap(w, bm.H)
	dropped := 0
	for y := 0; y < bm.H; y++ {
		src, dst := bm.Row(y), out.Row(y)
		copy(dst, src)
		if r := w % 8; r != 0 {
			dst[len(dst)-1] &= byte(0xff << (8 - r))
		}
		for x := w; x < bm.W; x++ {
			if bm.At(x, y) {
				dropped++
			}
		}
	}
	return out, dropped
}
