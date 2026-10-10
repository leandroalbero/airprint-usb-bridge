package cups

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// maxPPDSize bounds what LoadPPD reads; real PPDs are well under 1 MiB, the largest few MiB.
const maxPPDSize = 16 << 20

// PPD is the part of a PostScript Printer Description file the driver needs: UI options with
// their choices and defaults, and the main-keyword attributes (PaperDimension, ImageableArea,
// cupsFilter, ModelName, ...).
type PPD struct {
	Options []*Option         // UI options in file order
	Attrs   map[string][]Attr // every other "*Keyword spec: value" line, by keyword
	opts    map[string]*Option
}

// Attr is one "*Keyword Spec/Text: Value" line that is not an option choice.
type Attr struct {
	Spec, Text, Value string
}

// Option is an OpenUI/JCLOpenUI block.
type Option struct {
	Keyword string // without the leading '*'
	Text    string
	UI      string // PickOne, PickMany, Boolean
	Default string
	Order   float64 // OrderDependency; options without one sort last
	Choices []*Choice
	index   int
}

// Choice is one choice of an option; Code is its PostScript or JCL snippet.
type Choice struct {
	Name, Text, Code string
}

// LoadPPD reads a PPD file, gzip-compressed or not.
func LoadPPD(path string) (*PPD, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPPDSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		data, err = io.ReadAll(io.LimitReader(zr, maxPPDSize+1))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if len(data) > maxPPDSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxPPDSize)
	}
	p, err := ParsePPD(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ParsePPD parses PPD text.
func ParsePPD(data []byte) (*PPD, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), " \t\r\n"), []byte("*PPD-Adobe:")) {
		return nil, errors.New("not a PPD file (no *PPD-Adobe header)")
	}
	p := &PPD{Attrs: map[string][]Attr{}, opts: map[string]*Option{}}
	defaults := map[string]string{}
	orders := map[string]float64{}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "*") || strings.HasPrefix(line, "*%") {
			continue
		}
		head, value, hasValue := strings.Cut(line[1:], ":")
		if hasValue {
			value = strings.TrimLeft(value, " \t")
			if strings.HasPrefix(value, `"`) {
				// A quoted value may span lines; PPD strings cannot contain '"' (hex <22>
				// is used instead), so the next quote ends it.
				body := value[1:]
				if !strings.Contains(body, `"`) {
					// Find the closing line first and join once: appending line by line is
					// quadratic on an unterminated value.
					end := i + 1
					for end < len(lines)-1 && !strings.Contains(lines[end], `"`) {
						end++
					}
					if end < len(lines) {
						body += "\n" + strings.Join(lines[i+1:end+1], "\n")
						i = end
					}
				}
				if j := strings.Index(body, `"`); j >= 0 {
					body = body[:j]
				}
				value = body
			} else {
				value = strings.TrimSpace(value)
			}
		}
		keyword, spec, _ := strings.Cut(head, " ")
		keyword = strings.TrimSpace(keyword)
		spec = strings.TrimSpace(spec)
		name, trans, _ := strings.Cut(spec, "/")
		name = strings.TrimSpace(name)

		switch {
		case keyword == "OpenUI" || keyword == "JCLOpenUI":
			kw := strings.TrimPrefix(name, "*")
			if kw == "" {
				continue
			}
			if o, ok := p.opts[kw]; ok {
				o.UI = value
				continue
			}
			o := &Option{Keyword: kw, Text: trans, UI: value, index: len(p.Options)}
			p.Options = append(p.Options, o)
			p.opts[kw] = o
		case keyword == "OrderDependency" || keyword == "NonUIOrderDependency":
			f := strings.Fields(value)
			if len(f) >= 3 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					orders[strings.TrimPrefix(f[2], "*")] = v
				}
			}
		case strings.HasPrefix(keyword, "Default") && len(keyword) > len("Default") && spec == "":
			defaults[keyword[len("Default"):]] = value
		default:
			if o, ok := p.opts[keyword]; ok && name != "" {
				o.Choices = append(o.Choices, &Choice{Name: name, Text: trans, Code: value})
				continue
			}
			p.Attrs[keyword] = append(p.Attrs[keyword], Attr{Spec: name, Text: trans, Value: value})
		}
	}
	for _, o := range p.Options {
		o.Default = defaults[o.Keyword]
		if v, ok := orders[o.Keyword]; ok {
			o.Order = v
		} else {
			o.Order = 1e9
		}
		if o.Default == "" && len(o.Choices) > 0 {
			o.Default = o.Choices[0].Name
		}
	}
	for kw, v := range defaults {
		if _, ok := p.opts[kw]; !ok {
			p.Attrs["Default"+kw] = append(p.Attrs["Default"+kw], Attr{Value: v})
		}
	}
	return p, nil
}

// Option returns the UI option with the keyword (no leading '*').
func (p *PPD) Option(keyword string) *Option {
	return p.opts[keyword]
}

// Choice returns the named choice.
func (o *Option) Choice(name string) *Choice {
	if o == nil {
		return nil
	}
	for _, c := range o.Choices {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Attr returns the value of the first attribute with keyword and spec ("" matches any spec).
func (p *PPD) Attr(keyword, spec string) (string, bool) {
	for _, a := range p.Attrs[keyword] {
		if spec == "" || a.Spec == spec {
			return a.Value, true
		}
	}
	return "", false
}

// ordered returns the options sorted by OrderDependency, keeping file order for ties.
func (p *PPD) ordered() []*Option {
	out := append([]*Option(nil), p.Options...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Order < out[b].Order })
	return out
}

// Filter is one raster filter line: *cupsFilter2 "src dst cost program" or *cupsFilter
// "src cost program".
type Filter struct {
	Source, Dest string
	Cost         int
	Program      string
}

// RasterFilter returns the cheapest filter taking application/vnd.cups-raster. As in CUPS, any
// *cupsFilter2 line makes the *cupsFilter lines ignored.
func (p *PPD) RasterFilter() (Filter, error) {
	var all []Filter
	if lines := p.Attrs["cupsFilter2"]; len(lines) > 0 {
		for _, a := range lines {
			f := strings.Fields(a.Value)
			if len(f) < 4 {
				continue
			}
			cost, err := strconv.Atoi(f[2])
			if err != nil {
				continue
			}
			all = append(all, Filter{Source: f[0], Dest: f[1], Cost: cost, Program: strings.Join(f[3:], " ")})
		}
	} else {
		for _, a := range p.Attrs["cupsFilter"] {
			f := strings.Fields(a.Value)
			if len(f) < 3 {
				continue
			}
			cost, err := strconv.Atoi(f[1])
			if err != nil {
				continue
			}
			all = append(all, Filter{Source: f[0], Cost: cost, Program: strings.Join(f[2:], " ")})
		}
	}
	best := -1
	for i, f := range all {
		if f.Source == "application/vnd.cups-raster" && (best < 0 || f.Cost < all[best].Cost) {
			best = i
		}
	}
	if best < 0 {
		if len(all) == 0 {
			return Filter{}, errors.New("PPD has no *cupsFilter or *cupsFilter2 line (a PostScript PPD? try --driver postscript)")
		}
		var srcs []string
		for _, f := range all {
			srcs = append(srcs, f.Source)
		}
		return Filter{}, fmt.Errorf("PPD has no filter for application/vnd.cups-raster (it takes %s)", strings.Join(srcs, ", "))
	}
	return all[best], nil
}

// numbers parses a space-separated list of PPD numbers such as "18 36 594 756".
func numbers(s string, n int) ([]float64, bool) {
	f := strings.Fields(s)
	if len(f) != n {
		return nil, false
	}
	out := make([]float64, n)
	for i, v := range f {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, false
		}
		out[i] = x
	}
	return out, true
}

// parseResolution reads a Resolution choice name: "300dpi", "600x300dpi", "8dpmm".
func parseResolution(s string) (x, y int, ok bool) {
	unit := 1.0
	switch {
	case strings.HasSuffix(s, "dpi"):
		s = strings.TrimSuffix(s, "dpi")
	case strings.HasSuffix(s, "dpmm"):
		s, unit = strings.TrimSuffix(s, "dpmm"), 25.4
	default:
		return 0, 0, false
	}
	xs, ys, two := strings.Cut(s, "x")
	if !two {
		ys = xs
	}
	fx, err1 := strconv.ParseFloat(xs, 64)
	fy, err2 := strconv.ParseFloat(ys, 64)
	if err1 != nil || err2 != nil || fx <= 0 || fy <= 0 || fx > 10000 || fy > 10000 {
		return 0, 0, false
	}
	return int(fx*unit + 0.5), int(fy*unit + 0.5), true
}
