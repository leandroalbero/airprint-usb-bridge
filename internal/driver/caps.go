package driver

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Resolution is in dots per inch.
type Resolution struct{ X, Y int }

// Margins are in hundredths of a millimetre, as in media-col.
type Margins struct{ Top, Bottom, Left, Right int }

// Media is one fixed media size. Dimensions are hundredths of a millimetre.
type Media struct {
	Name    string // PWG self-describing name, e.g. "iso_a4_210x297mm", "om_62x29mm_62x29mm"
	Width   int    // x-dimension (across the feed)
	Length  int    // y-dimension; in JobInfo.Media of a roll job, 0 when the client gave no length
	Margins Margins
	Type    string // media-type, e.g. "stationery", "labels", "roll"
	Source  string // media-source, e.g. "main", "auto", "manual"
	Ready   bool   // loaded: listed in media-ready and media-col-ready
	Roll    bool   // only in JobInfo.Media: the job is on the continuous roll
}

// Roll is a continuous roll. A roll printer cuts each page to the length the client sent.
type Roll struct {
	Width                int // x-dimension
	MinLength, MaxLength int // y-dimension range clients may request
	DefaultLength        int // y-dimension of media-col-default
	Margins              Margins
	Type                 string // media-type, "roll" for the QL
	Source               string // media-source, "main" for the QL
}

const (
	QualityDraft  = 3
	QualityNormal = 4
	QualityHigh   = 5
)

// Capabilities describe the printer. The IPP server builds every printer attribute from them; see
// DRIVER-API.md for the attribute each field drives.
type Capabilities struct {
	MakeModel string   // printer-make-and-model and TXT ty, e.g. "Brother QL-700"
	DeviceID  string   // printer-device-id; empty derives MFG/MDL from MakeModel and CMD from the formats
	Kind      []string // printer-kind, e.g. {"label", "roll"} or {"document"}
	Color     bool     // color-supported; adds print-color-mode color and TXT Color=T
	Duplex    bool     // adds sides two-sided-long-edge/short-edge and TXT Duplex=T

	// Resolutions are printer-resolution-supported and pwg-raster-document-resolution-supported;
	// [0] is printer-resolution-default.
	Resolutions []Resolution
	// AlsoAccept are printer-resolution values accepted in a job but not advertised.
	AlsoAccept []Resolution
	Qualities  []int    // print-quality-supported; the default is normal when listed, else [0]
	URF        []string // urf-supported and TXT URF, verbatim, e.g. {"V1.4", "W8", "CP1", "RS300"}
	PWGTypes   []string // pwg-raster-document-type-supported, e.g. {"sgray_8"}
	// Formats are pass-through document formats besides image/urf and image/pwg-raster, which
	// are always accepted. Each needs a DocumentRenderer.
	Formats []string

	MaxCopies    int  // copies-supported upper bound; 0 or 1 advertises 1-1
	CopiesNative bool // the driver honours JobInfo.Copies itself; otherwise the bridge repeats the units

	Roll         *Roll   // the continuous roll of a roll printer
	Media        []Media // fixed sizes, after the roll in media-col-database order; presets for a roll printer
	DefaultMedia string  // Name of the default fixed size when there is no Roll; "" = Media[0]

	Verified bool // tested on real hardware
}

// Validate checks what the IPP server relies on.
func (c *Capabilities) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if c.MakeModel == "" {
		bad("empty MakeModel")
	}
	if len(c.Kind) == 0 {
		bad("empty Kind")
	}
	if len(c.Resolutions) == 0 {
		bad("no Resolutions")
	}
	for _, r := range append(append([]Resolution(nil), c.Resolutions...), c.AlsoAccept...) {
		if r.X <= 0 || r.Y <= 0 {
			bad("resolution %dx%d", r.X, r.Y)
		}
	}
	if len(c.Qualities) == 0 {
		bad("no Qualities")
	}
	for _, q := range c.Qualities {
		if q < QualityDraft || q > QualityHigh {
			bad("print-quality %d", q)
		}
	}
	if len(c.URF) == 0 {
		bad("no URF tokens")
	}
	if len(c.PWGTypes) == 0 {
		bad("no PWGTypes")
	}
	for _, f := range c.Formats {
		if !strings.Contains(f, "/") || f == "image/urf" || f == "image/pwg-raster" || f == "application/octet-stream" {
			bad("pass-through format %q", f)
		}
	}
	if c.MaxCopies < 0 {
		bad("MaxCopies %d", c.MaxCopies)
	}
	if c.Roll == nil && len(c.Media) == 0 {
		bad("no media: set Roll or Media")
	}
	if r := c.Roll; r != nil {
		if r.Width <= 0 {
			bad("roll width %d", r.Width)
		}
		if r.MinLength <= 0 || r.MinLength > r.MaxLength {
			bad("roll length range %d-%d", r.MinLength, r.MaxLength)
		}
		if r.DefaultLength <= 0 {
			bad("roll default length %d", r.DefaultLength)
		}
		if r.Type == "" || r.Source == "" {
			bad("roll needs Type and Source")
		}
		errs = append(errs, checkMargins("roll", r.Margins, r.Width, r.MinLength)...)
	}
	seen := map[string]bool{}
	for _, m := range c.Media {
		if m.Name == "" || seen[m.Name] {
			bad("media name %q empty or repeated", m.Name)
		}
		seen[m.Name] = true
		if m.Width <= 0 || m.Length <= 0 {
			bad("media %s size %dx%d", m.Name, m.Width, m.Length)
		}
		if m.Type == "" || m.Source == "" {
			bad("media %s needs Type and Source", m.Name)
		}
		errs = append(errs, checkMargins(m.Name, m.Margins, m.Width, m.Length)...)
	}
	if c.DefaultMedia != "" && !seen[c.DefaultMedia] {
		bad("default media %q not in Media", c.DefaultMedia)
	}
	if len(errs) > 0 {
		return fmt.Errorf("capabilities: %w", errors.Join(errs...))
	}
	return nil
}

func checkMargins(name string, m Margins, w, l int) []error {
	if m.Top < 0 || m.Bottom < 0 || m.Left < 0 || m.Right < 0 || m.Left+m.Right >= w || m.Top+m.Bottom >= l {
		return []error{fmt.Errorf("media %s margins %+v", name, m)}
	}
	return nil
}

// IsDocument reports whether printer-kind includes "document" (a sheet printer).
func (c *Capabilities) IsDocument() bool {
	for _, k := range c.Kind {
		if k == "document" {
			return true
		}
	}
	return false
}

// DefaultQuality is print-quality-default.
func (c *Capabilities) DefaultQuality() int {
	for _, q := range c.Qualities {
		if q == QualityNormal {
			return q
		}
	}
	if len(c.Qualities) > 0 {
		return c.Qualities[0]
	}
	return QualityNormal
}

// SidesSupported is sides-supported.
func (c *Capabilities) SidesSupported() []string {
	if c.Duplex {
		return []string{"one-sided", "two-sided-long-edge", "two-sided-short-edge"}
	}
	return []string{"one-sided"}
}

// ColorModes is print-color-mode-supported.
func (c *Capabilities) ColorModes() []string {
	modes := []string{"monochrome", "auto", "auto-monochrome"}
	if c.Color {
		modes = append(modes, "color")
	}
	return modes
}

// DefaultColorMode is print-color-mode-default: auto (which prints in colour) on a colour
// printer.
func (c *Capabilities) DefaultColorMode() string {
	if c.Color {
		return "auto"
	}
	return "monochrome"
}

// UseColor reports whether a job in print-color-mode mode prints in colour: only on a colour
// printer, and only for color and auto ("" is the default, auto). auto-monochrome and
// monochrome print gray.
func (c *Capabilities) UseColor(mode string) bool {
	switch mode {
	case "", "auto", "color":
		return c.Color
	}
	return false
}

// AcceptsResolution reports whether a job may ask for r.
func (c *Capabilities) AcceptsResolution(r Resolution) bool {
	for _, s := range c.Resolutions {
		if s == r {
			return true
		}
	}
	for _, s := range c.AlsoAccept {
		if s == r {
			return true
		}
	}
	return false
}

// RollName is a PWG roll name such as "roll_current_62x0mm" (kind is roll_current, roll_min,
// roll_max, custom_min or custom_max).
func (r *Roll) Name(kind string, length int) string {
	return kind + "_" + MMString(r.Width) + "x" + MMString(length) + "mm"
}

// RollMedia is the roll as the Media of a job of the given length (0 = unknown).
func (c *Capabilities) RollMedia(length int) Media {
	r := c.Roll
	return Media{
		Name: r.Name("roll_current", 0), Width: r.Width, Length: length, Margins: r.Margins,
		Type: r.Type, Source: r.Source, Ready: true, Roll: true,
	}
}

// Default is the media of a job that names none: the roll, else DefaultMedia, else Media[0].
func (c *Capabilities) Default() Media {
	if c.Roll != nil {
		return c.RollMedia(0)
	}
	for _, m := range c.Media {
		if m.Name == c.DefaultMedia {
			return m
		}
	}
	if len(c.Media) > 0 {
		return c.Media[0]
	}
	return Media{}
}

// SizeTolerance is how far a requested dimension may be from an offered one (1 mm): clients
// round sizes differently, e.g. 62 mm media or US sizes converted from inches.
const SizeTolerance = 100

func near(a, b int) bool { return a >= b-SizeTolerance && a <= b+SizeTolerance }

// MatchWidth reports whether any media is x wide.
func (c *Capabilities) MatchWidth(x int) bool {
	if c.Roll != nil && near(x, c.Roll.Width) {
		return true
	}
	for _, m := range c.Media {
		if near(x, m.Width) {
			return true
		}
	}
	return false
}

// MatchSize finds the media for a requested size. On a roll, any length inside the roll range,
// or exactly a fixed (preset) length, is the roll cut to that length. Fixed sizes match within
// SizeTolerance on both axes.
func (c *Capabilities) MatchSize(x, y int) (Media, bool) {
	if r := c.Roll; r != nil && near(x, r.Width) {
		if y >= r.MinLength && y <= r.MaxLength {
			return c.RollMedia(y), true
		}
		for _, m := range c.Media {
			if y == m.Length {
				return c.RollMedia(y), true
			}
		}
	}
	for _, m := range c.Media {
		if near(x, m.Width) && near(y, m.Length) {
			return m, true
		}
	}
	return Media{}, false
}

// MatchRange accepts a media-size whose y-dimension is a range inside the roll's.
func (c *Capabilities) MatchRange(x, lo, hi int) (Media, bool) {
	r := c.Roll
	if r == nil || !near(x, r.Width) || lo < r.MinLength || hi > r.MaxLength || lo > hi {
		return Media{}, false
	}
	return c.RollMedia(0), true
}

// MediaNames is media-supported: the fixed names, then the roll's PWG range names.
func (c *Capabilities) MediaNames() []string {
	var out []string
	for _, m := range c.Media {
		out = append(out, m.Name)
	}
	if r := c.Roll; r != nil {
		out = append(out,
			r.Name("roll_min", r.MinLength), r.Name("roll_max", r.MaxLength),
			r.Name("custom_min", r.MinLength), r.Name("custom_max", r.MaxLength))
	}
	return out
}

// ReadyNames is media-ready: the roll first, then the ready fixed sizes.
func (c *Capabilities) ReadyNames() []string {
	var out []string
	if c.Roll != nil {
		out = append(out, c.Roll.Name("roll_current", 0))
	}
	for _, m := range c.Media {
		if m.Ready {
			out = append(out, m.Name)
		}
	}
	return out
}

// LookupName resolves a media keyword: an advertised name, or any PWG self-describing name whose
// size matches.
func (c *Capabilities) LookupName(name string) (Media, bool) {
	for _, m := range c.Media {
		if m.Name == name {
			if c.Roll != nil && near(m.Width, c.Roll.Width) {
				return c.RollMedia(m.Length), true
			}
			return m, true
		}
	}
	if c.Roll != nil {
		for _, n := range c.MediaNames()[len(c.Media):] {
			if n == name {
				return c.RollMedia(0), true
			}
		}
		if name == c.Roll.Name("roll_current", 0) {
			return c.RollMedia(0), true
		}
	}
	x, y, ok := ParseMediaName(name)
	if !ok {
		return Media{}, false
	}
	return c.MatchSize(x, y)
}

var pwgMediaName = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9.-]*_([0-9]+(?:\.[0-9]+)?)x([0-9]+(?:\.[0-9]+)?)(mm|in)$`)

// ParseMediaName returns the size of a PWG self-describing media name in hundredths of a
// millimetre, e.g. "na_letter_8.5x11in" is 21590 x 27940.
func ParseMediaName(name string) (x, y int, ok bool) {
	m := pwgMediaName.FindStringSubmatch(name)
	if m == nil {
		return 0, 0, false
	}
	x, y = toHundredthsMM(m[1], m[3]), toHundredthsMM(m[2], m[3])
	if x < 0 || y < 0 {
		return 0, 0, false
	}
	return x, y, true
}

func toHundredthsMM(v, unit string) int {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return -1
	}
	if unit == "in" {
		f *= 25.4
	}
	return int(f*100 + 0.5)
}

// MMString formats hundredths of a millimetre as millimetres for PWG media names: 6200 is "62",
// 6250 is "62.5".
func MMString(v int) string {
	s := strconv.Itoa(v / 100)
	if f := v % 100; f != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%02d", f), "0")
	}
	return s
}

// Common PWG media names.
const (
	MediaA4        = "iso_a4_210x297mm"
	MediaA5        = "iso_a5_148x210mm"
	MediaA6        = "iso_a6_105x148mm"
	MediaLetter    = "na_letter_8.5x11in"
	MediaLegal     = "na_legal_8.5x14in"
	MediaExecutive = "na_executive_7.25x10.5in"
	Media4x6       = "na_index-4x6_4x6in"
)

// Sheets builds fixed-size Media from PWG self-describing names, all with the same margins, type
// and source.
func Sheets(names []string, m Margins, mediaType, source string) ([]Media, error) {
	out := make([]Media, 0, len(names))
	for _, n := range names {
		x, y, ok := ParseMediaName(n)
		if !ok {
			return nil, fmt.Errorf("media name %q is not a PWG self-describing name", n)
		}
		out = append(out, Media{Name: n, Width: x, Length: y, Margins: m, Type: mediaType, Source: source})
	}
	return out, nil
}

// MediaName builds a PWG custom media name ("om_<w>x<l>mm_<w>x<l>mm") for a size in hundredths of
// a millimetre.
func MediaName(w, l int) string {
	d := MMString(w) + "x" + MMString(l) + "mm"
	return "om_" + d + "_" + d
}
