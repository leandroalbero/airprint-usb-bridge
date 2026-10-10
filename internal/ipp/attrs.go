package ipp

import (
	"fmt"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
)

// attrGroup says which requested-attributes group keywords select an attribute.
type attrGroup uint8

const (
	groupDescription attrGroup = 1 << iota
	groupTemplate
	// groupExplicit attributes come back only when named, never for "all".
	groupExplicit
)

type printerAttr struct {
	name  string
	group attrGroup
	attr  goipp.Attribute               // static value
	dyn   func(v *view) goipp.Attribute // per-request value when non-nil
}

// view carries what per-request attributes depend on.
type view struct {
	host   string // host[:port] as the client addressed us, trailing dot removed
	now    time.Time
	status printerStatus
}

type printerStatus struct {
	state      PrinterState
	reasons    []string
	message    string
	changed    time.Time
	changedUp  int
	upTime     int
	queued     int
	configTime time.Time
}

func kw(vals ...string) []goipp.Value { return strs(vals) }

func strs(vals []string) []goipp.Value {
	out := make([]goipp.Value, len(vals))
	for i, v := range vals {
		out[i] = goipp.String(v)
	}
	return out
}

func mk(name string, tag goipp.Tag, vals ...goipp.Value) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, v := range vals {
		a.Values.Add(tag, v)
	}
	return a
}

func mkInts(name string, tag goipp.Tag, vals ...int) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, v := range vals {
		a.Values.Add(tag, goipp.Integer(v))
	}
	return a
}

func dpi(x, y int) goipp.Value {
	return goipp.Resolution{Xres: x, Yres: y, Units: goipp.UnitsDpi}
}

func memberInt(name string, v int) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(v))
}

// mediaSize returns a media-size collection; y < 0 means the roll range.
func (c *Config) mediaSize(y int) goipp.Collection {
	col := goipp.Collection{memberInt("x-dimension", c.RollWidth)}
	if y < 0 {
		col = append(col, goipp.MakeAttribute("y-dimension", goipp.TagRange,
			goipp.Range{Lower: c.RollMinLength, Upper: c.RollMaxLength}))
	} else {
		col = append(col, memberInt("y-dimension", y))
	}
	return col
}

func (c *Config) mediaCol(y int) goipp.Collection {
	return goipp.Collection{
		goipp.MakeAttribute("media-size", goipp.TagBeginCollection, c.mediaSize(y)),
		memberInt("media-bottom-margin", c.Margins.Bottom),
		memberInt("media-left-margin", c.Margins.Left),
		memberInt("media-right-margin", c.Margins.Right),
		memberInt("media-top-margin", c.Margins.Top),
		goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String("main")),
		goipp.MakeAttribute("media-type", goipp.TagName, goipp.String("roll")),
	}
}

func (c *Config) presetName(y int) string {
	d := mmString(c.RollWidth) + "x" + mmString(y) + "mm"
	return "om_" + d + "_" + d
}

func (c *Config) rollName(kind string, y int) string {
	return kind + "_" + mmString(c.RollWidth) + "x" + mmString(y) + "mm"
}

func (c *Config) mediaSupported() []string {
	var out []string
	for _, p := range c.Presets {
		out = append(out, c.presetName(p))
	}
	return append(out,
		c.rollName("roll_min", c.RollMinLength), c.rollName("roll_max", c.RollMaxLength),
		c.rollName("custom_min", c.RollMinLength), c.rollName("custom_max", c.RollMaxLength))
}

func (c *Config) mediaReady() []string {
	out := []string{c.rollName("roll_current", 0)}
	if c.MediaPolicy == MediaRollPresets {
		for _, p := range c.Presets {
			out = append(out, c.presetName(p))
		}
	}
	return out
}

var operationsSupported = []goipp.Op{
	goipp.OpPrintJob, goipp.OpPrintURI, goipp.OpValidateJob, goipp.OpCreateJob,
	goipp.OpSendDocument, goipp.OpSendURI, goipp.OpCancelJob, goipp.OpGetJobAttributes,
	goipp.OpGetJobs, goipp.OpGetPrinterAttributes, goipp.OpCancelMyJobs, goipp.OpCloseJob,
	goipp.OpIdentifyPrinter,
}

var (
	colorModesSupported = []string{"monochrome", "auto", "auto-monochrome"}
	scalingSupported    = []string{"auto", "auto-fit", "fill", "fit", "none"}
	whichJobsSupported  = []string{"completed", "not-completed", "aborted", "all", "canceled",
		"pending", "pending-held", "processing", "processing-stopped"}
	formatsSupported = []string{formatOctetStream, formatPWG, formatURF}
)

const (
	formatOctetStream = "application/octet-stream"
	formatPWG         = "image/pwg-raster"
	formatURF         = "image/urf"
)

// buildPrinterAttrs returns the printer attributes in the order ippeveprinter
// sends them for the live attribute file, which is the order iOS has accepted.
func buildPrinterAttrs(c *Config, maxKOctets int) []printerAttr {
	const d, t, x = groupDescription, groupTemplate, groupExplicit
	var list []printerAttr
	add := func(g attrGroup, a goipp.Attribute) {
		list = append(list, printerAttr{name: a.Name, group: g, attr: a})
	}
	dyn := func(g attrGroup, name string, f func(v *view) goipp.Attribute) {
		list = append(list, printerAttr{name: name, group: g, dyn: f})
	}

	resolutions := []goipp.Value{dpi(300, 300)}
	qualities := []int{4}
	if c.HighRes {
		resolutions = append(resolutions, dpi(600, 600))
		qualities = append(qualities, 5)
	}
	var sizes, db, ready []goipp.Value
	sizes = append(sizes, c.mediaSize(-1))
	db = append(db, c.mediaCol(-1))
	ready = append(ready, c.mediaCol(-1))
	for _, p := range c.Presets {
		sizes = append(sizes, c.mediaSize(p))
		db = append(db, c.mediaCol(p))
		if c.MediaPolicy == MediaRollPresets {
			ready = append(ready, c.mediaCol(p))
		}
	}
	tray := fmt.Sprintf("type=continuousRoll;mediafeed=-1;mediaxfeed=%d;maxcapacity=-2;level=-2;status=0;"+
		"name=Roll;index=1;dimunit=micrometers;unit=sheets;medianame=%s;", c.RollWidth, c.TrayName)

	add(d, mk("printer-make-and-model", goipp.TagText, goipp.String(c.MakeModel)))
	add(d, mk("printer-kind", goipp.TagKeyword, kw("label", "roll")...))
	add(d, mk("color-supported", goipp.TagBoolean, goipp.Boolean(false)))
	add(t, mkInts("copies-default", goipp.TagInteger, 1))
	add(t, mk("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 1}))
	add(d, mk("urf-supported", goipp.TagKeyword, strs(c.URFSupported())...))
	add(d, mk("pwg-raster-document-type-supported", goipp.TagKeyword, kw("sgray_8")...))
	add(d, mk("pwg-raster-document-resolution-supported", goipp.TagResolution, resolutions...))
	add(d, mk("pwg-raster-document-sheet-back", goipp.TagKeyword, kw("normal")...))
	add(d, mk("compression-supported", goipp.TagKeyword, kw("none")...))
	add(t, mk("printer-resolution-default", goipp.TagResolution, dpi(300, 300)))
	add(t, mk("printer-resolution-supported", goipp.TagResolution, resolutions...))
	add(t, mk("print-color-mode-default", goipp.TagKeyword, kw("monochrome")...))
	add(t, mk("print-color-mode-supported", goipp.TagKeyword, strs(colorModesSupported)...))
	add(t, mkInts("print-quality-default", goipp.TagEnum, 4))
	add(t, mkInts("print-quality-supported", goipp.TagEnum, qualities...))
	add(t, mkInts("orientation-requested-default", goipp.TagEnum, 3))
	add(t, mkInts("orientation-requested-supported", goipp.TagEnum, 3, 4))
	add(d, mkInts("landscape-orientation-requested-preferred", goipp.TagEnum, 4))
	add(t, mk("print-scaling-default", goipp.TagKeyword, kw("none")...))
	add(t, mk("print-scaling-supported", goipp.TagKeyword, strs(scalingSupported)...))
	add(t, mk("sides-default", goipp.TagKeyword, kw("one-sided")...))
	add(t, mk("sides-supported", goipp.TagKeyword, kw("one-sided")...))
	add(t, mkInts("finishings-default", goipp.TagEnum, 3))
	add(t, mkInts("finishings-supported", goipp.TagEnum, 3))
	add(t, mk("media-supported", goipp.TagKeyword, strs(c.mediaSupported())...))
	add(t, mk("media-default", goipp.TagKeyword, goipp.String(c.rollName("roll_current", 0))))
	add(t, mk("media-ready", goipp.TagKeyword, strs(c.mediaReady())...))
	add(t, mk("media-source-supported", goipp.TagKeyword, kw("main")...))
	add(t, mk("media-type-supported", goipp.TagName, kw("roll")...))
	add(t, mkInts("media-bottom-margin-supported", goipp.TagInteger, c.Margins.Bottom))
	add(t, mkInts("media-left-margin-supported", goipp.TagInteger, c.Margins.Left))
	add(t, mkInts("media-right-margin-supported", goipp.TagInteger, c.Margins.Right))
	add(t, mkInts("media-top-margin-supported", goipp.TagInteger, c.Margins.Top))
	add(t, mk("media-size-supported", goipp.TagBeginCollection, sizes...))
	add(x, mk("media-col-database", goipp.TagBeginCollection, db...))
	add(t, mk("media-col-ready", goipp.TagBeginCollection, ready...))
	add(t, mk("media-col-default", goipp.TagBeginCollection, c.mediaCol(c.DefaultLength)))
	add(d, mk("printer-input-tray", goipp.TagString, goipp.Binary(tray)))
	add(d, mk("charset-configured", goipp.TagCharset, goipp.String("utf-8")))
	add(d, mk("charset-supported", goipp.TagCharset, kw("us-ascii", "utf-8")...))
	add(d, mk("document-format-default", goipp.TagMimeType, goipp.String(formatOctetStream)))
	add(d, mk("document-format-supported", goipp.TagMimeType, strs(formatsSupported)...))
	add(d, mk("generated-natural-language-supported", goipp.TagLanguage, goipp.String("en")))
	add(d, mk("identify-actions-default", goipp.TagKeyword, kw("sound")...))
	add(d, mk("identify-actions-supported", goipp.TagKeyword, kw("display", "sound")...))
	add(d, mk("ipp-features-supported", goipp.TagKeyword, kw("ipp-everywhere")...))
	add(d, mk("ipp-versions-supported", goipp.TagKeyword, kw("1.1", "2.0")...))
	add(d, mk("job-creation-attributes-supported", goipp.TagKeyword, kw(
		"document-access", "document-charset", "document-format", "document-message", "document-metadata",
		"document-name", "document-natural-language", "ipp-attribute-fidelity", "job-name", "job-priority",
		"copies", "finishings", "media", "orientation-requested", "print-color-mode", "print-quality",
		"print-scaling", "printer-resolution", "sides")...))
	add(d, mk("job-ids-supported", goipp.TagBoolean, goipp.Boolean(true)))
	add(d, mk("job-k-octets-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: maxKOctets}))
	add(t, mkInts("job-priority-default", goipp.TagInteger, 50))
	add(t, mkInts("job-priority-supported", goipp.TagInteger, 1))
	add(t, mk("job-sheets-default", goipp.TagName, kw("none")...))
	add(t, mk("job-sheets-supported", goipp.TagName, kw("none")...))
	add(t, mk("media-col-supported", goipp.TagKeyword, kw("media-bottom-margin", "media-left-margin",
		"media-right-margin", "media-size", "media-size-name", "media-source", "media-top-margin", "media-type")...))
	add(t, mk("multiple-document-handling-supported", goipp.TagKeyword,
		kw("separate-documents-uncollated-copies", "separate-documents-collated-copies")...))
	add(d, mk("multiple-document-jobs-supported", goipp.TagBoolean, goipp.Boolean(false)))
	add(d, mkInts("multiple-operation-time-out", goipp.TagInteger, int(multipleOperationTimeout/time.Second)))
	add(d, mk("multiple-operation-time-out-action", goipp.TagKeyword, kw("abort-job")...))
	add(d, mk("natural-language-configured", goipp.TagLanguage, goipp.String("en")))
	ops := goipp.Attribute{Name: "operations-supported"}
	for _, op := range operationsSupported {
		ops.Values.Add(goipp.TagEnum, goipp.Integer(op))
	}
	add(d, ops)
	add(d, mk("pdl-override-supported", goipp.TagKeyword, kw("attempted")...))
	add(d, mk("preferred-attributes-supported", goipp.TagBoolean, goipp.Boolean(false)))
	add(d, mk("printer-get-attributes-supported", goipp.TagKeyword, kw("document-format")...))
	add(d, mk("printer-geo-location", goipp.TagUnknown, goipp.Void{}))
	dyn(d, "printer-is-accepting-jobs", func(*view) goipp.Attribute {
		return mk("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(true))
	})
	add(d, mk("printer-info", goipp.TagText, goipp.String(c.Name)))
	add(d, mk("printer-location", goipp.TagText, goipp.String(c.Location)))
	add(d, mk("printer-name", goipp.TagName, goipp.String(c.Name)))
	add(d, mk("printer-organization", goipp.TagText, goipp.String("")))
	add(d, mk("printer-organizational-unit", goipp.TagText, goipp.String("")))
	add(d, mk("printer-strings-languages-supported", goipp.TagLanguage, goipp.String("en")))
	add(d, mk("printer-uuid", goipp.TagURI, goipp.String(c.UUID)))
	add(d, mk("reference-uri-schemes-supported", goipp.TagURIScheme, kw("file", "ftp", "http", "https")...))
	add(d, mk("uri-authentication-supported", goipp.TagKeyword, kw("none", "none")...))
	add(d, mk("uri-security-supported", goipp.TagKeyword, kw("none", "tls")...))
	add(d, mk("which-jobs-supported", goipp.TagKeyword, strs(whichJobsSupported)...))
	dyn(d, "printer-config-change-date-time", func(v *view) goipp.Attribute {
		return mk("printer-config-change-date-time", goipp.TagDateTime, goipp.Time{Time: v.status.configTime.UTC()})
	})
	dyn(d, "printer-config-change-time", func(*view) goipp.Attribute {
		return mkInts("printer-config-change-time", goipp.TagInteger, 0)
	})
	dyn(d, "printer-current-time", func(v *view) goipp.Attribute {
		return mk("printer-current-time", goipp.TagDateTime, goipp.Time{Time: v.now.UTC()})
	})
	add(d, mk("printer-device-id", goipp.TagText, goipp.String(c.DeviceID)))
	dyn(d, "printer-icons", func(v *view) goipp.Attribute {
		return mk("printer-icons", goipp.TagURI, goipp.String("https://"+v.host+"/icon-sm.png"),
			goipp.String("https://"+v.host+"/icon.png"), goipp.String("https://"+v.host+"/icon-lg.png"))
	})
	dyn(d, "printer-more-info", func(v *view) goipp.Attribute {
		return mk("printer-more-info", goipp.TagURI, goipp.String("https://"+v.host+"/"))
	})
	dyn(d, "printer-state", func(v *view) goipp.Attribute {
		return mkInts("printer-state", goipp.TagEnum, int(v.status.state))
	})
	dyn(d, "printer-state-change-date-time", func(v *view) goipp.Attribute {
		return mk("printer-state-change-date-time", goipp.TagDateTime, goipp.Time{Time: v.status.changed.UTC()})
	})
	dyn(d, "printer-state-change-time", func(v *view) goipp.Attribute {
		return mkInts("printer-state-change-time", goipp.TagInteger, v.status.changedUp)
	})
	dyn(d, "printer-state-message", func(v *view) goipp.Attribute {
		return mk("printer-state-message", goipp.TagText, goipp.String(v.status.message))
	})
	dyn(d, "printer-state-reasons", func(v *view) goipp.Attribute {
		return mk("printer-state-reasons", goipp.TagKeyword, strs(v.status.reasons)...)
	})
	dyn(d, "printer-strings-uri", func(v *view) goipp.Attribute {
		return mk("printer-strings-uri", goipp.TagURI, goipp.String("https://"+v.host+"/en.strings"))
	})
	dyn(d, "printer-supply-info-uri", func(v *view) goipp.Attribute {
		return mk("printer-supply-info-uri", goipp.TagURI, goipp.String("https://"+v.host+"/supplies"))
	})
	dyn(d, "printer-up-time", func(v *view) goipp.Attribute {
		return mkInts("printer-up-time", goipp.TagInteger, v.status.upTime)
	})
	dyn(d, "printer-uri-supported", func(v *view) goipp.Attribute {
		return mk("printer-uri-supported", goipp.TagURI,
			goipp.String("ipp://"+v.host+printerPath), goipp.String("ipps://"+v.host+printerPath))
	})
	dyn(d, "queued-job-count", func(v *view) goipp.Attribute {
		return mkInts("queued-job-count", goipp.TagInteger, v.status.queued)
	})
	return list
}

// requested is a parsed requested-attributes. "all" and the group keywords
// never select explicit-only attributes such as media-col-database, matching
// ippeveprinter; iOS asks for "all,media-col-database".
type requested struct {
	all, description, template bool
	names                      map[string]bool
}

func parseRequested(a *goipp.Attribute, defaultAll bool) requested {
	r := requested{names: map[string]bool{}}
	if a == nil {
		r.all = defaultAll
		return r
	}
	for _, v := range a.Values {
		s, ok := v.V.(goipp.String)
		if !ok {
			continue
		}
		switch name := strings.TrimSpace(string(s)); name {
		case "all":
			r.all = true
		case "printer-description", "job-description":
			r.description = true
		case "job-template":
			r.template = true
		default:
			r.names[name] = true
		}
	}
	return r
}

func (r requested) wants(name string, g attrGroup) bool {
	if r.names[name] {
		return true
	}
	if g&groupExplicit != 0 {
		return false
	}
	return r.all || (r.description && g&groupDescription != 0) || (r.template && g&groupTemplate != 0)
}
