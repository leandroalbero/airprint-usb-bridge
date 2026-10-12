package ipp

import (
	"fmt"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
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

func mediaSize(x, y int) goipp.Collection {
	return goipp.Collection{memberInt("x-dimension", x), memberInt("y-dimension", y)}
}

func rollSize(r *driver.Roll) goipp.Collection {
	return goipp.Collection{
		memberInt("x-dimension", r.Width),
		goipp.MakeAttribute("y-dimension", goipp.TagRange, goipp.Range{Lower: r.MinLength, Upper: r.MaxLength}),
	}
}

func mediaCol(size goipp.Collection, m driver.Margins, source, mediaType string) goipp.Collection {
	return goipp.Collection{
		goipp.MakeAttribute("media-size", goipp.TagBeginCollection, size),
		memberInt("media-bottom-margin", m.Bottom),
		memberInt("media-left-margin", m.Left),
		memberInt("media-right-margin", m.Right),
		memberInt("media-top-margin", m.Top),
		goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(source)),
		goipp.MakeAttribute("media-type", mediaTypeTag(mediaType), goipp.String(mediaType)),
	}
}

// pwgMediaTypes are the media-type keywords of PWG 5100.7; anything else is sent as a name, as
// ippeveprinter does for "roll".
var pwgMediaTypes = []string{
	"auto", "cardstock", "continuous", "continuous-long", "continuous-short", "disc", "envelope",
	"labels", "labels-continuous", "photographic", "photographic-glossy", "photographic-high-gloss",
	"photographic-matte", "photographic-satin", "photographic-semi-gloss", "stationery",
	"stationery-coated", "stationery-heavyweight", "stationery-inkjet", "stationery-letterhead",
	"stationery-lightweight", "transparency",
}

func mediaTypeTag(t string) goipp.Tag {
	if contains(pwgMediaTypes, t) {
		return goipp.TagKeyword
	}
	return goipp.TagName
}

// mediaCols returns media-col-database (roll first, then the fixed sizes), media-col-ready and
// media-size-supported.
func mediaCols(c *driver.Capabilities) (db, ready, sizes []goipp.Value) {
	if r := c.Roll; r != nil {
		col := mediaCol(rollSize(r), r.Margins, r.Source, r.Type)
		db, ready, sizes = append(db, col), append(ready, col), append(sizes, rollSize(r))
	}
	for _, m := range c.Media {
		col := mediaCol(mediaSize(m.Width, m.Length), m.Margins, m.Source, m.Type)
		db, sizes = append(db, col), append(sizes, mediaSize(m.Width, m.Length))
		if m.Ready {
			ready = append(ready, col)
		}
	}
	return db, ready, sizes
}

func defaultMediaCol(c *driver.Capabilities) goipp.Collection {
	if r := c.Roll; r != nil {
		return mediaCol(mediaSize(r.Width, r.DefaultLength), r.Margins, r.Source, r.Type)
	}
	m := c.Default()
	return mediaCol(mediaSize(m.Width, m.Length), m.Margins, m.Source, m.Type)
}

func defaultMediaName(c *driver.Capabilities) string {
	if c.Roll != nil {
		return c.Roll.Name("roll_current", 0)
	}
	return c.Default().Name
}

// uniqueMedia lists a per-media value once each, roll first.
func uniqueMedia[T comparable](c *driver.Capabilities, get func(m driver.Margins, source, typ string) T) []T {
	var out []T
	add := func(v T) {
		for _, o := range out {
			if o == v {
				return
			}
		}
		out = append(out, v)
	}
	if r := c.Roll; r != nil {
		add(get(r.Margins, r.Source, r.Type))
	}
	for _, m := range c.Media {
		add(get(m.Margins, m.Source, m.Type))
	}
	return out
}

func mediaTypes(c *driver.Capabilities) goipp.Attribute {
	a := goipp.Attribute{Name: "media-type-supported"}
	for _, t := range uniqueMedia(c, func(_ driver.Margins, _, typ string) string { return typ }) {
		a.Values.Add(mediaTypeTag(t), goipp.String(t))
	}
	return a
}

func marginsSupported(c *driver.Capabilities, name string, side func(driver.Margins) int) goipp.Attribute {
	return mkInts(name, goipp.TagInteger, uniqueMedia(c, func(m driver.Margins, _, _ string) int { return side(m) })...)
}

// inputTray is printer-input-tray. The roll form, with hundredths of a millimetre under
// dimunit=micrometers, is what ippeveprinter sent and iOS accepted; keep it byte for byte.
func inputTray(c *driver.Capabilities) string {
	if r := c.Roll; r != nil {
		return fmt.Sprintf("type=continuousRoll;mediafeed=-1;mediaxfeed=%d;maxcapacity=-2;level=-2;status=0;"+
			"name=Roll;index=1;dimunit=micrometers;unit=sheets;medianame=%smm;", r.Width, driver.MMString(r.Width))
	}
	m := c.Default()
	return fmt.Sprintf("type=sheetFeedAutoRemovableTray;mediafeed=%d;mediaxfeed=%d;maxcapacity=-2;level=-2;status=0;"+
		"name=Main;index=1;dimunit=micrometers;unit=sheets;medianame=%s;", m.Length*10, m.Width*10, m.Name)
}

var operationsSupported = []goipp.Op{
	goipp.OpPrintJob, goipp.OpPrintURI, goipp.OpValidateJob, goipp.OpCreateJob,
	goipp.OpSendDocument, goipp.OpSendURI, goipp.OpCancelJob, goipp.OpGetJobAttributes,
	goipp.OpGetJobs, goipp.OpGetPrinterAttributes, goipp.OpCancelMyJobs, goipp.OpCloseJob,
	goipp.OpIdentifyPrinter,
}

var (
	scalingSupported   = []string{"auto", "auto-fit", "fill", "fit", "none"}
	whichJobsSupported = []string{"completed", "not-completed", "aborted", "all", "canceled",
		"pending", "pending-held", "processing", "processing-stopped"}
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

	caps := &c.Caps
	// Document printers get the further attributes IPP Everywhere (PWG 5100.12/5100.14) asks
	// for. Label printers keep the attribute set iOS has accepted from the Brother QL.
	doc := caps.IsDocument()
	var resolutions []goipp.Value
	for _, r := range caps.Resolutions {
		resolutions = append(resolutions, dpi(r.X, r.Y))
	}
	db, ready, sizes := mediaCols(caps)
	maxCopies := max(caps.MaxCopies, 1)
	sources := uniqueMedia(caps, func(_ driver.Margins, source, _ string) string { return source })

	add(d, mk("printer-make-and-model", goipp.TagText, goipp.String(caps.MakeModel)))
	add(d, mk("printer-kind", goipp.TagKeyword, strs(caps.Kind)...))
	add(d, mk("color-supported", goipp.TagBoolean, goipp.Boolean(caps.Color)))
	add(t, mkInts("copies-default", goipp.TagInteger, 1))
	add(t, mk("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: maxCopies}))
	add(d, mk("urf-supported", goipp.TagKeyword, strs(caps.URF)...))
	add(d, mk("pwg-raster-document-type-supported", goipp.TagKeyword, strs(caps.PWGTypes)...))
	add(d, mk("pwg-raster-document-resolution-supported", goipp.TagResolution, resolutions...))
	add(d, mk("pwg-raster-document-sheet-back", goipp.TagKeyword, kw("normal")...))
	add(d, mk("compression-supported", goipp.TagKeyword, kw("none")...))
	add(t, mk("printer-resolution-default", goipp.TagResolution, resolutions[0]))
	add(t, mk("printer-resolution-supported", goipp.TagResolution, resolutions...))
	add(t, mk("print-color-mode-default", goipp.TagKeyword, kw(caps.DefaultColorMode())...))
	add(t, mk("print-color-mode-supported", goipp.TagKeyword, strs(caps.ColorModes())...))
	add(t, mkInts("print-quality-default", goipp.TagEnum, caps.DefaultQuality()))
	add(t, mkInts("print-quality-supported", goipp.TagEnum, caps.Qualities...))
	add(t, mkInts("orientation-requested-default", goipp.TagEnum, 3))
	add(t, mkInts("orientation-requested-supported", goipp.TagEnum, 3, 4))
	add(d, mkInts("landscape-orientation-requested-preferred", goipp.TagEnum, 4))
	if doc {
		add(t, mk("print-scaling-default", goipp.TagKeyword, kw("auto")...))
	} else {
		add(t, mk("print-scaling-default", goipp.TagKeyword, kw("none")...))
	}
	add(t, mk("print-scaling-supported", goipp.TagKeyword, strs(scalingSupported)...))
	add(t, mk("sides-default", goipp.TagKeyword, kw("one-sided")...))
	add(t, mk("sides-supported", goipp.TagKeyword, strs(caps.SidesSupported())...))
	add(t, mkInts("finishings-default", goipp.TagEnum, 3))
	add(t, mkInts("finishings-supported", goipp.TagEnum, 3))
	if doc {
		add(t, mk("output-bin-default", goipp.TagKeyword, kw("face-down")...))
		add(t, mk("output-bin-supported", goipp.TagKeyword, kw("face-down")...))
		add(t, mk("print-content-optimize-default", goipp.TagKeyword, kw("auto")...))
		add(t, mk("print-content-optimize-supported", goipp.TagKeyword, kw("auto")...))
		add(t, mk("print-rendering-intent-default", goipp.TagKeyword, kw("auto")...))
		add(t, mk("print-rendering-intent-supported", goipp.TagKeyword, kw("auto")...))
	}
	add(t, mk("media-supported", goipp.TagKeyword, strs(caps.MediaNames())...))
	add(t, mk("media-default", goipp.TagKeyword, goipp.String(defaultMediaName(caps))))
	add(t, mk("media-ready", goipp.TagKeyword, strs(caps.ReadyNames())...))
	add(t, mk("media-source-supported", goipp.TagKeyword, strs(sources)...))
	add(t, mediaTypes(caps))
	add(t, marginsSupported(caps, "media-bottom-margin-supported", func(m driver.Margins) int { return m.Bottom }))
	add(t, marginsSupported(caps, "media-left-margin-supported", func(m driver.Margins) int { return m.Left }))
	add(t, marginsSupported(caps, "media-right-margin-supported", func(m driver.Margins) int { return m.Right }))
	add(t, marginsSupported(caps, "media-top-margin-supported", func(m driver.Margins) int { return m.Top }))
	add(t, mk("media-size-supported", goipp.TagBeginCollection, sizes...))
	add(x, mk("media-col-database", goipp.TagBeginCollection, db...))
	add(t, mk("media-col-ready", goipp.TagBeginCollection, ready...))
	add(t, mk("media-col-default", goipp.TagBeginCollection, defaultMediaCol(caps)))
	add(d, mk("printer-input-tray", goipp.TagString, goipp.Binary(inputTray(caps))))
	if doc {
		add(d, mk("printer-output-tray", goipp.TagString, goipp.Binary(
			"type=unRemovableBin;maxcapacity=-2;remaining=-2;status=0;name=Face Down;stackingorder=lastToFirst;pagedelivery=faceDown;")))
		// The bridge cannot read supply levels: one black supply of unknown level (-2).
		add(d, mk("printer-supply", goipp.TagString, goipp.Binary(
			"index=1;class=supplyThatIsConsumed;type=unknown;unit=percent;maxcapacity=100;level=-2;colorantname=black;")))
		add(d, mk("printer-supply-description", goipp.TagText, goipp.String("Black")))
		// No speed is known; 10 is ippeveprinter's default.
		add(d, mkInts("pages-per-minute", goipp.TagInteger, 10))
		// Required of a colour printer (PWG 5100.12), and must be absent otherwise.
		if caps.Color {
			add(d, mkInts("pages-per-minute-color", goipp.TagInteger, 10))
		}
	}
	add(d, mk("charset-configured", goipp.TagCharset, goipp.String("utf-8")))
	add(d, mk("charset-supported", goipp.TagCharset, kw("us-ascii", "utf-8")...))
	add(d, mk("document-format-default", goipp.TagMimeType, goipp.String(formatOctetStream)))
	add(d, mk("document-format-supported", goipp.TagMimeType, strs(c.documentFormats())...))
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
	add(d, mk("printer-device-id", goipp.TagText, goipp.String(caps.DeviceID)))
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
