package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	imgcolor "image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// rgbA4Page is an A4 sheet at dpi as an iPhone sends it in colour: a few solid colour blocks
// and black bars.
func rgbA4Page(dpi int, landscape bool) testutil.Page {
	w, h := 2480*dpi/300, 3508*dpi/300
	if landscape {
		w, h = h, w
	}
	pix := bytes.Repeat([]byte{0xff}, 3*w*h)
	colors := [][3]byte{{255, 0, 0}, {0, 160, 0}, {0, 0, 255}, {0, 0, 0}}
	for i, y := 0, h/10; y < h*9/10; i, y = i+1, y+h/12 {
		c := colors[i%len(colors)]
		for yy := y; yy < y+h/40; yy++ {
			for x := w / 8; x < w*7/8; x++ {
				copy(pix[3*(yy*w+x):], c[:])
			}
		}
	}
	return testutil.Page{Width: w, Height: h, XDPI: dpi, BPP: 24, ColorSpace: testutil.URFRGB, Pix: pix}
}

func colorPPD(t *testing.T) string {
	t.Helper()
	ppd, err := filepath.Abs("../../internal/driver/cups/testdata/color-deskjet.ppd")
	if err != nil {
		t.Fatal(err)
	}
	return ppd
}

// printDry prints doc on a dry-run printer and returns the device job's bytes.
func printDry(t *testing.T, o options, doc []byte, info driver.JobInfo) []byte {
	t.Helper()
	o.dryRun = t.TempDir()
	p, err := newPrinter(&o, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.send.Close()
	if _, err := p.printDocument(context.Background(), docOf(doc), p.jobInfo(info, t.Logf, "c"), nil); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(o.dryRun, "job-1.bin"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// neutralRGB is p, a gray page, sent as 24-bit sRGB.
func neutralRGB(p testutil.Page) testutil.Page {
	pix := make([]byte, 0, 3*len(p.Pix))
	for _, v := range p.Pix {
		pix = append(pix, v, v, v)
	}
	p.BPP, p.ColorSpace, p.Pix = 24, testutil.URFRGB, pix
	return p
}

// On a colour-enabled printer, a gray document (also one sent as sRGB) and any document sent in
// monochrome print exactly as on the same printer without colour; a colour document in auto or
// color mode prints in colour.
func TestColorPrinterKeepsGrayJobs(t *testing.T) {
	gray := testutil.EncodeURF(a4Page(300), a4Page(300))
	neutral := testutil.EncodeURF(neutralRGB(a4Page(300)), a4Page(300))
	color := testutil.EncodeURF(rgbA4Page(300, false), a4Page(300))
	cases := []struct {
		name   string
		mod    func(*options)
		marker string // in a colour job only
	}{
		{"pcl", func(o *options) { o.driver = "pcl" }, "\x1b*v6W"},
		{"postscript", func(o *options) { o.driver = "postscript" }, "aubrgb"},
		{"cups", func(o *options) { o.driver, o.ppd = "cups", colorPPD(t); o.driverOpts = []string{"filter=-"} }, "\x08\x00\x00\x00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, on := defaultOptions(t), defaultOptions(t)
			c.mod(&base)
			c.mod(&on)
			base.color, on.color = "off", "on"
			if c.name == "cups" {
				on.color = "auto"
			}
			want := printDry(t, base, gray, driver.JobInfo{Name: "g"})
			if got := printDry(t, base, neutral, driver.JobInfo{Name: "g"}); !bytes.Equal(got, want) {
				t.Fatal("gray printer: the sRGB gray document differs from the gray one")
			}
			for _, mode := range []string{"", "auto", "color", "monochrome", "auto-monochrome"} {
				if got := printDry(t, on, gray, driver.JobInfo{Name: "g", ColorMode: mode}); !bytes.Equal(got, want) {
					t.Errorf("gray document, mode %q: %d bytes, gray printer %d", mode, len(got), len(want))
				}
				if got := printDry(t, on, neutral, driver.JobInfo{Name: "g", ColorMode: mode}); !bytes.Equal(got, want) {
					t.Errorf("sRGB gray document, mode %q: %d bytes, gray printer %d", mode, len(got), len(want))
				}
			}
			wantMono := printDry(t, base, color, driver.JobInfo{Name: "c"})
			for _, mode := range []string{"monochrome", "auto-monochrome"} {
				if got := printDry(t, on, color, driver.JobInfo{Name: "c", ColorMode: mode}); !bytes.Equal(got, wantMono) {
					t.Errorf("colour document, mode %s: differs from the gray printer", mode)
				}
			}
			for _, mode := range []string{"", "auto", "color"} {
				got := printDry(t, on, color, driver.JobInfo{Name: "c", ColorMode: mode})
				if bytes.Equal(got, wantMono) {
					t.Errorf("colour document, mode %q: printed gray", mode)
				}
				if c.name != "cups" && !bytes.Contains(got, []byte(c.marker)) {
					t.Errorf("colour document, mode %q: no %q", mode, c.marker)
				}
			}
		})
	}
}

// A colour job that fits as gray but not in colour (a page over the decode budget as RGB, or a
// sheet over the job's budget) prints exactly what the printer without colour prints, streamed
// or rendered whole, instead of failing.
func TestColorTooLargePrintsGray(t *testing.T) {
	doc := testutil.EncodeURF(rgbA4Page(300, false), a4Page(300))
	grayPage := 2480 * 3508
	drivers := []struct {
		name string
		mod  func(*options)
	}{
		{"pcl", func(o *options) { o.driver = "pcl" }},
		{"postscript", func(o *options) { o.driver = "postscript" }},
		{"cups", func(o *options) { o.driver, o.ppd = "cups", colorPPD(t); o.driverOpts = []string{"filter=-"} }},
	}
	run := func(t *testing.T, mod func(*options), color string, stream bool, maxGray int, sheet int64) ([]byte, []string) {
		o := defaultOptions(t)
		mod(&o)
		o.color, o.dryRun = color, t.TempDir()
		var logs []string
		logf := func(f string, args ...any) { logs = append(logs, fmt.Sprintf(f, args...)) }
		p, err := newPrinter(&o, logf)
		if err != nil {
			t.Fatal(err)
		}
		defer p.send.Close()
		if !stream {
			p.send = plainSender{p.send}
		}
		p.maxGray = maxGray
		info := driver.JobInfo{Name: "big", ColorMode: "color", Budget: driver.NewBudget(sheet)}
		if _, err := p.printDocument(context.Background(), docOf(doc), p.jobInfo(info, logf, "big"), nil); err != nil {
			t.Fatalf("%v\n%s", err, strings.Join(logs, "\n"))
		}
		out, err := os.ReadFile(filepath.Join(o.dryRun, "job-1.bin"))
		if err != nil {
			t.Fatal(err)
		}
		return out, logs
	}
	for _, d := range drivers {
		for _, limit := range []string{"decode", "sheet"} {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", d.name, limit, stream), func(t *testing.T) {
					maxGray, sheet := maxGrayBytes, int64(driver.MaxSheetBytes)
					if limit == "decode" {
						maxGray = 2*grayPage + 4096 // both pages as gray, not one as RGB
					} else {
						sheet = int64(grayPage) * 3 / 2
					}
					want, _ := run(t, d.mod, "off", stream, maxGray, sheet)
					got, logs := run(t, d.mod, "on", stream, maxGray, sheet)
					if !bytes.Equal(got, want) {
						t.Errorf("%d bytes, the gray printer's job is %d", len(got), len(want))
					}
					if !strings.Contains(strings.Join(logs, "\n"), "printing in gray: not possible in colour") {
						t.Errorf("no fallback logged:\n%s", strings.Join(logs, "\n"))
					}
					// With room for colour, the same job is in colour.
					if limit == "decode" {
						maxGray = maxGrayBytes
					} else {
						sheet = driver.MaxSheetBytes
					}
					if color, _ := run(t, d.mod, "on", stream, maxGray, sheet); bytes.Equal(color, want) {
						t.Error("printed gray with room for colour")
					}
				})
			}
		}
	}
}

func TestColorFlag(t *testing.T) {
	o := defaultOptions(t)
	if o.color != "auto" {
		t.Errorf("default --color %q", o.color)
	}
	o.color, o.dryRun = "sometimes", t.TempDir()
	if _, err := newPrinter(&o, t.Logf); err == nil {
		t.Error("--color sometimes accepted")
	}
	// Label drivers print black only, whatever --color says.
	o.color, o.driver = "on", "brother-ql"
	p, err := newPrinter(&o, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.send.Close()
	if p.caps.Color {
		t.Error("brother-ql in colour")
	}
}

// colorIPPTest is what an iPhone does with a colour printer: read the colour capabilities, then
// Print-Job an SRGB24 URF in color (and the same job in monochrome).
const colorIPPTest = `{
	NAME "Colour capabilities"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword requested-attributes all,media-col-database
	STATUS successful-ok
	EXPECT color-supported OF-TYPE boolean WITH-VALUE true
	EXPECT print-color-mode-supported OF-TYPE keyword WITH-VALUE "color"
	EXPECT print-color-mode-supported WITH-VALUE "monochrome"
	EXPECT print-color-mode-default OF-TYPE keyword WITH-VALUE "auto"
	EXPECT urf-supported WITH-VALUE "SRGB24"
	EXPECT urf-supported WITH-VALUE "W8"
	EXPECT pwg-raster-document-type-supported WITH-VALUE "srgb_8"
	EXPECT pwg-raster-document-type-supported WITH-VALUE "sgray_8"
}
{
	NAME "Print-Job SRGB24 URF in color"
	OPERATION Print-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR name job-name "colour page"
	ATTR mimeMediaType document-format image/urf
	GROUP job-attributes-tag
	ATTR keyword print-color-mode color
	ATTR enum print-quality 4
	ATTR keyword sides one-sided
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 21000 MEMBER integer y-dimension 29700 } }
	FILE $filename
	STATUS successful-ok
}
{
	NAME "Print-Job SRGB24 URF in monochrome"
	OPERATION Print-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR mimeMediaType document-format image/urf
	GROUP job-attributes-tag
	ATTR keyword print-color-mode monochrome
	FILE $filename
	STATUS successful-ok
}
`

// TestIPPToolColorEveryDriver drives a dry-run instance of each colour driver with ipptool as
// an iPhone would: colour capabilities, TXT Color=T, and an SRGB24 URF printed in colour and in
// monochrome.
func TestIPPToolColorEveryDriver(t *testing.T) {
	ipptool, err := exec.LookPath("ipptool")
	if err != nil {
		t.Skip("ipptool not on PATH")
	}
	cases := []struct {
		name   string
		mod    func(*options)
		marker string
	}{
		{"pcl", func(o *options) { o.driver, o.color = "pcl", "on" }, "\x1b*v6W\x00\x03\x00\x08\x08\x08"},
		{"postscript", func(o *options) { o.driver, o.color = "postscript", "on" }, "/DeviceRGB setcolorspace"},
		{"cups", func(o *options) { o.driver, o.ppd = "cups", colorPPD(t) }, "\x1b*r-4U"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "cups" && findCUPSFilter("rastertohp") == "" {
				t.Skip("CUPS filter rastertohp not installed")
			}
			ts := startService(t, c.mod)
			if txt := ts.cfg.TXT("printer.local", 8631); txt["Color"] != "T" || !strings.Contains(txt["URF"], "SRGB24") {
				t.Errorf("TXT Color=%q URF=%q", txt["Color"], txt["URF"])
			}
			dir := t.TempDir()
			urf := filepath.Join(dir, "colour.urf")
			test := filepath.Join(dir, "colour.test")
			if err := os.WriteFile(urf, testutil.EncodeURF(rgbA4Page(300, false)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(test, []byte(colorIPPTest), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(ipptool, "-tv", "-T", "30", "-f", urf, "ipp://"+ts.addr+"/ipp/print", test).CombinedOutput()
			if err != nil || strings.Count(string(out), "[PASS]") != 3 {
				t.Fatalf("%v\n%s", err, out)
			}
			for id := 1; id <= 2; id++ {
				if st := attrInt(t, ts.waitJob(t, "ipp", id), "job-state"); st != 9 {
					t.Fatalf("job %d: job-state %d", id, st)
				}
			}
			color, err := os.ReadFile(filepath.Join(ts.dryDir, "job-1.bin"))
			if err != nil {
				t.Fatal(err)
			}
			mono, err := os.ReadFile(filepath.Join(ts.dryDir, "job-2.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(color, []byte(c.marker)) || bytes.Contains(mono, []byte(c.marker)) {
				t.Errorf("colour job has %q: %v; monochrome job has it: %v", c.marker,
					bytes.Contains(color, []byte(c.marker)), bytes.Contains(mono, []byte(c.marker)))
			}
		})
	}
}

// plainSender hides SendStream, so the bridge renders the whole job before sending it.
type plainSender struct{ driver.Sender }

// Without streaming, a gray document on a colour printer is still the gray job, and a colour
// document prints in colour.
func TestColorRenderPath(t *testing.T) {
	gray := testutil.EncodeURF(a4Page(300))
	color := testutil.EncodeURF(rgbA4Page(300, false))
	run := func(colorFlag string, doc []byte) []byte {
		o := defaultOptions(t)
		o.driver, o.color, o.dryRun = "pcl", colorFlag, t.TempDir()
		p, err := newPrinter(&o, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		defer p.send.Close()
		p.send = plainSender{p.send}
		if p.streams() {
			t.Fatal("still streaming")
		}
		if _, err := p.printDocument(context.Background(), docOf(doc), p.jobInfo(driver.JobInfo{Name: "r"}, t.Logf, "r"), nil); err != nil {
			t.Fatal(err)
		}
		out, err := os.ReadFile(filepath.Join(o.dryRun, "job-1.bin"))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !bytes.Equal(run("on", gray), run("off", gray)) {
		t.Error("gray document prints differently on the colour printer")
	}
	if out := run("on", color); !bytes.Contains(out, []byte("\x1b*v6W")) {
		t.Error("colour document not in colour")
	}
}

// print FILE.png on a colour printer: a gray PNG, or an RGB one with only gray pixels, is the
// gray job; an RGB one with colour prints in colour.
func TestPrintPNGColor(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, img image.Image) string {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	g := image.NewGray(image.Rect(0, 0, 300, 200))
	n := image.NewRGBA(g.Rect)
	c := image.NewRGBA(g.Rect)
	for y := 0; y < 200; y++ {
		for x := 0; x < 300; x++ {
			v := uint8(x)
			g.SetGray(x, y, imgcolor.Gray{Y: v})
			n.Set(x, y, imgcolor.RGBA{v, v, v, 255})
			c.Set(x, y, imgcolor.RGBA{v, v, v, 255})
			if y > 100 {
				c.Set(x, y, imgcolor.RGBA{255, 0, 0, 255})
			}
		}
	}
	gray, neutral, colour := write("gray.png", g), write("neutral.png", n), write("colour.png", c)
	print := func(file string, args ...string) []byte {
		dry := t.TempDir()
		args = append([]string{"--dry-run", dry, "--state-dir", t.TempDir(), "--mdns", "off"}, args...)
		if _, errOut, code := runCLI(t, append(args, "print", file)...); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		out, err := os.ReadFile(filepath.Join(dry, "job-1.bin"))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, drv := range [][]string{{"--driver", "pcl"}, {"--driver", "cups", "--ppd", colorPPD(t), "--driver-opt", "filter=-"}} {
		off, on := append([]string{"--color", "off"}, drv...), append([]string{"--color", "on"}, drv...)
		for _, f := range []string{gray, neutral} {
			if !bytes.Equal(print(f, on...), print(f, off...)) {
				t.Errorf("%v: %s differs from the gray printer's job", drv, filepath.Base(f))
			}
		}
		if bytes.Equal(print(colour, on...), print(colour, off...)) {
			t.Errorf("%v: colour PNG printed gray", drv)
		}
	}
}
