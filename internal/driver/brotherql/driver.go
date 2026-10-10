// Package brotherql is the Brother QL label printer driver: continuous-roll labels through the QL
// raster protocol (internal/ql), one device job per label, with status-aware sending over usblp
// and network port 9100.
package brotherql

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

const Name = "brother-ql"

// Media policies.
const (
	// PolicyRoll advertises only the roll as loaded media.
	PolicyRoll = "roll"
	// PolicyRollPresets also advertises the preset label lengths as loaded media. iOS then shows
	// a paper menu, but apps that pick the "best" loaded paper stop using the roll.
	PolicyRollPresets = "roll+presets"
)

type Options struct {
	Profile     *ql.Profile
	Media       string // profile media id, "62"
	MediaPolicy string // PolicyRoll or PolicyRollPresets
	Presets     []int  // preset label lengths, hundredths of a millimetre
	HighRes     bool   // offer print-quality high (300x600 dpi)
	Compress    bool   // TIFF (PackBits) raster lines, on profiles that support it
	TrimMax     int    // blank rows trimmed per label end at 300 dpi; < 0 unlimited
	Dither      string // "auto", "on" or "off"
}

type Driver struct {
	prof  *ql.Profile
	media ql.Media
	opts  Options
	ropts raster.Options
	caps  driver.Capabilities
}

func init() {
	driver.Register(driver.Info{
		Name:        Name,
		Description: "Brother QL label printers, continuous rolls (QL raster protocol); profiles for " + strings.Join(ql.EmbeddedModels(), ", "),
		Verified:    "QL-700 with 62 mm DK-22205",
		Media:       "a continuous media id from the profile: 12, 29, 38, 50, 54 or 62, plus 102 on the QL-1050/1060N and 102 or 103 on the QL-1100/1110NWB; default 62",
		Options: []driver.Option{
			{Key: "profile", Default: "", Help: "built-in profile (" + strings.Join(ql.EmbeddedProfileNames(), ", ") + ") or a profile JSON path; default: from the printer's model, else ql700"},
			{Key: "compress", Default: "false", Help: "send TIFF-compressed raster lines (profiles that support it)"},
			{Key: "media-policy", Default: PolicyRoll, Help: "loaded media advertised to clients: roll or roll+presets"},
			{Key: "presets", Default: "29,50", Help: "preset label lengths in mm, comma separated"},
			{Key: "trim", Default: "max", Help: "blank rows trimmed per label end: max (no limit) or N"},
		},
		New: func(c driver.Config) (driver.Driver, error) { return NewFromConfig(c) },
	})
}

// NewFromConfig builds the driver from bridge settings and the options listed in its Info.
func NewFromConfig(c driver.Config) (*Driver, error) {
	model := ""
	if !c.DeviceID.Empty() {
		model = c.DeviceID.Model()
	}
	prof, err := LoadProfileFor(c.Option("profile", ""), model, c.USBVendorID, c.USBProductID)
	if err != nil {
		return nil, err
	}
	presets, err := ParsePresets(c.Option("presets", "29,50"))
	if err != nil {
		return nil, err
	}
	trim, err := ParseTrim(c.Option("trim", "max"))
	if err != nil {
		return nil, err
	}
	compress, err := c.BoolOption("compress", false)
	if err != nil {
		return nil, err
	}
	highRes := c.HighRes
	if highRes && (prof.NoHighRes || prof.NoExpandedMode) {
		c.Log("brother-ql: %s has no 300x600 mode; --high-res ignored", prof.Model)
		highRes = false
	}
	if prof.TwoColor {
		c.Log("brother-ql: %s: two-colour (red/black) printing is not supported; labels print in black", prof.Model)
	}
	if !prof.IsVerified() {
		c.Log("brother-ql: the %s profile is unverified (from Brother's command reference, not tested on hardware)", prof.Model)
	}
	media := c.Media
	if media == "" {
		media = "62"
	}
	return New(Options{
		Profile:     prof,
		Media:       media,
		MediaPolicy: c.Option("media-policy", PolicyRoll),
		Presets:     presets,
		HighRes:     highRes,
		Compress:    compress,
		TrimMax:     trim,
		Dither:      c.Dither,
	})
}

// LoadProfile picks a profile. A name is a built-in profile ("ql820nwb", "QL-820NWB") or else a
// profile JSON path. With no name, model (the printer's 1284 MDL) picks the built-in profile,
// and with neither it is the QL-700.
func LoadProfile(name, model string) (*ql.Profile, error) {
	return LoadProfileFor(name, model, 0, 0)
}

// LoadProfileFor is LoadProfile with the printer's USB vendor and product ID as a fallback
// when the device ID's model is missing or unknown.
func LoadProfileFor(name, model string, vendor, product uint16) (*ql.Profile, error) {
	if name != "" {
		if p, ok, err := ql.EmbeddedProfileByName(name); ok || err != nil {
			return p, err
		}
		return ql.LoadProfile(name)
	}
	if model != "" {
		if p, ok, err := ql.ProfileForModel(model); ok || err != nil {
			return p, err
		}
	}
	if p, ok, err := ql.ProfileForUSB(vendor, product); ok || err != nil {
		return p, err
	}
	if model != "" {
		return nil, fmt.Errorf("no built-in profile for %s (have %s); pass --profile FILE",
			model, strings.Join(ql.EmbeddedModels(), ", "))
	}
	return ql.EmbeddedProfile()
}

// New builds the driver for one roll of a profile.
func New(o Options) (*Driver, error) {
	if o.Profile == nil {
		return nil, errors.New("brother-ql: nil profile")
	}
	media, ok := o.Profile.MediaByID(o.Media)
	if !ok {
		var ids []string
		for _, m := range o.Profile.Media {
			ids = append(ids, m.ID)
		}
		return nil, fmt.Errorf("profile %s has no media %q (have %s)", o.Profile.Model, o.Media, strings.Join(ids, ", "))
	}
	if media.Kind != ql.MediaContinuous {
		return nil, fmt.Errorf("media %q is not a continuous roll; only rolls are supported", o.Media)
	}
	if o.HighRes && (o.Profile.NoHighRes || o.Profile.NoExpandedMode) {
		return nil, fmt.Errorf("%s has no 300x600 high-resolution mode", o.Profile.Model)
	}
	if o.Compress && !o.Profile.SupportsCompress {
		return nil, fmt.Errorf("%s does not support compressed raster data", o.Profile.Model)
	}
	switch o.MediaPolicy {
	case "":
		o.MediaPolicy = PolicyRoll
	case PolicyRoll, PolicyRollPresets:
	default:
		return nil, fmt.Errorf("media policy %q (want roll or roll+presets)", o.MediaPolicy)
	}
	switch o.Dither {
	case "", "auto", "on", "off":
	default:
		return nil, fmt.Errorf("dither %q: want auto, on or off", o.Dither)
	}
	// Clients rasterise the full roll width (6200/2540*300 = 732 dots for 62 mm);
	// anything far from it means the page arrived on the wrong axis or media.
	full := int(math.Round(float64(media.WidthMM) * 300 / 25.4))
	d := &Driver{
		prof:  o.Profile,
		media: media,
		opts:  o,
		ropts: raster.Options{
			Pins:     media.PrintPins,
			WidthMin: full - 8,
			WidthMax: full + 8,
			TrimMax:  o.TrimMax,
			Dither:   o.Dither,
		},
	}
	d.caps = d.buildCaps()
	if err := d.caps.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Driver) Name() string                        { return Name }
func (d *Driver) Capabilities() driver.Capabilities   { return d.caps }
func (d *Driver) Profile() *ql.Profile                { return d.prof }
func (d *Driver) Media() ql.Media                     { return d.media }
func (d *Driver) Inspect(data []byte) (string, error) { return ql.Decode(data) }

// PageLimits refuses a page of the wrong width from its header, before decoding it.
func (d *Driver) PageLimits(driver.JobInfo) raster.Limits {
	return raster.Limits{WidthMin: d.ropts.WidthMin, WidthMax: d.ropts.WidthMax}
}

func (d *Driver) buildCaps() driver.Capabilities {
	width := 6200
	// The 62 mm margins are the ones the live attribute set iOS accepted was verified with.
	margins := driver.Margins{Left: 152, Right: 152}
	if d.media.WidthMM != 62 {
		width = d.media.WidthMM * 100
		side := (width - d.media.PrintPins*2540/300) / 2
		margins = driver.Margins{Left: max(side, 0), Right: max(side, 0)}
	}
	maxLength := 100000
	if hi := d.prof.PageLengthDots.V[1]; hi > 0 {
		maxLength = ql.LengthMM(d.prof.DPI, hi, false) * 100
	}
	c := driver.Capabilities{
		MakeModel:   "Brother " + d.prof.Model,
		Kind:        []string{"label", "roll"},
		Resolutions: []driver.Resolution{{X: 300, Y: 300}},
		Qualities:   []int{driver.QualityNormal},
		URF:         []string{"V1.4", "W8", "CP1", "RS300"},
		PWGTypes:    []string{"sgray_8"},
		MaxCopies:   1,
		Roll: &driver.Roll{
			Width:         width,
			MinLength:     800,
			MaxLength:     maxLength,
			DefaultLength: 10000,
			Margins:       margins,
			Type:          "roll",
			Source:        "main",
		},
		Verified: d.prof.IsVerified(),
	}
	if d.opts.HighRes {
		c.Resolutions = append(c.Resolutions, driver.Resolution{X: 600, Y: 600})
		c.AlsoAccept = []driver.Resolution{{X: 300, Y: 600}}
		c.Qualities = append(c.Qualities, driver.QualityHigh)
		c.URF = []string{"V1.4", "W8", "CP1", "PQ4-5", "RS300-600"}
	}
	for _, p := range d.opts.Presets {
		c.Media = append(c.Media, driver.Media{
			Name: driver.MediaName(width, p), Width: width, Length: p, Margins: margins,
			Type: "roll", Source: "main", Ready: d.opts.MediaPolicy == PolicyRollPresets,
		})
	}
	return c
}

// Render converts and checks every page before returning, so a bad page never leaves a
// half-printed job on the roll. Each non-blank page becomes one label, sent as its own job.
func (d *Driver) Render(ctx context.Context, job driver.JobInfo, pages []raster.Page) ([]driver.Unit, error) {
	highRes := d.opts.HighRes && job.Quality == driver.QualityHigh
	opts := d.ropts
	opts.HighRes = highRes
	type label struct {
		page int
		bmp  *ql.Bitmap
	}
	var labels []label
	// One 600 dpi page puts the whole job in high-res mode, so every label of a
	// job is fed at the same resolution.
	jobHighRes := false
	for i, pg := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := i + 1
		if highRes && pg.XDPI == 600 {
			jobHighRes = true
		}
		lab, err := raster.Prepare(pg, opts)
		pages[i].Gray = nil
		if err != nil {
			if errors.Is(err, raster.ErrWidth) || errors.Is(err, raster.ErrResolution) {
				err = driver.DocumentError(err)
			}
			return nil, fmt.Errorf("page %d: %w; not printing", n, err)
		}
		if pg.XDPI == 600 {
			mode := "300x300"
			if lab.HighRes {
				mode = "300x600 high-res"
			}
			job.Log("page %d: 600 dpi reduced to %s", n, mode)
		}
		if lab.Dithered {
			job.Log("page %d: dithered (photo-like content)", n)
		}
		if lab.Dropped > 0 {
			job.Log("page %d: %d black dots outside the %d printable columns were dropped", n, lab.Dropped, opts.Pins)
		}
		if lab.Blank {
			job.Log("page %d is blank; skipped", n)
			continue
		}
		dpi := 300.0
		if lab.HighRes {
			dpi = 600
		}
		job.Log("page %d: label %d dots long (%.2f mm)", n, lab.Bitmap.H, float64(lab.Bitmap.H)*25.4/dpi)
		if job.Debug != nil {
			job.DebugImage(fmt.Sprintf("p%d-label", n), BitmapImage(lab.Bitmap))
		}
		labels = append(labels, label{n, lab.Bitmap})
	}
	units := make([]driver.Unit, 0, len(labels))
	for i, l := range labels {
		u, err := d.unit(d.job(l.bmp, jobHighRes))
		if err != nil {
			return nil, fmt.Errorf("label %d (page %d): %w", i+1, l.page, err)
		}
		u.Label = fmt.Sprintf("label %d of %d (page %d)", i+1, len(labels), l.page)
		units = append(units, u)
	}
	return units, nil
}

func (d *Driver) job(bmp *ql.Bitmap, highRes bool) ql.Job {
	j := JobFor(d.prof, d.media, bmp, highRes)
	j.Options.Compress = d.opts.Compress
	return j
}

func (d *Driver) unit(job ql.Job) (driver.Unit, error) {
	data, err := ql.NewEncoder().Encode(d.prof, job)
	if err != nil {
		return driver.Unit{}, fmt.Errorf("encode: %w", err)
	}
	return driver.Unit{Data: data, Meta: job}, nil
}

// RenderImage prints an image as one label, left-aligned on the roll: black where
// (r+g+b)/3 < 0x8000, no scaling, cropping or trimming.
func (d *Driver) RenderImage(_ context.Context, job driver.JobInfo, img image.Image) ([]driver.Unit, error) {
	bmp := ThresholdImage(img)
	if bmp.W > d.media.PrintPins {
		return nil, driver.DocumentErrorf("image is %d dots wide; %s mm media prints %d", bmp.W, d.media.ID, d.media.PrintPins)
	}
	u, err := d.unit(d.job(bmp, d.opts.HighRes && job.Quality == driver.QualityHigh))
	if err != nil {
		return nil, err
	}
	u.Label = fmt.Sprintf("%dx%d", bmp.W, bmp.H)
	return []driver.Unit{u}, nil
}

// ThresholdImage converts an image to a label bitmap: black when (r+g+b)/3 < 0x8000.
func ThresholdImage(img image.Image) *ql.Bitmap {
	r := img.Bounds()
	b := ql.NewBitmap(r.Dx(), r.Dy())
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			cr, cg, cb, _ := img.At(r.Min.X+x, r.Min.Y+y).RGBA()
			if (cr+cg+cb)/3 < 0x8000 {
				b.Bits[y*b.W+x] = true
			}
		}
	}
	return b
}

// BitmapImage is a 1-bit view of a label for debug PNGs (black on white).
func BitmapImage(b *ql.Bitmap) image.Image {
	img := image.NewPaletted(image.Rect(0, 0, b.W, b.H), color.Palette{color.White, color.Black})
	for y := 0; y < b.H; y++ {
		row := b.Bits[y*b.W : (y+1)*b.W]
		for x, black := range row {
			if black {
				img.Pix[y*img.Stride+x] = 1
			}
		}
	}
	return img
}

// NewSender returns the status-aware QL sender for usblp and TCP targets. Dry runs use the
// generic transport, which writes Unit.Data and the Inspect text.
func (d *Driver) NewSender(t backend.Target) (driver.Sender, error) {
	switch t.Kind {
	case backend.TargetUSB:
		return sender{NewUSB(d.prof, t.Device)}, nil
	case backend.TargetTCP:
		return sender{NewTCP(d.prof, t.Addr)}, nil
	}
	return nil, nil
}

type sender struct{ b Backend }

func (s sender) Send(ctx context.Context, u driver.Unit) error {
	job, ok := u.Meta.(ql.Job)
	if !ok {
		return fmt.Errorf("brother-ql: unit %q carries no ql.Job", u.Label)
	}
	return s.b.Print(ctx, job)
}

func (s sender) State(ctx context.Context) (backend.State, error) { return s.b.State(ctx) }
func (s sender) Close() error                                     { return s.b.Close() }

// ParseTrim reads the trim option: "max" (or "unlimited", "-1") or a row count.
func ParseTrim(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "max", "unlimited", "-1":
		return -1, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--trim %q: want max or a row count", s)
	}
	return n, nil
}

// ParsePresets turns "29,50" (mm) into hundredths of a millimetre.
func ParsePresets(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		mm, err := strconv.ParseFloat(strings.TrimSuffix(f, "mm"), 64)
		if err != nil || mm <= 0 || mm > 1000 {
			return nil, fmt.Errorf("--presets: bad length %q", f)
		}
		out = append(out, int(math.Round(mm*100)))
	}
	return out, nil
}
