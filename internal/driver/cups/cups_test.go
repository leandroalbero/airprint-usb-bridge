package cups

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// TestMain doubles as a fake CUPS filter: the driver runs this test binary with the
// environment a filter gets, and the TestMode PPD option picks its behaviour.
func TestMain(m *testing.M) {
	if os.Getenv("CONTENT_TYPE") == "application/vnd.cups-raster" && strings.HasSuffix(os.Getenv("PPD"), "fake.ppd") {
		os.Exit(fakeFilter())
	}
	os.Exit(m.Run())
}

func fakeFilter() int {
	mode := ""
	if len(os.Args) == 6 {
		for _, kv := range strings.Fields(os.Args[5]) {
			if v, ok := strings.CutPrefix(kv, "TestMode="); ok {
				mode = v
			}
		}
	}
	switch mode {
	case "fail":
		io.Copy(io.Discard, os.Stdin)
		fmt.Fprint(os.Stderr, "DEBUG: noise\nERROR: Printer jammed\nSTATE: +media-jam-error\nWARNING: tray low")
		return 1
	case "hang":
		time.Sleep(time.Minute)
		return 0
	case "flood":
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for i := 0; i < 1000; i++ {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return 1
			}
		}
		return 0
	case "silent":
		return 0
	case "trailer":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 2
		}
		time.Sleep(300 * time.Millisecond)
		os.Stdout.Write([]byte("JOB-END"))
		return 0
	case "trickle":
		io.Copy(io.Discard, os.Stdin)
		for i := 0; i < 10; i++ {
			os.Stdout.Write([]byte("x"))
			time.Sleep(200 * time.Millisecond)
		}
		return 0
	}
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 2
	}
	var env []string
	for _, k := range []string{"PPD", "CHARSET", "CONTENT_TYPE", "FINAL_CONTENT_TYPE", "DEVICE_URI", "PRINTER", "PATH", "HOME"} {
		env = append(env, k+"="+os.Getenv(k))
	}
	fmt.Fprintf(os.Stderr, "NOTICE: got %d bytes\nDEBUG: ignored\nERROR: logged, but the exit status decides\n", len(in))
	fmt.Fprintf(os.Stdout, "argv=%q\nenv=%q\n", os.Args, env)
	os.Stdout.Write(in)
	return 0
}

// fakePPD writes a PPD naming this test binary as its raster filter.
func fakePPD(t *testing.T, extra string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("testdata/laser-pcl.ppd")
	if err != nil {
		t.Fatal(err)
	}
	ppd := strings.Replace(string(src), `*cupsFilter: "application/vnd.cups-raster 50 rastertohp"`,
		`*cupsFilter2: "application/vnd.cups-raster application/vnd.test 0 `+exe+`"`, 1)
	ppd += `*OpenUI *TestMode/Test Mode: PickOne
*OrderDependency: 30 AnySetup *TestMode
*DefaultTestMode: echo
*TestMode echo/Echo: ""
*TestMode fail/Fail: ""
*TestMode hang/Hang: ""
*TestMode flood/Flood: ""
*TestMode silent/Silent: ""
*TestMode trickle/Trickle: ""
*TestMode trailer/Trailer: ""
*CloseUI: *TestMode
` + extra
	path := filepath.Join(t.TempDir(), "fake.ppd")
	if err := os.WriteFile(path, []byte(ppd), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newDriver(t *testing.T, cfg driver.Config) *Driver {
	t.Helper()
	cfg.Logf = t.Logf
	d, err := driver.New("cups", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Driver)
}

// splitEcho separates the fake filter's argv/env report from the raster it echoed.
func splitEcho(t *testing.T, out []byte) (argv, env string, ras []byte) {
	t.Helper()
	a, rest, ok1 := bytes.Cut(out, []byte("\n"))
	e, ras, ok2 := bytes.Cut(rest, []byte("\n"))
	if !ok1 || !ok2 {
		t.Fatalf("fake filter output %q", out[:min(len(out), 200)])
	}
	return string(a), string(e), ras
}

func TestDriverFakeFilter(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: fakePPD(t, ""), Options: map[string]string{"options": "InputSlot=Manual"}})
	caps := d.Capabilities()
	job := drivertest.Job(t, caps)
	job.ID, job.User, job.Name = 42, "al\x00ice\n", "My\tReport"
	pages := []raster.Page{
		drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 100, Y0: 100, X1: 400, Y1: 200}),
		drivertest.SheetPage(job.Media, 150, 0, drivertest.Rect{X0: 0, Y0: 0, X1: 1240, Y1: 1754}),
	}
	res := drivertest.Run(t, d, job, pages...)
	if len(res.Units) != 1 || res.Units[0].Impressions != 2 {
		t.Fatalf("units %+v", res.Units)
	}
	if pages[0].Gray != nil {
		t.Error("page gray not released")
	}
	argv, env, ras := splitEcho(t, res.Data)
	wantArgv := fmt.Sprintf("argv=%q", []string{printerName, "42", "alice", "MyReport", "1",
		"Duplex=None InputSlot=Manual PageRegion=A4 PageSize=A4 Resolution=300dpi"})
	if argv != wantArgv {
		t.Errorf("argv\n got %s\nwant %s", argv, wantArgv)
	}
	for _, want := range []string{"PPD=" + d.ppdPath, "CHARSET=utf-8", "CONTENT_TYPE=application/vnd.cups-raster",
		"FINAL_CONTENT_TYPE=application/vnd.test", "DEVICE_URI=file:///dev/null", "PRINTER=" + printerName, `"HOME="`} {
		if !strings.Contains(env, want) {
			t.Errorf("env %s lacks %s", env, want)
		}
	}

	// The raster the filter got, checked at the header offsets measured from cups/raster.h.
	if string(ras[:4]) != "3SaR" {
		t.Fatalf("sync %q", ras[:4])
	}
	le := binary.LittleEndian
	h1 := ras[4:]
	u := func(off int) uint32 { return le.Uint32(h1[off:]) }
	// A4 is 595x842 pt; the imageable area 12 12 583 830 at 300 dpi is 2379x3408 pixels.
	for _, c := range []struct {
		name      string
		got, want uint32
	}{
		{"HWResolution x", u(276), 300}, {"HWResolution y", u(280), 300},
		{"ImagingBoundingBox left", u(284), 12}, {"bottom", u(288), 12}, {"right", u(292), 583}, {"top", u(296), 830},
		{"Margins left", u(312), 12}, {"Margins bottom", u(316), 12},
		{"MediaPosition", u(324), 2}, {"NumCopies", u(340), 1},
		{"PageSize w", u(352), 595}, {"PageSize h", u(356), 842},
		{"cupsWidth", u(372), 2379}, {"cupsHeight", u(376), 3408},
		{"cupsBitsPerColor", u(384), 1}, {"cupsBitsPerPixel", u(388), 1}, {"cupsBytesPerLine", u(392), 298},
		{"cupsColorOrder", u(396), 0}, {"cupsColorSpace", u(400), 3}, {"cupsNumColors", u(420), 1},
		{"Duplex", u(272), 0},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if name := string(bytes.TrimRight(h1[1732:1796], "\x00")); name != "A4" {
		t.Errorf("cupsPageSizeName %q", name)
	}
	if len(ras) != 4+2*(HeaderSize+298*3408) {
		t.Errorf("raster is %d bytes", len(ras))
	}
	// Page 1: the black bar at (100,100)-(400,200) of the sheet lands 50 px left and up
	// (12 pt at 300 dpi) in the imageable area.
	data := ras[4+HeaderSize:]
	row := func(y int) []byte { return data[y*298 : (y+1)*298] }
	if row(49)[10] != 0 || row(50)[6] != 0x3f || row(50)[7] != 0xff || row(50)[42] != 0xff || row(50)[43] != 0xfc || row(149)[10] != 0xff || row(150)[10] != 0 {
		t.Errorf("page 1 bar misplaced: row 50 % x", row(50)[:48])
	}
	// Page 2, 150 dpi all black, scaled to the sheet: black everywhere except the edge columns.
	data2 := ras[4+2*HeaderSize+298*3408:]
	if data2[298*1000+100] != 0xff {
		t.Errorf("page 2 not black: %x", data2[298*1000+100])
	}
	text := res.Inspect[0]
	if !strings.Contains(text, "bytes:") {
		t.Errorf("inspect %q", text)
	}
	insp, err := Inspect(ras)
	if err != nil || strings.Count(insp, "\n") != 2 || !strings.Contains(insp, "page 2: A4 595x842pt 2379x3408 px @300x300 dpi, 1-bit cs 3") {
		t.Errorf("inspect raster %v:\n%s", err, insp)
	}
	drivertest.Golden(t, "attributes-laser.golden", []byte(res.Attributes))
	if res.TXT["URF"] != "V1.4,W8,CP99,DM1,IS1,MT1,RS300-600" || res.TXT["Duplex"] != "T" {
		t.Errorf("TXT %v", res.TXT)
	}
}

func TestDriverDuplexAndResolution(t *testing.T) {
	ppd := fakePPD(t, "")
	src, _ := os.ReadFile(ppd)
	os.WriteFile(ppd, bytes.Replace(src, []byte(`*cupsBackSide: "Normal"`), []byte(`*cupsBackSide: "Rotated"`), 1), 0o644)
	d := newDriver(t, driver.Config{PPD: ppd, HighRes: true, Media: "Letter"})
	caps := d.Capabilities()
	if caps.DefaultMedia != driver.MediaLetter || caps.Qualities[1] != driver.QualityHigh || len(caps.Resolutions) != 2 {
		t.Fatalf("caps %+v", caps)
	}
	job := drivertest.Job(t, caps)
	job.Sides = "two-sided-long-edge"
	job.Quality = driver.QualityHigh
	a5, ok := caps.LookupName(driver.MediaA5)
	if !ok {
		t.Fatal("no A5")
	}
	job.Media = a5
	mark := drivertest.Rect{X0: 0, Y0: 0, X1: 10, Y1: 10}
	pages := []raster.Page{drivertest.SheetPage(a5, 300, 0, mark), drivertest.SheetPage(a5, 300, 0, mark)}
	res := drivertest.Run(t, d, job, pages...)
	argv, _, ras := splitEcho(t, res.Data)
	if !strings.Contains(argv, "Duplex=DuplexNoTumble PageRegion=A5 PageSize=A5 Resolution=600dpi") {
		t.Errorf("argv %s", argv)
	}
	h, err := UnmarshalHeader(ras[4:], binary.LittleEndian)
	if err != nil || !h.Duplex || h.Tumble || h.HWResolution != [2]uint32{600, 600} || h.CupsPageSizeName != "A5" {
		t.Fatalf("header %+v %v", h, err)
	}
	// A5 imageable area 12 12 408 583 at 600 dpi: 3300x4758. The mark sits at the sheet's
	// top-left corner, inside the cleared margin, so both pages are blank.
	if h.CupsWidth != 3300 || h.CupsHeight != 4758 {
		t.Errorf("size %dx%d", h.CupsWidth, h.CupsHeight)
	}

	job.Sides = "two-sided-short-edge"
	res = drivertest.Run(t, d, job, drivertest.SheetPage(a5, 300, 0))
	argv, _, ras = splitEcho(t, res.Data)
	if h, _ = UnmarshalHeader(ras[4:], binary.LittleEndian); !h.Duplex || !h.Tumble || !strings.Contains(argv, "Duplex=DuplexTumble") {
		t.Errorf("short edge: duplex %v tumble %v, argv %s", h.Duplex, h.Tumble, argv)
	}

	// A visible mark near the top-left: on the back page with cupsBackSide Rotated (long edge)
	// it must end up bottom-right.
	job.Sides = "two-sided-long-edge"
	mark = drivertest.Rect{X0: 100, Y0: 100, X1: 110, Y1: 110}
	pages = []raster.Page{drivertest.SheetPage(a5, 300, 0, mark), drivertest.SheetPage(a5, 300, 0, mark)}
	job.Quality = driver.QualityNormal
	res = drivertest.Run(t, d, job, pages...)
	_, _, ras = splitEcho(t, res.Data)
	h, _ = UnmarshalHeader(ras[4:], binary.LittleEndian)
	bpl, rows := int(h.CupsBytesPerLine), int(h.CupsHeight)
	front := ras[4+HeaderSize : 4+HeaderSize+bpl*rows]
	back := ras[4+2*HeaderSize+bpl*rows:]
	ink := func(b []byte) (first, last int) {
		first, last = -1, -1
		for i, v := range b {
			if v != 0 {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		return
	}
	ff, _ := ink(front)
	bf, bl := ink(back)
	if ff < 0 || bf < 0 || ff/bpl > rows/4 || bl/bpl < rows*3/4 {
		t.Errorf("front ink from row %d, back ink rows %d-%d of %d", ff/bpl, bf/bpl, bl/bpl, rows)
	}
}

// TestDuplexBackPageAsymmetricArea follows each inked back-page raster pixel to where it lands on
// the sheet. The printer puts raster row 0 at the imageable area's top in its own frame, then
// turns or flips the sheet, so with unequal margins the crop window must be mirrored too.
func TestDuplexBackPageAsymmetricArea(t *testing.T) {
	const left, bottom, right, top = 12.0, 36.0, 560.0, 830.0   // A4 595x842
	mark := drivertest.Rect{X0: 400, Y0: 250, X1: 440, Y1: 290} // 300 dpi sheet pixels
	for _, c := range []struct {
		backSide, sides string
		fx, fy          bool
	}{
		{"Rotated", "two-sided-long-edge", true, true},
		{"Flipped", "two-sided-long-edge", false, true},
		{"Flipped", "two-sided-short-edge", true, false},
		{"ManualTumble", "two-sided-short-edge", true, true},
		{"Normal", "two-sided-long-edge", false, false},
	} {
		t.Run(c.backSide+"/"+c.sides, func(t *testing.T) {
			ppd := fakePPD(t, "")
			src, _ := os.ReadFile(ppd)
			src = bytes.Replace(src, []byte(`*cupsBackSide: "Normal"`), []byte(`*cupsBackSide: "`+c.backSide+`"`), 1)
			src = bytes.Replace(src, []byte(`*ImageableArea A4/A4: "12 12 583 830"`),
				[]byte(fmt.Sprintf(`*ImageableArea A4/A4: "%g %g %g %g"`, left, bottom, right, top)), 1)
			if err := os.WriteFile(ppd, src, 0o644); err != nil {
				t.Fatal(err)
			}
			d := newDriver(t, driver.Config{PPD: ppd, Media: "A4"})
			job := drivertest.Job(t, d.Capabilities())
			job.Sides = c.sides
			a4 := job.Media
			res := drivertest.Run(t, d, job, drivertest.SheetPage(a4, 300, 0, mark), drivertest.SheetPage(a4, 300, 0, mark))
			_, _, ras := splitEcho(t, res.Data)
			h, err := UnmarshalHeader(ras[4:], binary.LittleEndian)
			if err != nil || h.CupsBitsPerPixel != 1 || h.HWResolution != [2]uint32{300, 300} {
				t.Fatalf("header %+v %v", h, err)
			}
			bpl, rows := int(h.CupsBytesPerLine), int(h.CupsHeight)
			for page, flips := range [][2]bool{{false, false}, {c.fx, c.fy}} {
				fx, fy := flips[0], flips[1]
				data := ras[4+(page+1)*HeaderSize+page*bpl*rows:][:bpl*rows]
				inked := 0
				for r := 0; r < rows; r++ {
					for col := 0; col < int(h.CupsWidth); col++ {
						if data[r*bpl+col/8]&(0x80>>(col%8)) == 0 {
							continue
						}
						inked++
						// Device frame: the raster starts at the area's top-left corner.
						x := left + (float64(col)+0.5)*72/300
						y := 842 - top + (float64(r)+0.5)*72/300
						if fx {
							x = 595 - x
						}
						if fy {
							y = 842 - y
						}
						px, py := x*300/72, y*300/72
						if px < float64(mark.X0)-1 || px > float64(mark.X1)+1 || py < float64(mark.Y0)-1 || py > float64(mark.Y1)+1 {
							t.Fatalf("page %d: raster pixel %d,%d lands at sheet pixel %.0f,%.0f, outside the mark %+v", page+1, col, r, px, py, mark)
						}
					}
				}
				if inked < 40*40*9/10 {
					t.Fatalf("page %d: %d inked pixels", page+1, inked)
				}
			}
		})
	}
}

// TestHeaderFractionalPaper pins the page geometry for a PPD with fractional PaperDimension and
// ImageableArea, as vendor tools write them. PageSize is rounded on purpose (see header).
func TestHeaderFractionalPaper(t *testing.T) {
	ppd := fakePPD(t, "")
	src, _ := os.ReadFile(ppd)
	src = bytes.Replace(src, []byte(`*PaperDimension A4/A4: "595 842"`), []byte(`*PaperDimension A4/A4: "595.28 841.89"`), 1)
	src = bytes.Replace(src, []byte(`*ImageableArea A4/A4: "12 12 583 830"`), []byte(`*ImageableArea A4/A4: "12.5 12.25 583.3 829.9"`), 1)
	if err := os.WriteFile(ppd, src, 0o644); err != nil {
		t.Fatal(err)
	}
	d := newDriver(t, driver.Config{PPD: ppd, Media: "A4", HighRes: true})
	caps := d.Capabilities()
	for _, c := range []struct {
		dpi           int
		width, height uint32
	}{{300, 2378, 3407}, {600, 4757, 6814}} {
		r := resChoice{}
		for _, x := range d.res {
			if x.res.X == c.dpi {
				r = x
			}
		}
		if r.res.X == 0 {
			t.Fatalf("no %d dpi resolution in %v", c.dpi, caps.Resolutions)
		}
		h, _, err := d.header(d.sizeFor(caps.Default()), r, "one-sided")
		if err != nil {
			t.Fatal(err)
		}
		if h.PageSize != [2]uint32{595, 842} || h.CupsPageSize != [2]float32{595.28, 841.89} ||
			h.ImagingBoundingBox != [4]uint32{12, 12, 583, 829} || h.Margins != [2]uint32{12, 12} ||
			h.CupsImagingBBox != [4]float32{12.5, 12.25, 583.3, 829.9} ||
			h.CupsWidth != c.width || h.CupsHeight != c.height {
			t.Errorf("%d dpi: PageSize %v %v, bbox %v %v, margins %v, %dx%d; want %dx%d", c.dpi, h.PageSize, h.CupsPageSize,
				h.ImagingBoundingBox, h.CupsImagingBBox, h.Margins, h.CupsWidth, h.CupsHeight, c.width, c.height)
		}
	}
}

// TestSheetCap checks that New refuses or drops page sizes whose single sheet would not fit the
// job budget (they used to pass New and abort the process on the first job), and that a job on
// a size too large at the asked resolution falls back to a lower one.
func TestSheetCap(t *testing.T) {
	edit := func(t *testing.T, repl ...string) string {
		ppd := fakePPD(t, "")
		src, _ := os.ReadFile(ppd)
		text := strings.NewReplacer(repl...).Replace(string(src))
		if err := os.WriteFile(ppd, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return ppd
	}
	const a4Size = `*PageSize A4/A4: "<</PageSize[595 842]/ImagingBBox null>>setpagedevice"`
	const res600 = `*Resolution 600dpi/600 DPI: "<</HWResolution[600 600]/cupsBitsPerColor 1/cupsColorSpace 3/cupsColorOrder 0>>setpagedevice"`

	t.Run("huge", func(t *testing.T) {
		ppd := edit(t,
			`*PaperDimension A4/A4: "595 842"`, `*PaperDimension A4/A4: "720 7200"`,
			`*ImageableArea A4/A4: "12 12 583 830"`, `*ImageableArea A4/A4: "0 0 720 7200"`,
			res600, `*Resolution 10000dpi/10000 DPI: "<</HWResolution[10000 10000]/cupsBitsPerColor 1/cupsColorSpace 3/cupsColorOrder 0>>setpagedevice"`,
			`*Resolution 300dpi/300 DPI: "<</HWResolution[300 300]/cupsBitsPerColor 1/cupsColorSpace 3/cupsColorOrder 0>>setpagedevice"`, ``,
			`*DefaultResolution: 300dpi`, `*DefaultResolution: 10000dpi`)
		_, err := driver.New("cups", driver.Config{PPD: ppd, Media: "A4", Logf: t.Logf})
		if err == nil || !strings.Contains(err.Error(), "sheet too large") {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("dropped and lowered", func(t *testing.T) {
		ppd := edit(t,
			a4Size, a4Size+"\n"+`*PageSize Banner/Banner: "<</PageSize[720 7200]/ImagingBBox null>>setpagedevice"`+
				"\n"+`*PageSize Huge/Huge: "<</PageSize[2000 20000]/ImagingBBox null>>setpagedevice"`,
			`*PaperDimension A4/A4: "595 842"`, `*PaperDimension A4/A4: "595 842"`+"\n"+`*PaperDimension Banner/Banner: "720 7200"`+
				"\n"+`*PaperDimension Huge/Huge: "2000 20000"`)
		var logs []string
		d, err := driver.New("cups", driver.Config{PPD: ppd, Media: "A4", HighRes: true, Logf: func(f string, a ...any) {
			logs = append(logs, fmt.Sprintf(f, a...))
		}})
		if err != nil {
			t.Fatal(err)
		}
		caps := d.Capabilities()
		if len(caps.Resolutions) != 2 || caps.Default().Name != driver.MediaA4 {
			t.Fatalf("caps %+v", caps)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "skipping page size Huge") {
			t.Errorf("logs %q", logs)
		}
		var banner driver.Media
		for _, m := range caps.Media {
			if m.Width == ptToHmm(720) && m.Length == ptToHmm(7200) {
				banner = m
			}
			if m.Width == ptToHmm(2000) {
				t.Fatalf("Huge advertised as %s", m.Name)
			}
		}
		if banner.Name == "" {
			t.Fatalf("no banner in %+v", caps.Media)
		}
		job := drivertest.Job(t, caps)
		job.Media, job.Quality = banner, driver.QualityHigh
		logs = nil
		job.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
		units, err := d.Render(context.Background(), job, []raster.Page{drivertest.SheetPage(banner, 100, 0)})
		if err != nil || len(units) != 1 {
			t.Fatalf("units %d, %v", len(units), err)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "too large at 600x600 dpi; using 300x300 dpi") {
			t.Errorf("logs %q", logs)
		}
	})
}

func TestDriverFilterFailures(t *testing.T) {
	ppd := fakePPD(t, "")
	sheet := func(caps driver.Capabilities) raster.Page {
		return drivertest.SheetPage(caps.Default(), 100, 0)
	}
	cases := []struct {
		opts map[string]string
		want string
	}{
		{map[string]string{"options": "TestMode=fail"}, "exit status 1: Printer jammed (state +media-jam-error)"},
		{map[string]string{"options": "TestMode=hang", "timeout": "300ms"}, "timed out after 300ms"},
		{map[string]string{"options": "TestMode=flood", "max-output": "1M"}, "output exceeds 1048576 bytes"},
		{map[string]string{"options": "TestMode=silent"}, "produced no output"},
	}
	for _, c := range cases {
		d := newDriver(t, driver.Config{PPD: ppd, Options: c.opts})
		job := drivertest.Job(t, d.Capabilities())
		start := time.Now()
		_, err := d.Render(context.Background(), job, []raster.Page{sheet(d.Capabilities())})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err %v, want %q", c.opts, err, c.want)
		}
		if time.Since(start) > 20*time.Second {
			t.Errorf("%v took %v", c.opts, time.Since(start))
		}
	}

	d := newDriver(t, driver.Config{PPD: ppd, Options: map[string]string{"options": "TestMode=hang"}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.Render(ctx, drivertest.Job(t, d.Capabilities()), []raster.Page{sheet(d.Capabilities())})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancelled job: %v", err)
	}
	// Nothing reached the printer, so there is no device job to end: no grace.
	if time.Since(start) > 10*time.Second {
		t.Errorf("cancelled job without output took %v", time.Since(start))
	}

	_, err = d.Render(context.Background(), drivertest.Job(t, d.Capabilities()), []raster.Page{{Width: 10, Height: 10, XDPI: 300, YDPI: 300}})
	if !errors.Is(err, driver.ErrDocument) {
		t.Errorf("malformed page: %v", err)
	}
	_, err = d.Render(context.Background(), drivertest.Job(t, d.Capabilities()), nil)
	if !errors.Is(err, driver.ErrDocument) {
		t.Errorf("no pages: %v", err)
	}
}

// TestOutWriter checks that the filter's output passes straight on, and that the filter is
// stopped once it passes max-output or the destination fails.
func TestOutWriter(t *testing.T) {
	const limit = 1<<20 + 123
	cancelled := false
	var dst bytes.Buffer
	w := &outWriter{w: &dst, max: limit, cancel: func() { cancelled = true }, act: &activity{}}
	chunk := bytes.Repeat([]byte{'x'}, 4093)
	written := 0
	for written+len(chunk) <= limit {
		if n, err := w.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("write at %d: %d %v", written, n, err)
		}
		written += len(chunk)
	}
	if _, err := w.Write(chunk); !errors.Is(err, errOutputCap) || !cancelled {
		t.Fatalf("over the cap: %v cancelled %v", err, cancelled)
	}
	if n, _, over := w.result(); n != int64(written) || !over || dst.Len() != written {
		t.Fatalf("passed %d (%d), over %v, want %d", n, dst.Len(), over, written)
	}

	cancelled = false
	broken := errors.New("printer gone")
	w = &outWriter{w: failWriter{broken}, max: limit, cancel: func() { cancelled = true }, act: &activity{}}
	if _, err := w.Write(chunk); !errors.Is(err, broken) || !cancelled {
		t.Fatalf("broken destination: %v cancelled %v", err, cancelled)
	}
	if _, err, over := w.result(); !errors.Is(err, broken) || over {
		t.Fatalf("result %v %v", err, over)
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestStderrLog(t *testing.T) {
	var lines, msgs []string
	s := &stderrLog{name: "f", logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) },
		message: func(m string) { msgs = append(msgs, m) }}
	s.Write([]byte("DEBUG: x\nERR"))
	s.Write([]byte("OR: bad thing\r\nSTATE: -connecting-to-device\nINFO: 5% done\nplain text\nWARNING: low"))
	s.flush()
	want := []string{"filter f: error: bad thing", "filter f: state -connecting-to-device", "filter f: plain text", "filter f: warning: low"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines %q", lines)
	}
	if strings.Join(msgs, "|") != "bad thing" {
		t.Errorf("state messages %q", msgs)
	}
	if got := s.summary(); got != ": bad thing (state -connecting-to-device)" {
		t.Errorf("summary %q", got)
	}
	for i := 0; i < 2*maxStderrLogged; i++ {
		s.Write([]byte("NOTICE: spam\n"))
	}
	if len(lines) != maxStderrLogged || s.dropped == 0 {
		t.Errorf("%d lines logged, %d dropped", len(lines), s.dropped)
	}
}

func TestNewErrors(t *testing.T) {
	ppd := fakePPD(t, "")
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	laser, err := os.ReadFile("testdata/laser-pcl.ppd")
	if err != nil {
		t.Fatal(err)
	}
	noFilter := write("ps.ppd", strings.Replace(string(laser), `*cupsFilter: "application/vnd.cups-raster 50 rastertohp"`, "", 1))
	missing := write("missing.ppd", strings.Replace(string(laser), "rastertohp", "no-such-filter", 1))
	lab := write("lab.ppd", strings.ReplaceAll(string(laser), "cupsColorSpace 3", "cupsColorSpace 16"))
	cases := []struct {
		cfg  driver.Config
		want string
	}{
		{driver.Config{}, "needs --ppd"},
		{driver.Config{PPD: filepath.Join(dir, "nope.ppd")}, "no such file"},
		{driver.Config{PPD: noFilter}, "no *cupsFilter"},
		{driver.Config{PPD: missing, Options: map[string]string{"filter-dir": dir}}, "no-such-filter not found in " + dir},
		{driver.Config{PPD: ppd, Options: map[string]string{"filter": "../bin/sh"}}, "bare program name"},
		{driver.Config{PPD: ppd, Options: map[string]string{"filter": write("notexec", "x")}}, "not an executable"},
		{driver.Config{PPD: lab, Options: map[string]string{"filter": "-"}}, "cupsColorSpace 16 is not supported"},
		{driver.Config{PPD: ppd, Media: "Tabloid"}, `media "Tabloid" is not a PageSize of the PPD (have Letter, A4, A5, Legal)`},
		{driver.Config{PPD: ppd, Options: map[string]string{"options": "Nope=1"}}, "no option Nope"},
		{driver.Config{PPD: ppd, Options: map[string]string{"options": "InputSlot=Tray9"}}, "InputSlot has no choice Tray9 (have Auto, Manual)"},
		{driver.Config{PPD: ppd, Options: map[string]string{"options": "PageSize=A4"}}, "--media"},
		{driver.Config{PPD: ppd, Options: map[string]string{"options": "Duplex=None"}}, "duplex=false"},
		{driver.Config{PPD: ppd, Options: map[string]string{"options": "garbage"}}, "not Key=Choice"},
		{driver.Config{PPD: ppd, Options: map[string]string{"timeout": "soon"}}, "positive duration"},
		{driver.Config{PPD: ppd, Options: map[string]string{"max-output": "-3"}}, "positive size"},
		{driver.Config{PPD: ppd, Options: map[string]string{"kind": "sticker"}}, "auto, document or label"},
		{driver.Config{PPD: ppd, Options: map[string]string{"duplex": "maybe"}}, "auto, true or false"},
		{driver.Config{PPD: ppd, Options: map[string]string{"full-page": "y"}}, "true or false"},
		{driver.Config{PPD: ppd, Options: map[string]string{"bogus": "1"}}, `no option "bogus"`},
	}
	for _, c := range cases {
		_, err := driver.New("cups", c.cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err %v, want %q", c.cfg, err, c.want)
		}
	}
}

func TestDriverCapabilities(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: "testdata/laser-pcl.ppd", Options: map[string]string{"filter": "-", "duplex": "false", "options": "resolution=600dpi"}})
	caps := d.Capabilities()
	if caps.MakeModel != "Generic PCL Laser" || caps.Duplex || caps.Kind[0] != "document" || len(caps.Resolutions) != 1 ||
		caps.Resolutions[0] != (driver.Resolution{X: 600, Y: 600}) || caps.DefaultMedia != driver.MediaA4 || caps.CopiesNative || caps.MaxCopies != 99 {
		t.Errorf("caps %+v", caps)
	}
	var names []string
	for _, m := range caps.Media {
		names = append(names, m.Name)
	}
	if got := strings.Join(names, " "); got != "na_letter_8.5x11in iso_a4_210x297mm iso_a5_148x210mm na_legal_8.5x14in" {
		t.Errorf("media %s", got)
	}
	// 12 pt is 4.233 mm.
	if m := caps.Media[1]; m.Margins != (driver.Margins{Top: 423, Bottom: 423, Left: 423, Right: 423}) || m.Type != "stationery" || !m.Ready {
		t.Errorf("A4 %+v", m)
	}
	drivertest.CheckCapabilities(t, caps)

	// A PPD defaulting to 600 dpi still advertises 300 first (clients send that, and it is
	// what the decode cap can hold), but a job naming no resolution prints at 600.
	laser, _ := os.ReadFile("testdata/laser-pcl.ppd")
	hi := filepath.Join(t.TempDir(), "hi.ppd")
	os.WriteFile(hi, bytes.Replace(laser, []byte("*DefaultResolution: 300dpi"), []byte("*DefaultResolution: 600dpi"), 1), 0o644)
	h := newDriver(t, driver.Config{PPD: hi, Options: map[string]string{"filter": "-"}})
	if r := h.Capabilities().Resolutions; r[0] != (driver.Resolution{X: 300, Y: 300}) || r[1] != (driver.Resolution{X: 600, Y: 600}) {
		t.Errorf("resolutions %v", r)
	}
	hjob := drivertest.Job(t, h.Capabilities())
	if r := h.resolutionFor(hjob); r.choice != "600dpi" {
		t.Errorf("default job at %s", r.choice)
	}
	hjob.Resolution = driver.Resolution{X: 300, Y: 300}
	if r := h.resolutionFor(hjob); r.choice != "300dpi" {
		t.Errorf("300 dpi job at %s", r.choice)
	}

	z := newDriver(t, driver.Config{PPD: "testdata/label-zpl.ppd", Options: map[string]string{"filter": "-"}})
	zc := z.Capabilities()
	if zc.Kind[0] != "label" || zc.Media[0].Name != "om_50.8x25.4mm_50.8x25.4mm" || zc.Media[2].Name != driver.Media4x6 ||
		zc.Media[0].Type != "labels" || zc.URF[len(zc.URF)-1] != "RS203" {
		t.Errorf("zpl caps %+v", zc)
	}
	drivertest.CheckCapabilities(t, zc)
	attrs, _ := drivertest.Attributes(t, zc)
	drivertest.Golden(t, "attributes-zpl.golden", []byte(attrs))
}

// TestPassThroughRaster runs with filter "-" (the PPD's way to send raster unfiltered) and a
// colour-only ColorModel default, to cover the gray model pick and the raw output.
func TestPassThroughRaster(t *testing.T) {
	laser, err := os.ReadFile("testdata/laser-pcl.ppd")
	if err != nil {
		t.Fatal(err)
	}
	ppd := strings.Replace(string(laser), "*OpenUI *InputSlot", `*OpenUI *ColorModel/Colour: PickOne
*OrderDependency: 30 AnySetup *ColorModel
*DefaultColorModel: CMYK
*ColorModel CMYK/Colour: "<</cupsColorSpace 6/cupsColorOrder 1/cupsBitsPerColor 1>>setpagedevice"
*ColorModel Gray8/Gray: "<</cupsColorSpace 18/cupsColorOrder 0/cupsBitsPerColor 8>>setpagedevice"
*CloseUI: *ColorModel
*OpenUI *InputSlot`, 1)
	path := filepath.Join(t.TempDir(), "colour.ppd")
	os.WriteFile(path, []byte(ppd), 0o644)
	d := newDriver(t, driver.Config{PPD: path, Media: "a5", Options: map[string]string{"filter": "-", "full-page": "true"}})
	if d.colorModel != "Gray8" {
		t.Fatalf("color model %q", d.colorModel)
	}
	job := drivertest.Job(t, d.Capabilities())
	p := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 200, Y0: 300, X1: 260, Y1: 330})
	res := drivertest.Run(t, d, job, p)
	h, err := UnmarshalHeader(res.Data[4:], binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	// Full page: 420x595 pt at 300 dpi is 1750x2479, sW 8-bit.
	if h.CupsWidth != 1750 || h.CupsHeight != 2479 || h.CupsColorSpace != CSpaceSW || h.CupsBitsPerColor != 8 ||
		h.CupsBytesPerLine != 1750 || h.ImagingBoundingBox != [4]uint32{0, 0, 420, 595} || h.Margins != [2]uint32{} {
		t.Fatalf("header %+v", h)
	}
	// The 60x30 mark at (200, 300) keeps its size; Place centres the 1748-pixel page on the
	// 1750-pixel sheet, so it may move by a pixel.
	data := res.Data[4+HeaderSize:]
	x0, y0, x1, y1 := 1750, 2479, -1, -1
	for i, v := range data {
		if v == 0 {
			x, y := i%1750, i/1750
			x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x), max(y1, y)
		}
	}
	if x1-x0 != 59 || y1-y0 != 29 || abs(x0-200) > 1 || abs(y0-300) > 1 {
		t.Errorf("mark at (%d,%d)-(%d,%d)", x0, y0, x1, y1)
	}
	drivertest.GoldenDigest(t, "passthrough-a5.sha256", res.Data)
	if !strings.HasPrefix(res.Inspect[0], "page 1: A5 420x595pt 1750x2479 px @300x300 dpi, 8-bit cs 18 order 0") {
		t.Errorf("inspect %q", res.Inspect[0])
	}
}
