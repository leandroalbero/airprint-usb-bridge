package ipp

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
)

// docCaps describe a duplex mono laser that also takes PDF.
func docCaps(t *testing.T) driver.Capabilities {
	t.Helper()
	m := driver.Margins{Top: 423, Bottom: 423, Left: 423, Right: 423}
	media, err := driver.Sheets([]string{driver.MediaA4, driver.MediaLetter, driver.MediaLegal, driver.MediaA5}, m, "stationery", "main")
	if err != nil {
		t.Fatal(err)
	}
	media[0].Ready, media[1].Ready = true, true
	return driver.Capabilities{
		MakeModel:   "Example Laser 1000",
		Kind:        []string{"document"},
		Duplex:      true,
		Resolutions: []driver.Resolution{{X: 600, Y: 600}, {X: 300, Y: 300}},
		Qualities:   []int{driver.QualityDraft, driver.QualityNormal, driver.QualityHigh},
		URF:         []string{"V1.4", "W8", "DM1", "CP99", "IS1", "MT1", "RS300-600"},
		PWGTypes:    []string{"sgray_8"},
		Formats:     []string{"application/pdf"},
		MaxCopies:   99,
		Media:       media,
	}
}

func docConfig(t *testing.T) Config {
	c := testConfig()
	c.Caps = docCaps(t)
	return c
}

func TestDocumentPrinterAttributes(t *testing.T) {
	ts := startServer(t, docConfig(t), (&recorder{}).process)
	pa := ts.do(t, "ipp", opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipp"), requestedAttrs("all", "media-col-database")), nil)
	want := map[string]string{
		"printer-kind":                       "[document]",
		"copies-supported":                   "[1-99]",
		"sides-supported":                    "[one-sided two-sided-long-edge two-sided-short-edge]",
		"sides-default":                      "[one-sided]",
		"media-default":                      "[iso_a4_210x297mm]",
		"media-ready":                        "[iso_a4_210x297mm na_letter_8.5x11in]",
		"media-supported":                    "[iso_a4_210x297mm na_letter_8.5x11in na_legal_8.5x14in iso_a5_148x210mm]",
		"media-type-supported":               "[stationery]",
		"media-left-margin-supported":        "[423]",
		"printer-resolution-default":         "[600x600dpi]",
		"print-quality-supported":            "[3 4 5]",
		"print-quality-default":              "[4]",
		"document-format-supported":          "[application/octet-stream application/pdf image/pwg-raster image/urf]",
		"urf-supported":                      "[V1.4 W8 DM1 CP99 IS1 MT1 RS300-600]",
		"printer-device-id":                  "[MFG:Example;MDL:Laser 1000;CMD:URF,PWG,PDF;]",
		"print-color-mode-supported":         "[monochrome auto auto-monochrome]",
		"pwg-raster-document-type-supported": "[sgray_8]",
		"print-scaling-default":              "[auto]",
		"output-bin-supported":               "[face-down]",
		"print-content-optimize-supported":   "[auto]",
		"print-rendering-intent-supported":   "[auto]",
		"printer-supply-description":         "[Black]",
		"pages-per-minute":                   "[10]",
	}
	for name, w := range want {
		a, ok := attrOf(pa.Printer, name)
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		var vals []string
		for _, v := range a.Values {
			vals = append(vals, v.V.String())
		}
		if got := "[" + strings.Join(vals, " ") + "]"; got != w {
			t.Errorf("%s = %s, want %s", name, got, w)
		}
	}
	if a, _ := attrOf(pa.Printer, "media-type-supported"); a.Values[0].T != goipp.TagKeyword {
		t.Errorf("media-type-supported tag %s", a.Values[0].T)
	}
	for name, n := range map[string]int{"media-col-database": 4, "media-col-ready": 2, "media-size-supported": 4} {
		if a, _ := attrOf(pa.Printer, name); len(a.Values) != n {
			t.Errorf("%s has %d values, want %d", name, len(a.Values), n)
		}
	}
	tray, _ := attrOf(pa.Printer, "printer-input-tray")
	if s := string(tray.Values[0].V.(goipp.Binary)); !strings.Contains(s, "type=sheetFeedAutoRemovableTray;mediafeed=297000;mediaxfeed=210000;") {
		t.Errorf("printer-input-tray %s", s)
	}
	txt := ts.cfg.TXT("printer.local", 8631)
	if txt["pdl"] != "application/pdf,image/pwg-raster,image/urf" || txt["Duplex"] != "T" || txt["Color"] != "F" || txt["ty"] != "Example Laser 1000" ||
		txt["kind"] != "document" || txt["product"] != "(Example Laser 1000)" || txt["PaperMax"] != "legal-A4" {
		t.Errorf("TXT %v", txt)
	}
	for _, name := range []string{"printer-output-tray", "printer-supply"} {
		if a, ok := attrOf(pa.Printer, name); !ok || a.Values[0].T != goipp.TagString {
			t.Errorf("%s: %v", name, a)
		}
	}
	resp, err := ts.client.Get("http://" + ts.addr + "/en.strings")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "Roll") {
		t.Errorf("a sheet printer's en.strings names a roll: %q", body)
	}
}

func TestPaperMax(t *testing.T) {
	sheets := func(names ...string) []driver.Media {
		m, err := driver.Sheets(names, driver.Margins{}, "stationery", "main")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for want, media := range map[string][]driver.Media{
		"<legal-A4":  sheets(driver.MediaA5, driver.MediaLetter),
		"legal-A4":   sheets(driver.MediaA4, driver.MediaLegal),
		"tabloid-A3": sheets(driver.MediaA4, "iso_a3_297x420mm"),
		"isoC-A2":    sheets("iso_a2_420x594mm"),
		">isoC-A2":   sheets("iso_a1_594x841mm"),
	} {
		if got := paperMax(media); got != want {
			t.Errorf("%v: %s, want %s", media[len(media)-1].Name, got, want)
		}
	}
}

func sizeCol(x, y int) goipp.Attribute {
	size := goipp.Collection{
		goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(x)),
		goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(y)),
	}
	return goipp.MakeAttribute("media-col", goipp.TagBeginCollection,
		goipp.Collection{goipp.MakeAttribute("media-size", goipp.TagBeginCollection, size)})
}

func TestDocumentPrinterValidateJob(t *testing.T) {
	rec := &recorder{}
	ts := startServer(t, docConfig(t), rec.process)
	yes := true
	kwAttr := func(name, v string) goipp.Attribute {
		return goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(v))
	}
	intAttr := func(name string, n int) goipp.Attribute {
		return goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(n))
	}
	rangeCol := goipp.MakeAttribute("media-col", goipp.TagBeginCollection, goipp.Collection{
		goipp.MakeAttribute("media-size", goipp.TagBeginCollection, goipp.Collection{
			goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(21000)),
			goipp.MakeAttribute("y-dimension", goipp.TagRange, goipp.Range{Lower: 800, Upper: 100000}),
		})})
	cases := []struct {
		name string
		attr goipp.Attribute
		ok   bool
	}{
		{"A4", sizeCol(21000, 29700), true},
		{"Letter rounded", sizeCol(21590, 27940), true},
		{"A5 within 1 mm", sizeCol(14850, 21000), true},
		{"A3", sizeCol(29700, 42000), false},
		{"A4 width, Letter length", sizeCol(21000, 27940), false},
		{"length range", rangeCol, false},
		{"letter keyword", kwAttr("media", driver.MediaLetter), true},
		{"pwg custom A4", kwAttr("media", "om_x_210x297mm"), true},
		{"roll keyword", kwAttr("media", "roll_current_62x0mm"), false},
		{"duplex", kwAttr("sides", "two-sided-long-edge"), true},
		{"copies 5", intAttr("copies", 5), true},
		{"copies 100", intAttr("copies", 100), false},
		{"colour", kwAttr("print-color-mode", "color"), false},
		{"draft", goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(3)), true},
		{"300 dpi", goipp.MakeAttribute("printer-resolution", goipp.TagResolution, goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}), true},
		{"300x600 dpi", goipp.MakeAttribute("printer-resolution", goipp.TagResolution, goipp.Resolution{Xres: 300, Yres: 600, Units: goipp.UnitsDpi}), false},
	}
	for _, c := range cases {
		st := status(validateJob(t, ts, &yes, c.attr))
		if (st == goipp.StatusOk) != c.ok {
			t.Errorf("%s: %s", c.name, st)
		}
	}

	resp := printJob(t, ts, formatOctetStream, []byte("%PDF-1.4\n%%EOF\n"), sizeCol(21590, 27940),
		kwAttr("sides", "two-sided-short-edge"), intAttr("copies", 2))
	if st := status(resp); st != goipp.StatusOk {
		t.Fatalf("print pdf: %s", st)
	}
	id := intOf(t, resp.Job, "job-id")
	waitJobState(t, ts, id, JobCompleted)
	rec.mu.Lock()
	job, format := rec.jobs[0], rec.formats[0]
	rec.mu.Unlock()
	if format != "application/pdf" {
		t.Errorf("format %q", format)
	}
	a := job.Attrs
	if a.Resolved.Name != driver.MediaLetter || a.Sides != "two-sided-short-edge" || a.Copies != 2 || a.PrintQuality != 4 {
		t.Errorf("attrs %+v", a)
	}

	if st := status(printJob(t, ts, "application/postscript", []byte("%!PS\n"))); st != goipp.StatusErrorDocumentFormatNotSupported {
		t.Errorf("postscript: %s", st)
	}
	if st := status(printJob(t, ts, formatOctetStream, []byte("%!PS\n"))); st != goipp.StatusErrorDocumentFormatNotSupported {
		t.Errorf("sniffed postscript: %s", st)
	}
}

const documentSmoke = `
{
	NAME "Get-Printer-Attributes"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword requested-attributes all,media-col-database
	STATUS successful-ok
	EXPECT printer-kind OF-TYPE keyword WITH-VALUE "document"
	EXPECT media-col-ready OF-TYPE collection COUNT 2
	EXPECT media-col-database OF-TYPE collection COUNT 4
	EXPECT sides-supported WITH-VALUE "two-sided-long-edge"
	EXPECT document-format-supported WITH-VALUE "image/pwg-raster"
	EXPECT pwg-raster-document-type-supported WITH-VALUE "sgray_8"
	EXPECT urf-supported WITH-VALUE "DM1"
	EXPECT output-bin-default OF-TYPE keyword COUNT 1
	EXPECT output-bin-supported OF-TYPE keyword
	EXPECT pages-per-minute OF-TYPE integer COUNT 1 WITH-VALUE >0
	EXPECT print-content-optimize-default OF-TYPE keyword COUNT 1 WITH-VALUE "auto"
	EXPECT print-rendering-intent-default OF-TYPE keyword COUNT 1 WITH-VALUE "auto"
	EXPECT printer-supply OF-TYPE octetString SAME-COUNT-AS printer-supply-description
	EXPECT printer-supply-description OF-TYPE text SAME-COUNT-AS printer-supply
	EXPECT printer-output-tray OF-TYPE octetString
	EXPECT print-scaling-default OF-TYPE keyword COUNT 1 WITH-VALUE "auto"
}
{
	NAME "Print-Job A4 duplex"
	OPERATION Print-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR mimeMediaType document-format image/urf
	ATTR boolean ipp-attribute-fidelity true
	GROUP job-attributes-tag
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 21000 MEMBER integer y-dimension 29700 } MEMBER integer media-top-margin 423 MEMBER integer media-bottom-margin 423 MEMBER integer media-left-margin 423 MEMBER integer media-right-margin 423 MEMBER keyword media-source main MEMBER keyword media-type stationery }
	ATTR keyword sides two-sided-long-edge
	ATTR integer copies 3
	ATTR keyword print-color-mode monochrome
	FILE $filename
	STATUS successful-ok
	EXPECT job-id
}
`

func TestIPPToolDocumentPrinter(t *testing.T) {
	ipptool := requireIPPTool(t)
	ts := startServer(t, docConfig(t), (&recorder{}).process)
	dir := t.TempDir()
	testFile := filepath.Join(dir, "doc.test")
	urfFile := filepath.Join(dir, "page.urf")
	if err := os.WriteFile(testFile, []byte(documentSmoke), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(urfFile, tinyURF(2480, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"ipp", "ipps"} {
		out, err := exec.Command(ipptool, "-tv", "-T", "30", "-f", urfFile, ts.uri(scheme), testFile).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", scheme, err, out)
		}
	}
}

// colorRule is the colour rule of CUPS's ipp-2.0.test (PWG 5100.12 section 6.2).
const colorRule = `{
	NAME "pages-per-minute-color"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	STATUS successful-ok
	EXPECT color-supported OF-TYPE boolean IN-GROUP printer-attributes-tag COUNT 1 WITH-VALUE true DEFINE-MATCH PRINTER_IS_COLOR
	EXPECT pages-per-minute OF-TYPE integer IN-GROUP printer-attributes-tag COUNT 1
	EXPECT pages-per-minute-color OF-TYPE integer IN-GROUP printer-attributes-tag COUNT 1 IF-DEFINED PRINTER_IS_COLOR
	EXPECT !pages-per-minute-color IF-NOT-DEFINED PRINTER_IS_COLOR
}
`

func TestPagesPerMinuteColor(t *testing.T) {
	for _, color := range []bool{false, true} {
		c := docConfig(t)
		if color {
			c.Caps.Color = true
			c.Caps.URF = []string{"V1.4", "W8", "SRGB24", "DM1", "CP99", "IS1", "MT1", "RS300-600"}
			c.Caps.PWGTypes = []string{"sgray_8", "srgb_8"}
		}
		attrs, err := PrinterAttributes(c)
		if err != nil {
			t.Fatal(err)
		}
		var ppm, ppmColor []string
		for _, a := range attrs {
			for _, v := range a.Values {
				switch a.Name {
				case "pages-per-minute":
					ppm = append(ppm, v.V.String())
				case "pages-per-minute-color":
					ppmColor = append(ppmColor, v.V.String())
				}
			}
		}
		want := []string(nil)
		if color {
			want = []string{"10"}
		}
		if strings.Join(ppm, " ") != "10" || strings.Join(ppmColor, " ") != strings.Join(want, " ") {
			t.Errorf("colour %v: pages-per-minute %v, pages-per-minute-color %v", color, ppm, ppmColor)
		}

		ipptool := requireIPPTool(t)
		testFile := filepath.Join(t.TempDir(), "color.test")
		if err := os.WriteFile(testFile, []byte(colorRule), 0o644); err != nil {
			t.Fatal(err)
		}
		ts := startServer(t, c, (&recorder{}).process)
		out, err := exec.Command(ipptool, "-tv", "-T", "30", ts.uri("ipp"), testFile).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "[PASS]") {
			t.Errorf("colour %v: ipptool: %v\n%s", color, err, out)
		}
	}
}
