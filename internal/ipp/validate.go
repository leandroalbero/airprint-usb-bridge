package ipp

import (
	"regexp"
	"strconv"

	"github.com/OpenPrinting/goipp"
)

// rollTolerance is how far a media-col x-dimension may be from the roll width
// (1 mm): clients round 62 mm media differently.
const rollTolerance = 100

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
	jr.attrs = JobAttrs{Copies: 1, PrintQuality: 4}
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
		if !contains(formatsSupported, f) {
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
		return ok && n == 1
	case "finishings":
		for _, v := range a.Values {
			if n, ok := v.V.(goipp.Integer); !ok || v.T != goipp.TagEnum || n != 3 {
				return false
			}
		}
		return len(a.Values) > 0
	case "sides":
		v, ok := single(a, goipp.TagKeyword)
		return ok && v == "one-sided"
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
		if !ok || !contains(colorModesSupported, v) {
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
		if !ok || (n != 4 && !(n == 5 && s.cfg.HighRes)) {
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
		switch {
		case r.Xres == 300 && r.Yres == 300:
		case s.cfg.HighRes && r.Yres == 600 && (r.Xres == 300 || r.Xres == 600):
		default:
			return false
		}
		ja.XDPI, ja.YDPI = r.Xres, r.Yres
	case "media":
		v, ok := single(a, goipp.TagKeyword, goipp.TagName)
		if !ok || !s.mediaNameOK(v) {
			return false
		}
		ja.Media = v
	case "media-col":
		if len(a.Values) != 1 || a.Values[0].T != goipp.TagBeginCollection {
			return false
		}
		col, ok := a.Values[0].V.(goipp.Collection)
		if !ok {
			return false
		}
		mc, ok := s.checkMediaCol(col)
		if !ok {
			return false
		}
		ja.MediaCol = &mc
	}
	return true
}

// lengthOK accepts a label length inside the roll range or equal to a preset.
func (s *Server) lengthOK(y int) bool {
	if y >= s.cfg.RollMinLength && y <= s.cfg.RollMaxLength {
		return true
	}
	for _, p := range s.cfg.Presets {
		if y == p {
			return true
		}
	}
	return false
}

func (s *Server) widthOK(x int) bool {
	return x >= s.cfg.RollWidth-rollTolerance && x <= s.cfg.RollWidth+rollTolerance
}

// checkMediaCol accepts a media-col for the loaded roll: x-dimension within
// 1 mm of the roll width and y-dimension inside the roll range or equal to a
// preset. Margins, source and type are not checked: iOS sends zero margins and
// no source or type, and there is only one way to feed a roll.
func (s *Server) checkMediaCol(col goipp.Collection) (MediaCol, bool) {
	var mc MediaCol
	sized := false
	for i := range col {
		m := &col[i]
		switch m.Name {
		case "media-size":
			if len(m.Values) != 1 {
				return mc, false
			}
			size, ok := m.Values[0].V.(goipp.Collection)
			if !ok {
				return mc, false
			}
			var hasX, hasY bool
			for k := range size {
				d := &size[k]
				if len(d.Values) != 1 {
					return mc, false
				}
				switch d.Name {
				case "x-dimension":
					x, ok := d.Values[0].V.(goipp.Integer)
					if !ok || !s.widthOK(int(x)) {
						return mc, false
					}
					mc.XDimension, hasX = int(x), true
				case "y-dimension":
					switch y := d.Values[0].V.(type) {
					case goipp.Integer:
						if !s.lengthOK(int(y)) {
							return mc, false
						}
						mc.YDimension = int(y)
					case goipp.Range:
						if y.Lower < s.cfg.RollMinLength || y.Upper > s.cfg.RollMaxLength || y.Lower > y.Upper {
							return mc, false
						}
					default:
						return mc, false
					}
					hasY = true
				}
			}
			if !hasX || !hasY {
				return mc, false
			}
			sized = true
		case "media-size-name":
			v, ok := single(m, goipp.TagKeyword, goipp.TagName)
			if !ok {
				return mc, false
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
	if !sized && mc.SizeName != "" && !s.mediaNameOK(mc.SizeName) {
		return mc, false
	}
	return mc, true
}

var pwgMediaName = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9.-]*_([0-9]+(?:\.[0-9]+)?)x([0-9]+(?:\.[0-9]+)?)(mm|in)$`)

// mediaNameOK accepts the advertised media keywords and any PWG self-describing
// name whose size fits the roll.
func (s *Server) mediaNameOK(name string) bool {
	if contains(s.cfg.mediaSupported(), name) || contains(s.cfg.mediaReady(), name) {
		return true
	}
	m := pwgMediaName.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	x, y := toHundredthsMM(m[1], m[3]), toHundredthsMM(m[2], m[3])
	if x < 0 || y < 0 {
		return false
	}
	return s.widthOK(x) && s.lengthOK(y)
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
