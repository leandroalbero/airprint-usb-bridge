package ipp

import (
	"github.com/OpenPrinting/goipp"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
)

// textValue returns a text or name value, with or without language.
func textValue(a *goipp.Attribute) (string, bool) {
	if a == nil || len(a.Values) != 1 {
		return "", false
	}
	switch v := a.Values[0].V.(type) {
	case goipp.String:
		switch a.Values[0].T {
		case goipp.TagName, goipp.TagText:
			return string(v), true
		}
	case goipp.TextWithLang:
		return v.Text, true
	}
	return "", false
}

// operation attributes shared by the job-creating operations.
type jobRequest struct {
	name     string
	user     string
	format   string // as supplied, "" when absent
	fidelity bool
	attrs    JobAttrs
}

func (req *request) userName() string {
	if u, ok := textValue(find(req.ops, "requesting-user-name")); ok && u != "" {
		return u
	}
	return "anonymous"
}

// checkJobRequest validates the operation and job template attributes of
// Print-Job, Validate-Job and Create-Job. Unsupported values are collected in
// req.unsupported; they fail the request only with ipp-attribute-fidelity=true.
func (req *request) checkJobRequest() (jobRequest, *ippError) {
	jr := jobRequest{name: "Untitled", user: req.userName()}
	jr.attrs = JobAttrs{Copies: 1, PrintQuality: req.s.cfg.Caps.DefaultQuality()}
	if a := find(req.ops, "job-name"); a != nil {
		n, ok := textValue(a)
		if !ok {
			return jr, fail(goipp.StatusErrorBadRequest, "Bad job-name.")
		}
		jr.name = n
	} else if n, ok := textValue(find(req.ops, "document-name")); ok && n != "" {
		jr.name = n
	}
	if a := find(req.ops, "ipp-attribute-fidelity"); a != nil {
		b, ok := singleBool(a)
		if !ok {
			return jr, fail(goipp.StatusErrorBadRequest, "Bad ipp-attribute-fidelity.")
		}
		jr.fidelity = b
	}
	if err := req.checkDocumentOps(&jr.format); err != nil {
		return jr, err
	}

	var bad goipp.Attributes
	for i := range req.msg.Job {
		a := &req.msg.Job[i]
		if !req.s.checkTemplateAttr(a, &jr.attrs) {
			bad.Add(*a)
		}
	}
	if len(bad) > 0 {
		req.unsupported = append(req.unsupported, bad...)
		if jr.fidelity {
			return jr, fail(goipp.StatusErrorAttributesOrValues, "Unsupported job template attribute values.")
		}
		req.status = goipp.StatusOkIgnoredOrSubstituted
		req.statusMsg = "Unsupported attribute values were ignored."
	}
	return jr, nil
}

// checkDocumentOps validates document-format and compression.
func (req *request) checkDocumentOps(format *string) *ippError {
	if a := find(req.ops, "compression"); a != nil {
		c, ok := single(a, goipp.TagKeyword)
		if !ok || c != "none" {
			req.unsupported.Add(*a)
			return fail(goipp.StatusErrorCompressionNotSupported, "Unsupported compression %q.", c)
		}
	}
	if a := find(req.ops, "document-format"); a != nil {
		f, ok := single(a, goipp.TagMimeType)
		if !ok {
			return fail(goipp.StatusErrorBadRequest, "Bad document-format.")
		}
		if !contains(req.s.cfg.documentFormats(), f) {
			req.unsupported.Add(*a)
			return fail(goipp.StatusErrorDocumentFormatNotSupported, "Unsupported document-format %q.", f)
		}
		*format = f
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// checkTemplateAttr validates one job template attribute and records accepted
// values in ja. Attributes it does not know are ignored, as ippeveprinter does.
func (s *Server) checkTemplateAttr(a *goipp.Attribute, ja *JobAttrs) bool {
	switch a.Name {
	case "copies":
		n, ok := singleInt(a, goipp.TagInteger)
		if !ok || n < 1 || n > max(s.cfg.Caps.MaxCopies, 1) {
			return false
		}
		ja.Copies = n
	case "finishings":
		for _, v := range a.Values {
			if n, ok := v.V.(goipp.Integer); !ok || v.T != goipp.TagEnum || n != 3 {
				return false
			}
		}
		return len(a.Values) > 0
	case "sides":
		v, ok := single(a, goipp.TagKeyword)
		if !ok || !contains(s.cfg.Caps.SidesSupported(), v) {
			return false
		}
		ja.Sides = v
	case "job-sheets":
		v, ok := single(a, goipp.TagKeyword, goipp.TagName)
		return ok && v == "none"
	case "job-priority":
		n, ok := singleInt(a, goipp.TagInteger)
		return ok && n >= 1 && n <= 100
	case "multiple-document-handling":
		v, ok := single(a, goipp.TagKeyword)
		return ok && (v == "separate-documents-uncollated-copies" || v == "separate-documents-collated-copies")
	case "orientation-requested":
		n, ok := singleInt(a, goipp.TagEnum)
		if !ok || (n != 3 && n != 4) {
			return false
		}
		ja.Orientation = n
	case "print-color-mode":
		v, ok := single(a, goipp.TagKeyword)
		if !ok || !contains(s.cfg.Caps.ColorModes(), v) {
			return false
		}
		ja.ColorMode = v
	case "print-scaling":
		v, ok := single(a, goipp.TagKeyword)
		if !ok || !contains(scalingSupported, v) {
			return false
		}
		ja.Scaling = v
	case "print-quality":
		n, ok := singleInt(a, goipp.TagEnum)
		if !ok || !containsInt(s.cfg.Caps.Qualities, n) {
			return false
		}
		ja.PrintQuality = n
	case "printer-resolution":
		if len(a.Values) != 1 || a.Values[0].T != goipp.TagResolution {
			return false
		}
		r, ok := a.Values[0].V.(goipp.Resolution)
		if !ok || r.Units != goipp.UnitsDpi {
			return false
		}
		if !s.cfg.Caps.AcceptsResolution(driver.Resolution{X: r.Xres, Y: r.Yres}) {
			return false
		}
		ja.XDPI, ja.YDPI = r.Xres, r.Yres
	case "media":
		v, ok := single(a, goipp.TagKeyword, goipp.TagName)
		if !ok {
			return false
		}
		m, ok := s.cfg.Caps.LookupName(v)
		if !ok {
			return false
		}
		ja.Media = v
		if ja.MediaCol == nil {
			ja.Resolved = m
		}
	case "media-col":
		if len(a.Values) != 1 || a.Values[0].T != goipp.TagBeginCollection {
			return false
		}
		col, ok := a.Values[0].V.(goipp.Collection)
		if !ok {
			return false
		}
		mc, m, ok := s.checkMediaCol(col)
		if !ok {
			return false
		}
		ja.MediaCol = &mc
		ja.Resolved = m
	}
	return true
}

// checkMediaCol accepts a media-col whose size the printer offers: on a roll, an x-dimension
// within 1 mm of the roll width and a y-dimension inside the roll range (as a number or a range)
// or equal to a preset; otherwise a fixed size within 1 mm on both axes. Margins, source and type
// are not checked: iOS sends zero margins and no source or type.
func (s *Server) checkMediaCol(col goipp.Collection) (MediaCol, driver.Media, bool) {
	var mc MediaCol
	var media driver.Media
	caps := &s.cfg.Caps
	sized := false
	for i := range col {
		m := &col[i]
		switch m.Name {
		case "media-size":
			if len(m.Values) != 1 {
				return mc, media, false
			}
			size, ok := m.Values[0].V.(goipp.Collection)
			if !ok {
				return mc, media, false
			}
			var hasX, hasY bool
			var yRange *goipp.Range
			for k := range size {
				d := &size[k]
				if len(d.Values) != 1 {
					return mc, media, false
				}
				switch d.Name {
				case "x-dimension":
					x, ok := d.Values[0].V.(goipp.Integer)
					if !ok || !caps.MatchWidth(int(x)) {
						return mc, media, false
					}
					mc.XDimension, hasX = int(x), true
				case "y-dimension":
					switch y := d.Values[0].V.(type) {
					case goipp.Integer:
						mc.YDimension = int(y)
					case goipp.Range:
						yRange = &y
					default:
						return mc, media, false
					}
					hasY = true
				}
			}
			if !hasX || !hasY {
				return mc, media, false
			}
			if yRange != nil {
				media, ok = caps.MatchRange(mc.XDimension, yRange.Lower, yRange.Upper)
			} else {
				media, ok = caps.MatchSize(mc.XDimension, mc.YDimension)
			}
			if !ok {
				return mc, media, false
			}
			sized = true
		case "media-size-name":
			v, ok := single(m, goipp.TagKeyword, goipp.TagName)
			if !ok {
				return mc, media, false
			}
			mc.SizeName = v
		case "media-source":
			mc.Source, _ = single(m, goipp.TagKeyword, goipp.TagName)
		case "media-type":
			mc.Type, _ = single(m, goipp.TagKeyword, goipp.TagName)
		case "media-top-margin":
			mc.Top, _ = singleInt(m, goipp.TagInteger)
		case "media-bottom-margin":
			mc.Bottom, _ = singleInt(m, goipp.TagInteger)
		case "media-left-margin":
			mc.Left, _ = singleInt(m, goipp.TagInteger)
		case "media-right-margin":
			mc.Right, _ = singleInt(m, goipp.TagInteger)
		}
	}
	if !sized {
		if mc.SizeName == "" {
			return mc, caps.Default(), true
		}
		m, ok := caps.LookupName(mc.SizeName)
		if !ok {
			return mc, media, false
		}
		media = m
	}
	return mc, media, true
}

func containsInt(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}
