package dymo

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
)

// label is a die-cut label size. Width is across the print head and Length along the feed, in
// hundredths of a millimetre; DYMO's own size names list the long side first (99012 "89 x 36 mm"
// is 36 mm across the head).
type label struct {
	IDs    []string // --media ids: DYMO part numbers (EU first, then US) and aliases
	Name   string   // PWG name; "" = driver.MediaName(Width, Length)
	Width  int
	Length int
	Desc   string
	XLOnly bool // only fits the 4XL
}

var labels = []label{
	{IDs: []string{"99012", "30321", "large-address"}, Width: 3600, Length: 8900, Desc: "large address 36x89 mm"},
	{IDs: []string{"99010", "30252", "address"}, Width: 2800, Length: 8900, Desc: "address 28x89 mm"},
	{IDs: []string{"11352", "30336"}, Width: 2500, Length: 5400, Desc: "return address / multi-purpose 25x54 mm"},
	{IDs: []string{"11355", "30330"}, Width: 1900, Length: 5100, Desc: "multi-purpose 19x51 mm"},
	{IDs: []string{"11354", "30334"}, Width: 5700, Length: 3200, Desc: "multi-purpose 57x32 mm"},
	{IDs: []string{"30332"}, Width: 2500, Length: 2500, Desc: "square 25x25 mm"},
	{IDs: []string{"99014"}, Width: 5400, Length: 10100, Desc: "shipping 54x101 mm"},
	{IDs: []string{"30256"}, Width: 5900, Length: 10200, Desc: "shipping 59x102 mm"},
	{IDs: []string{"1744907", "4x6"}, Name: driver.Media4x6, Width: 10160, Length: 15240, Desc: "4XL shipping 4x6 in", XLOnly: true},
	{IDs: []string{"s0904980", "104x159"}, Width: 10400, Length: 15900, Desc: "4XL shipping 104x159 mm", XLOnly: true},
}

func (l label) name() string {
	if l.Name != "" {
		return l.Name
	}
	return driver.MediaName(l.Width, l.Length)
}

// MediaHelp is the --media help text.
const MediaHelp = "a die-cut label: 99012/30321 (default, 36x89 mm), 99010/30252 (28x89), 11352/30336 (25x54), " +
	"11355/30330 (19x51), 11354/30334 (57x32), 30332 (25x25), 99014 (54x101), 30256 (59x102); " +
	"4XL also 1744907/4x6 (default on the 4XL) and S0904980 (104x159); " +
	"or continuous (full head width) / continuous-<mm> for continuous paper"

// selection is the loaded media resolved from --media.
type selection struct {
	continuous bool
	width      int   // continuous paper width, hundredths of a millimetre
	label      label // die-cut label
}

const minContinuousWidth = 1000

func (m model) labels() []label {
	var out []label
	for _, l := range labels {
		if !l.XLOnly || m.xl {
			out = append(out, l)
		}
	}
	return out
}

func (m model) selectMedia(id string) (selection, error) {
	key := strings.ToLower(strings.TrimSpace(id))
	if key == "" {
		key = "99012"
		if m.xl {
			key = "4x6"
		}
	}
	if key == "continuous" {
		// Whole millimetres keep the PWG roll names short ("roll_current_56x0mm").
		return selection{continuous: true, width: m.headWidth() / 100 * 100}, nil
	}
	if rest, ok := strings.CutPrefix(key, "continuous-"); ok {
		mm, err := strconv.ParseFloat(strings.TrimSuffix(rest, "mm"), 64)
		w := int(math.Round(mm * 100))
		if err != nil || w < minContinuousWidth || w > m.maxWidth {
			return selection{}, fmt.Errorf("--media %q: continuous width must be %s-%s mm",
				id, driver.MMString(minContinuousWidth), driver.MMString(m.maxWidth))
		}
		return selection{continuous: true, width: w}, nil
	}
	for _, l := range m.labels() {
		if l.name() == key {
			return selection{label: l}, nil
		}
		for _, lid := range l.IDs {
			if lid == key {
				return selection{label: l}, nil
			}
		}
	}
	var ids []string
	for _, l := range m.labels() {
		ids = append(ids, strings.Join(l.IDs, "/"))
	}
	return selection{}, fmt.Errorf("--media %q: unknown for the %s (have %s, continuous, continuous-<mm>)",
		id, m.name, strings.Join(ids, ", "))
}

// margins keeps 1 mm clear of the label edges (the label position on the head is not exact) and
// blanks whatever lies past the last printable dot.
func (m model) margins(width int, roll bool) driver.Margins {
	right := max(edgeMargin, width-m.printableWidth())
	if roll {
		return driver.Margins{Left: edgeMargin, Right: right}
	}
	return driver.Margins{Top: edgeMargin, Bottom: edgeMargin, Left: edgeMargin, Right: right}
}

const edgeMargin = 100
