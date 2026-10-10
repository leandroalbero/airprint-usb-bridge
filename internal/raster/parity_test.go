package raster

import (
	"bufio"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

const referenceScript = "../../testdata/private/reference/job.py"

func baseOptions() Options {
	return Options{Pins: 696, WidthMin: 724, WidthMax: 740, TrimMax: 24, Dither: "auto"}
}

type pyResult struct {
	failed   bool
	log      string
	labels   map[int]*ql.Bitmap
	phase    map[int]string
	dithered map[int]bool
	dropped  map[int]int
	highRes  map[int]bool
}

var (
	rePage    = regexp.MustCompile(`^INFO: page (\d+)[: ]`)
	rePhase   = regexp.MustCompile(`\((300x\d+)[^,]*, phase (col=\d row=\w+)\)`)
	reDither  = regexp.MustCompile(`: dithered`)
	reDropped = regexp.MustCompile(`^INFO: (\d+) black dots outside`)
)

func pythonReference(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(referenceScript); err != nil {
		t.Skipf("reference script %s not present", referenceScript)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	return py
}

func runPython(t *testing.T, py string, data []byte, ext string, o Options) pyResult {
	t.Helper()
	dir := t.TempDir()
	debug := filepath.Join(dir, "debug")
	dry := filepath.Join(dir, "dry")
	tmp := filepath.Join(dir, "tmp")
	for _, d := range []string{debug, dry, tmp} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := filepath.Join(dir, "inv")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "job"+ext)
	if err := os.WriteFile(in, data, 0o644); err != nil {
		t.Fatal(err)
	}
	trim := o.TrimMax
	if trim < 0 {
		trim = 1 << 30
	}
	dither := o.Dither
	if dither == "" {
		dither = "auto"
	}
	quality := "normal"
	if o.HighRes {
		quality = "high"
	}
	script, err := filepath.Abs(referenceScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-I", script, in)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + tmp,
		"QL_INV=" + stub,
		"QL_DEBUG_DIR=" + debug,
		"QL_DRY_RUN_DIR=" + dry,
		"QL_BUSY_DEADLINE=0",
		"IPP_JOB_ID=1",
		"IPP_PRINT_QUALITY=" + quality,
		"QL_TRIM_MAX=" + strconv.Itoa(trim),
		"QL_DITHER=" + dither,
		"QL_PINS=" + strconv.Itoa(o.Pins),
		"QL_WIDTH_MIN=" + strconv.Itoa(o.WidthMin),
		"QL_WIDTH_MAX=" + strconv.Itoa(o.WidthMax),
	}
	out, err := cmd.CombinedOutput()
	r := pyResult{
		log:      string(out),
		labels:   map[int]*ql.Bitmap{},
		phase:    map[int]string{},
		dithered: map[int]bool{},
		dropped:  map[int]int{},
		highRes:  map[int]bool{},
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("python: %v", err)
		}
		r.failed = true
	}
	page := 0
	sc := bufio.NewScanner(strings.NewReader(r.log))
	for sc.Scan() {
		line := sc.Text()
		if m := rePage.FindStringSubmatch(line); m != nil {
			page, _ = strconv.Atoi(m[1])
		}
		if m := rePhase.FindStringSubmatch(line); m != nil {
			r.phase[page] = m[2]
			r.highRes[page] = m[1] == "300x600"
		}
		if reDither.MatchString(line) {
			r.dithered[page] = true
		}
		if m := reDropped.FindStringSubmatch(line); m != nil {
			r.dropped[page], _ = strconv.Atoi(m[1])
		}
		if strings.HasPrefix(line, "Traceback") {
			t.Fatalf("python crashed:\n%s", r.log)
		}
	}
	pngs, _ := filepath.Glob(filepath.Join(debug, "job1-p*-label.png"))
	for _, f := range pngs {
		var idx int
		if _, err := fmt.Sscanf(filepath.Base(f), "job1-p%d-label.png", &idx); err != nil {
			t.Fatal(err)
		}
		r.labels[idx] = readLabelPNG(t, f)
	}
	return r
}

func readLabelPNG(t *testing.T, path string) *ql.Bitmap {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	bm := ql.NewBitmap(b.Dx(), b.Dy())
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, _, _, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			bm.Bits[y*bm.W+x] = r < 0x8000
		}
	}
	return bm
}

type goResult struct {
	err    error
	labels []Label
}

func runGo(data []byte, o Options) goResult {
	pages, err := Decode(data)
	if err != nil {
		return goResult{err: err}
	}
	var r goResult
	for i, p := range pages {
		l, err := Prepare(p, o)
		if err != nil {
			return goResult{err: fmt.Errorf("page %d: %w", i+1, err)}
		}
		r.labels = append(r.labels, l)
	}
	return r
}

func diffBitmaps(a, b *ql.Bitmap) string {
	if a.W != b.W || a.H != b.H {
		return fmt.Sprintf("size %dx%d vs %dx%d", a.W, a.H, b.W, b.H)
	}
	n, first := 0, -1
	for i := range a.Bits {
		if a.Bits[i] != b.Bits[i] {
			if first < 0 {
				first = i
			}
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d dots differ, first at x=%d y=%d", n, first%a.W, first/a.W)
}

// compareWithPython asserts dot-identical labels plus the same dither, phase,
// high-res and dropped-dot decisions, and returns both results for extra checks.
func compareWithPython(t *testing.T, py string, data []byte, ext string, o Options) (goResult, pyResult) {
	t.Helper()
	g := runGo(data, o)
	p := runPython(t, py, data, ext, o)
	if (g.err != nil) != p.failed {
		t.Fatalf("go err=%v, python failed=%v\npython log:\n%s", g.err, p.failed, p.log)
	}
	if g.err != nil {
		return g, p
	}
	for i, l := range g.labels {
		idx := i + 1
		want, ok := p.labels[idx]
		if l.Blank {
			if ok {
				t.Errorf("page %d: go says blank, python wrote a label", idx)
			}
			continue
		}
		if !ok {
			t.Errorf("page %d: python wrote no label\n%s", idx, p.log)
			continue
		}
		if d := diffBitmaps(l.Bitmap, want); d != "" {
			t.Errorf("page %d: bitmaps differ: %s", idx, d)
		}
		if l.Dithered != p.dithered[idx] {
			t.Errorf("page %d: dithered go=%v python=%v", idx, l.Dithered, p.dithered[idx])
		}
		if l.Dropped != p.dropped[idx] {
			t.Errorf("page %d: dropped go=%d python=%d", idx, l.Dropped, p.dropped[idx])
		}
		if l.HighRes != p.highRes[idx] {
			t.Errorf("page %d: high-res go=%v python=%v", idx, l.HighRes, p.highRes[idx])
		}
	}
	if len(p.labels) > len(g.labels) {
		t.Errorf("python wrote %d labels for %d pages", len(p.labels), len(g.labels))
	}
	return g, p
}

func TestPythonParity(t *testing.T) {
	py := pythonReference(t)

	gray := func(w, h, dpi int, g []byte) testutil.Page { return testutil.GrayPage(w, h, dpi, g) }
	text300 := gray(732, 260, 300, textGray(11, 732, 260, 40, 1, 0, true))
	odd600 := gray(1464, 520, 600, textGray(12, 1464, 520, 80, 2, 1, false))
	even600 := gray(1464, 520, 600, textGray(13, 1464, 520, 80, 2, 0, false))
	oddW600 := gray(1461, 300, 600, textGray(14, 1461, 300, 60, 2, 1, false))
	evenOddW600 := gray(1461, 300, 600, textGray(15, 1461, 300, 60, 2, 0, false))
	photo := gray(732, 200, 300, gradientGray(16, 732, 200))
	photo600 := gray(1464, 240, 600, gradientGray(17, 1464, 240))
	blank := gray(732, 120, 300, whiteGray(732, 120))
	textG := textGray(18, 732, 200, 30, 1, 0, true)
	photoG := gradientGray(19, 732, 160)

	dropped := whiteGray(740, 100)
	fillRect(dropped, 740, 0, 10, 4, 50, 0)
	fillRect(dropped, 740, 735, 30, 5, 50, 0)
	fillRect(dropped, 740, 300, 40, 100, 20, 0)

	narrow := func(w int) testutil.Page {
		g := whiteGray(w, 40)
		fillRect(g, w, 0, 5, 3, 30, 0)
		fillRect(g, w, w-3, 5, 3, 30, 0)
		return gray(w, 40, 300, g)
	}
	narrowOpts := baseOptions()
	narrowOpts.WidthMin = 600

	opts := func(f func(*Options)) Options {
		o := baseOptions()
		f(&o)
		return o
	}
	unlimited := opts(func(o *Options) { o.TrimMax = -1 })
	high := opts(func(o *Options) { o.HighRes = true })

	type check func(t *testing.T, g goResult, p pyResult)
	phase := func(want string) check {
		return func(t *testing.T, g goResult, p pyResult) {
			if p.phase[1] != want {
				t.Errorf("python phase %q, want %q", p.phase[1], want)
			}
		}
	}
	dithered := func(want ...bool) check {
		return func(t *testing.T, g goResult, p pyResult) {
			for i, w := range want {
				if g.labels[i].Dithered != w {
					t.Errorf("page %d dithered=%v, want %v", i+1, g.labels[i].Dithered, w)
				}
			}
		}
	}
	blankPages := func(want ...bool) check {
		return func(t *testing.T, g goResult, p pyResult) {
			if len(g.labels) != len(want) {
				t.Fatalf("%d labels, want %d", len(g.labels), len(want))
			}
			for i, w := range want {
				if g.labels[i].Blank != w {
					t.Errorf("page %d blank=%v, want %v", i+1, g.labels[i].Blank, w)
				}
			}
		}
	}
	rows := func(want int) check {
		return func(t *testing.T, g goResult, p pyResult) {
			if g.labels[0].Bitmap.H != want {
				t.Errorf("label is %d rows, want %d", g.labels[0].Bitmap.H, want)
			}
		}
	}
	someDropped := func(t *testing.T, g goResult, p pyResult) {
		if g.labels[0].Dropped == 0 {
			t.Error("expected dropped dots")
		}
	}

	urf, pwg := ".urf", ".pwg"
	cases := []struct {
		name   string
		data   []byte
		ext    string
		o      Options
		checks []check
	}{
		{"300dpi text", testutil.EncodeURF(text300), urf, baseOptions(), []check{dithered(false)}},
		{"300dpi text pwg", testutil.EncodePWG(text300), pwg, baseOptions(), nil},
		{"300dpi text trim unlimited", testutil.EncodeURF(text300), urf, unlimited, nil},
		{"300dpi text trim 0", testutil.EncodeURF(text300), urf, opts(func(o *Options) { o.TrimMax = 0 }), []check{rows(260)}},
		{"300dpi text dither on", testutil.EncodeURF(text300), urf, opts(func(o *Options) { o.Dither = "on" }), []check{dithered(true)}},
		{"600dpi odd offsets", testutil.EncodeURF(odd600), urf, baseOptions(), []check{phase("col=1 row=1")}},
		{"600dpi even offsets", testutil.EncodeURF(even600), urf, baseOptions(), []check{phase("col=0 row=0")}},
		{"600dpi odd offsets pwg", testutil.EncodePWG(odd600), pwg, unlimited, []check{phase("col=1 row=1")}},
		{"600dpi odd width odd offsets", testutil.EncodeURF(oddW600), urf, baseOptions(), []check{phase("col=1 row=1")}},
		{"600dpi odd width even offsets", testutil.EncodeURF(evenOddW600), urf, baseOptions(), []check{phase("col=0 row=0")}},
		{"high-res 600", testutil.EncodeURF(odd600), urf, high, []check{phase("col=1 row=None")}},
		{"high-res 600 even", testutil.EncodeURF(even600), urf, high, []check{phase("col=0 row=None")}},
		{"high-res 600 trim unlimited", testutil.EncodeURF(odd600), urf, opts(func(o *Options) { o.HighRes = true; o.TrimMax = -1 }), nil},
		{"high-res on 300dpi page", testutil.EncodeURF(text300), urf, high, nil},
		{"photo auto dither", testutil.EncodeURF(photo), urf, baseOptions(), []check{dithered(true)}},
		{"photo dither off", testutil.EncodeURF(photo), urf, opts(func(o *Options) { o.Dither = "off" }), []check{dithered(false)}},
		{"photo 600 auto dither", testutil.EncodeURF(photo600), urf, baseOptions(), []check{dithered(true)}},
		{"photo 600 high-res dither", testutil.EncodeURF(photo600), urf, high, []check{dithered(true)}},
		{"urf cmyk32", testutil.EncodeURF(colourPage(20, 732, 200, 300, 32, testutil.URFCMYK, true, textG)), urf, baseOptions(), nil},
		{"urf cmyk32 photo", testutil.EncodeURF(colourPage(21, 732, 160, 300, 32, testutil.URFCMYK, true, photoG)), urf, baseOptions(), []check{dithered(true)}},
		{"pwg black8", testutil.EncodePWG(testutil.Page{Width: 732, Height: 200, XDPI: 300, BPP: 8, ColorSpace: testutil.PWGBlack, Pix: invert(textG)}), pwg, baseOptions(), nil},
		{"pwg cmyk32", testutil.EncodePWG(colourPage(22, 732, 160, 300, 32, testutil.PWGCMYK, true, photoG)), pwg, baseOptions(), nil},
		{"urf rgb24", testutil.EncodeURF(colourPage(23, 732, 200, 300, 24, testutil.URFRGB, false, textG)), urf, baseOptions(), nil},
		{"urf rgb24 photo", testutil.EncodeURF(colourPage(24, 732, 160, 300, 24, testutil.URFRGB, false, photoG)), urf, baseOptions(), []check{dithered(true)}},
		{"urf rgb32", testutil.EncodeURF(colourPage(25, 732, 160, 300, 32, testutil.URFRGB, false, photoG)), urf, baseOptions(), nil},
		{"pwg rgb24 600", testutil.EncodePWG(colourPage(26, 1464, 520, 600, 24, testutil.PWGRGB, false, odd600.Pix)), pwg, baseOptions(), nil},
		{"multi-page", testutil.EncodeURF(text300, blank, photo, odd600), urf, baseOptions(), []check{blankPages(false, true, false, false), dithered(false, false, true, false)}},
		{"multi-page pwg", testutil.EncodePWG(photo, text300, blank), pwg, unlimited, []check{blankPages(false, false, true)}},
		{"multi-page unknown count", testutil.EncodeURFCount(0, text300, photo), urf, baseOptions(), []check{blankPages(false, false)}},
		{"blank page", testutil.EncodeURF(blank), urf, baseOptions(), []check{blankPages(true)}},
		{"blank page trim unlimited", testutil.EncodeURF(blank), urf, unlimited, []check{blankPages(true)}},
		{"blank 600", testutil.EncodeURF(gray(1464, 100, 600, whiteGray(1464, 100))), urf, baseOptions(), []check{blankPages(true)}},
		{"wrong width 300", testutil.EncodeURF(gray(696, 50, 300, textGray(27, 696, 50, 5, 1, 0, false))), urf, baseOptions(), nil},
		{"wrong width 600", testutil.EncodeURF(gray(1200, 50, 600, textGray(28, 1200, 50, 5, 1, 0, false))), urf, baseOptions(), nil},
		{"wrong width second page", testutil.EncodeURF(text300, gray(800, 50, 300, whiteGray(800, 50))), urf, baseOptions(), nil},
		{"width 724", testutil.EncodeURF(gray(724, 80, 300, textGray(29, 724, 80, 5, 1, 0, true))), urf, baseOptions(), nil},
		{"width 740 dropped dots", testutil.EncodeURF(gray(740, 100, 300, dropped)), urf, baseOptions(), []check{someDropped}},
		{"bad dpi", testutil.EncodeURF(gray(732, 20, 200, textGray(30, 732, 20, 2, 1, 0, false))), urf, baseOptions(), nil},
		{"narrow 691 pads", testutil.EncodeURF(narrow(691)), urf, narrowOpts, nil},
		{"narrow 690 pads", testutil.EncodeURF(narrow(690)), urf, narrowOpts, nil},
		{"narrow 701 crops", testutil.EncodeURF(narrow(701)), urf, narrowOpts, nil},
	}
	wantErr := map[string]bool{"wrong width 300": true, "wrong width 600": true, "wrong width second page": true, "bad dpi": true}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g, p := compareWithPython(t, py, c.data, c.ext, c.o)
			if wantErr[c.name] != (g.err != nil) {
				t.Fatalf("err=%v, want error=%v\n%s", g.err, wantErr[c.name], p.log)
			}
			for _, ch := range c.checks {
				ch(t, g, p)
			}
		})
	}
}

func TestPythonParityTrimMax(t *testing.T) {
	py := pythonReference(t)
	g := whiteGray(732, 300)
	fillRect(g, 732, 200, 100, 300, 100, 0)
	page := testutil.GrayPage(732, 300, 300, g)
	data := testutil.EncodeURF(page)
	capped, _ := compareWithPython(t, py, data, ".urf", baseOptions())
	o := baseOptions()
	o.TrimMax = -1
	free, _ := compareWithPython(t, py, data, ".urf", o)
	if capped.labels[0].Bitmap.H != 300-2*24 || free.labels[0].Bitmap.H != 100 {
		t.Fatalf("capped %d rows, unlimited %d rows", capped.labels[0].Bitmap.H, free.labels[0].Bitmap.H)
	}
}

func TestPythonParityPrivateFixtures(t *testing.T) {
	py := pythonReference(t)
	files, _ := filepath.Glob("../../testdata/private/*.urf")
	if len(files) == 0 {
		t.Skip("no private URF captures")
	}
	for i, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, trim := range []int{24, -1} {
			t.Run(fmt.Sprintf("capture%d/trim%d", i, trim), func(t *testing.T) {
				t.Parallel()
				o := baseOptions()
				o.TrimMax = trim
				g, _ := compareWithPython(t, py, data, ".urf", o)
				if g.err != nil {
					t.Fatal(g.err)
				}
			})
		}
	}
}
