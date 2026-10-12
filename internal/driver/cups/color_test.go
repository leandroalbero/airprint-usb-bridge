package cups

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/drivertest"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

const colorPPD = "testdata/color-deskjet.ppd"

func TestColorFromPPD(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: colorPPD})
	caps := d.Capabilities()
	drivertest.CheckCapabilities(t, caps)
	if !caps.Color || d.colorChoice != "CMYK" || d.colorModel != "Gray" {
		t.Fatalf("colour %v, colour model %q, gray model %q", caps.Color, d.colorChoice, d.colorModel)
	}
	_, txt := drivertest.Attributes(t, caps)
	if txt["Color"] != "T" || txt["URF"] != "V1.4,W8,SRGB24,CP99,IS1,MT1,RS150-300-600" {
		t.Errorf("TXT %v", txt)
	}

	if off := newDriver(t, driver.Config{PPD: colorPPD, Color: "off"}); off.Capabilities().Color {
		t.Error("--color off still colour")
	}
	rgb := newDriver(t, driver.Config{PPD: colorPPD, Options: map[string]string{"options": "ColorModel=RGB"}})
	if !rgb.Capabilities().Color || rgb.colorChoice != "" || rgb.colorModel != "" {
		t.Errorf("pinned RGB: colour %v choice %q gray %q", rgb.Capabilities().Color, rgb.colorChoice, rgb.colorModel)
	}
	if h, _, err := rgb.headerColor(rgb.sizes[0], rgb.res[0], "", true); err != nil || h.CupsColorSpace != CSpaceCMY || h.CupsColorOrder != OrderBanded {
		t.Errorf("pinned RGB header %d/%d %v", h.CupsColorSpace, h.CupsColorOrder, err)
	}
	gray := newDriver(t, driver.Config{PPD: colorPPD, Options: map[string]string{"options": "ColorModel=Gray"}})
	if gray.Capabilities().Color {
		t.Error("pinned Gray ColorModel still colour")
	}

	if laser := newDriver(t, driver.Config{PPD: "testdata/laser-pcl.ppd"}); laser.Capabilities().Color {
		t.Error("laser PPD (ColorDevice False, no ColorModel) is colour")
	}
	if _, err := driver.New("cups", driver.Config{PPD: "testdata/laser-pcl.ppd", Color: "on"}); err == nil || !strings.Contains(err.Error(), "--color on") {
		t.Errorf("--color on with a gray PPD: %v", err)
	}
	// ColorDevice True alone is not enough: the raster the filter gets must be colour.
	laser, _ := os.ReadFile("testdata/laser-pcl.ppd")
	path := filepath.Join(t.TempDir(), "claims.ppd")
	os.WriteFile(path, bytes.Replace(laser, []byte("*ColorDevice: False"), []byte("*ColorDevice: True"), 1), 0o644)
	if d := newDriver(t, driver.Config{PPD: path}); d.Capabilities().Color {
		t.Error("ColorDevice True with only a K raster format is colour")
	}
}

// hdr is a header of one colour format for encoder tests.
func hdr(space, order, bpc uint32, w, h int) (*Header, format) {
	hd := &Header{CupsColorSpace: space, CupsColorOrder: order, CupsBitsPerColor: bpc, CupsWidth: uint32(w), CupsHeight: uint32(h)}
	f, err := layout(hd, w, h)
	if err != nil {
		panic(err)
	}
	return hd, f
}

// The encoder's output for one row of red, red, green, blue, black, white, white, cyan, worked
// out by hand from cups/raster.h: KCMY is K C M Y, black generation K = min(C, M, Y).
func TestColorEncoderHandChecked(t *testing.T) {
	row := [][3]byte{{255, 0, 0}, {255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {0, 0, 0}, {255, 255, 255}, {255, 255, 255}, {0, 255, 255}}
	page := drivertest.RGBPage(len(row), 1, 300, func(x, _ int) (byte, byte, byte) { return row[x][0], row[x][1], row[x][2] })
	encode := func(space, order, bpc uint32) []byte {
		h, f := hdr(space, order, bpc, len(row), 1)
		e := newColorPage(h, f, page, 0, 0, false)
		out := e.next(nil, 1<<20)
		if !e.done() || len(out) != f.bytesPerRow*f.rows {
			t.Fatalf("cs %d order %d %d-bit: %d bytes, want %d", space, order, bpc, len(out), f.bytesPerRow*f.rows)
		}
		return out
	}
	cases := []struct {
		name              string
		space, order, bpc uint32
		want              []byte
	}{
		// K 00001000, C 00110001, M 11010000, Y 11100000.
		{"KCMY banded 1-bit", CSpaceKCMY, OrderBanded, 1, []byte{0x08, 0x31, 0xd0, 0xe0}},
		{"KCMY planar 1-bit", CSpaceKCMY, OrderPlanar, 1, []byte{0x08, 0x31, 0xd0, 0xe0}},
		// C 00110001, M 11010000, Y 11100000, then K 00001000.
		{"CMYK banded 1-bit", CSpaceCMYK, OrderBanded, 1, []byte{0x31, 0xd0, 0xe0, 0x08}},
		// Without black generation black is C+M+Y: C 00111001, M 11011000, Y 11101000.
		{"CMY banded 1-bit", CSpaceCMY, OrderBanded, 1, []byte{0x39, 0xd8, 0xe8}},
		{"YMC planar 1-bit", CSpaceYMC, OrderPlanar, 1, []byte{0xe8, 0xd8, 0x39}},
		// Nibbles K C M Y per pixel: red 0011, green 0101, blue 0110, black 1000, white 0000,
		// cyan 0100.
		{"KCMY chunked 1-bit", CSpaceKCMY, OrderChunked, 1, []byte{0x33, 0x56, 0x80, 0x04}},
		// RGB is the pixels as they are; additive 1-bit is 1 for full intensity.
		{"RGB banded 1-bit", CSpaceRGB, OrderBanded, 1, []byte{0xc6, 0x27, 0x17}},
		// Gray (R+G+B)/3: 85 85 85 85 0 255 255 170; 1-bit W is 1 from 128 up.
		{"W chunked 1-bit", CSpaceW, OrderChunked, 1, []byte{0x07}},
		{"K chunked 1-bit", CSpaceK, OrderChunked, 1, []byte{0xf8}},
		// 2-bit K samples are the top two bits of the ink: 170 -> 2, 255 -> 3, 0 -> 0, 85 -> 1.
		{"K chunked 2-bit", CSpaceK, OrderChunked, 2, []byte{0xaa, 0xc1}},
	}
	for _, c := range cases {
		if got := encode(c.space, c.order, c.bpc); !bytes.Equal(got, c.want) {
			t.Errorf("%s: % x, want % x", c.name, got, c.want)
		}
	}
	// 8-bit chunky CMYK and sRGB, 16-bit YMCK (samples in the stream's byte order).
	if got := encode(CSpaceCMYK, OrderChunked, 8)[:12]; !bytes.Equal(got, []byte{0, 255, 255, 0, 0, 255, 255, 0, 255, 0, 255, 0}) {
		t.Errorf("CMYK 8-bit % x", got)
	}
	if got := encode(CSpaceSRGB, OrderChunked, 8)[:9]; !bytes.Equal(got, []byte{255, 0, 0, 255, 0, 0, 0, 255, 0}) {
		t.Errorf("sRGB 8-bit % x", got)
	}
	if got := encode(CSpaceYMCK, OrderChunked, 16)[:8]; !bytes.Equal(got, []byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0}) {
		t.Errorf("YMCK 16-bit % x", got)
	}
}

// A page window that leaves the sheet is white there, and gray pages expand to R=G=B.
func TestColorEncoderWindowAndGray(t *testing.T) {
	page := drivertest.Page(4, 2, 300, func(x, y int) byte { return byte(60 * x) })
	h, f := hdr(CSpaceRGB, OrderChunked, 8, 6, 3)
	e := newColorPage(h, f, page, -1, 0, false)
	got := e.next(nil, 1)
	got = e.next(got, 1<<20)
	want := []byte{}
	for y := 0; y < 3; y++ {
		for x := -1; x < 5; x++ {
			v := byte(255)
			if x >= 0 && x < 4 && y < 2 {
				v = byte(60 * x)
			}
			want = append(want, v, v, v)
		}
	}
	if !bytes.Equal(got, want) {
		t.Errorf("window:\n% x\nwant\n% x", got, want)
	}
}

// Dithered low-bit colour keeps each channel's mean.
func TestColorEncoderDithers(t *testing.T) {
	page := drivertest.RGBPage(64, 64, 300, func(x, y int) (byte, byte, byte) { return 255, 128, 64 })
	for _, bpc := range []uint32{1, 2, 4} {
		h, f := hdr(CSpaceCMY, OrderPlanar, bpc, 64, 64)
		out := newColorPage(h, f, page, 0, 0, true).next(nil, 1<<30)
		for c, want := range []float64{0, 127, 191} {
			sum := 0
			for y := 0; y < 64; y++ {
				line := out[(c*64+y)*f.bytesPerRow:]
				for x := 0; x < 64; x++ {
					bit := x * int(bpc)
					sum += int(line[bit/8]>>(8-int(bpc)-bit%8)) & (1<<bpc - 1)
				}
			}
			mean := float64(sum) / 4096 * 255 / float64(int(1)<<bpc-1)
			if mean < want-4 || mean > want+4 {
				t.Errorf("%d-bit channel %d mean %.1f, want %.0f", bpc, c, mean, want)
			}
		}
	}
}

func redPage(m driver.Media, dpi int) raster.Page {
	w, h := (m.Width*dpi+1270)/2540, (m.Length*dpi+1270)/2540
	return drivertest.RGBPage(w, h, dpi, func(int, int) (byte, byte, byte) { return 255, 0, 0 })
}

// A colour job writes the colour ColorModel's raster; a gray job on the same driver is byte for
// byte what the driver writes with --color off.
func TestColorJobRaster(t *testing.T) {
	raw := map[string]string{"filter": "-"}
	d := newDriver(t, driver.Config{PPD: colorPPD, Options: raw})
	job := drivertest.Job(t, d.Capabilities())
	res := drivertest.Run(t, d, job, redPage(job.Media, 300))
	h, err := UnmarshalHeader(res.Data[4:], binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if h.CupsColorSpace != CSpaceKCMY || h.CupsColorOrder != OrderBanded || h.CupsBitsPerColor != 1 || h.CupsCompression != 2 ||
		h.CupsWidth != 2329 || h.CupsHeight != 3208 || h.CupsBytesPerLine != 4*292 {
		t.Fatalf("header %+v", h)
	}
	if !strings.Contains(res.Inspect[0], "1-bit cs 8 order 1") {
		t.Errorf("inspect %q", res.Inspect[0])
	}
	drivertest.GoldenDigest(t, "color-red-a4.sha256", res.Data)
	data := res.Data[4+HeaderSize:]
	if len(data) != 4*292*3208 {
		t.Fatalf("%d bytes of raster", len(data))
	}
	for y := 0; y < 3208; y++ {
		line := data[y*4*292 : (y+1)*4*292]
		k, c, m, ye := line[:292], line[292:584], line[584:876], line[876:]
		full := append(bytes.Repeat([]byte{0xff}, 291), 0x80) // 2329 dots: one in the last byte
		if !bytes.Equal(k, make([]byte, 292)) || !bytes.Equal(c, make([]byte, 292)) || !bytes.Equal(m, full) || !bytes.Equal(ye, full) {
			t.Fatalf("row %d: K % x... C % x... M % x... Y % x...", y, k[:4], c[:4], m[:4], ye[:4])
		}
	}

	off := newDriver(t, driver.Config{PPD: colorPPD, Options: raw, Color: "off"})
	for _, mode := range []string{"monochrome", "auto-monochrome"} {
		job.ColorMode, job.Color = mode, false
		page := drivertest.SheetPage(job.Media, 300, 0, drivertest.Rect{X0: 300, Y0: 300, X1: 900, Y1: 340})
		got := drivertest.Run(t, d, job, page).Data
		want := drivertest.Run(t, off, job, page).Data
		if !bytes.Equal(got, want) {
			t.Errorf("%s job differs from --color off", mode)
		}
		if h, _ := UnmarshalHeader(got[4:], binary.LittleEndian); h.CupsColorSpace != CSpaceK {
			t.Errorf("%s job: colour space %d, want the Gray ColorModel's K", mode, h.CupsColorSpace)
		}
	}
}

// pclPlanes reads rastertohp's raster: per row, planes sent with ESC *b#V and the last with
// ESC *b#W, TIFF-compressed (mode 2), blank rows skipped with ESC *b#Y.
func pclPlanes(t *testing.T, data []byte, planes, rowBytes int) [][][]byte {
	t.Helper()
	start := bytes.Index(data, []byte("\x1b*r1A"))
	if start < 0 {
		t.Fatal("no raster")
	}
	var rows [][][]byte
	var cur [][]byte
	mode := 0
	for i := start + 5; i < len(data); {
		if data[i] != 0x1b || !bytes.HasPrefix(data[i:], []byte("\x1b*b")) {
			if bytes.HasPrefix(data[i:], []byte("\x1b*r")) || data[i] == '\f' {
				break
			}
			t.Fatalf("unexpected byte 0x%02x at %d", data[i], i)
		}
		j := i + 3
		for j < len(data) && data[j] >= '0' && data[j] <= '9' {
			j++
		}
		n, _ := strconv.Atoi(string(data[i+3 : j]))
		cmd := data[j]
		i = j + 1
		switch cmd {
		case 'M':
			mode = n
		case 'Y':
			for k := 0; k < n; k++ {
				rows = append(rows, nil)
			}
		case 'V', 'W':
			if mode != 2 {
				t.Fatalf("compression %d", mode)
			}
			plane, err := imaging.UnpackBits(data[i:i+n], rowBytes)
			if err != nil {
				t.Fatalf("row %d plane %d: %v", len(rows), len(cur), err)
			}
			cur = append(cur, plane)
			i += n
			if cmd == 'W' {
				if len(cur) != planes {
					t.Fatalf("row %d: %d planes", len(rows), len(cur))
				}
				rows = append(rows, cur)
				cur = nil
			}
		default:
			t.Fatalf("ESC *b%d%c", n, cmd)
		}
	}
	return rows
}

// TestRealRasterToHPColor sends a red page through the system's rastertohp with the DeskJet-style
// PPD: the filter must switch to KCMY planes and print magenta and yellow only, where the page is
// red; the monochrome job of the same page is a one-plane black job.
func TestRealRasterToHPColor(t *testing.T) {
	systemFilter(t, "rastertohp")
	d := newDriver(t, driver.Config{PPD: colorPPD})
	job := drivertest.Job(t, d.Capabilities())
	page := redPage(job.Media, 300)
	color := drivertest.Run(t, d, job, page).Data
	for _, want := range []string{"\x1b*t300R", "\x1b*r-4U", "\x1b*r2329S", "\x1b*r3208T", "\x1b*b2M"} {
		if !bytes.Contains(color, []byte(want)) {
			t.Errorf("colour job: no %q", want)
		}
	}
	// Where the placed sheet is red, inside the imageable area (18 pt left, 36 pt top).
	sheet, err := imaging.Place(page, d.sizes[d.byName[job.Media.Name]].sheet(), imaging.PlaceOptions{XDPI: 300, YDPI: 300})
	if err != nil {
		t.Fatal(err)
	}
	x0, y0 := 75, 150
	rows := pclPlanes(t, color, 4, 292)
	if len(rows) != 3208 {
		t.Fatalf("%d rows", len(rows))
	}
	inked := 0
	for y, planes := range rows {
		for x := 0; x < 2329; x++ {
			o := 3 * ((y0+y)*sheet.Width + x0 + x)
			red := sheet.RGB[o] == 255 && sheet.RGB[o+1] == 0 && sheet.RGB[o+2] == 0
			bit := func(p int) bool { return planes != nil && planes[p][x/8]&(0x80>>(x%8)) != 0 }
			if bit(0) || bit(1) || bit(2) != red || bit(3) != red {
				t.Fatalf("(%d,%d): K %v C %v M %v Y %v, red %v", x, y, bit(0), bit(1), bit(2), bit(3), red)
			}
			if red {
				inked++
			}
		}
	}
	if inked < 2329*3208*9/10 {
		t.Errorf("only %d red dots", inked)
	}

	job.ColorMode, job.Color = "monochrome", false
	mono := drivertest.Run(t, d, job, page).Data
	if bytes.Equal(mono, color) || bytes.Contains(mono, []byte("\x1b*r-4U")) || bytes.Contains(mono, []byte("\x1b*r-3U")) {
		t.Error("monochrome job is not a one-plane job")
	}
	// Red is gray 85 in monochrome: dithered, about two dots in three black.
	dots := 0
	for _, planes := range pclPlanes(t, mono, 1, 292) {
		if planes != nil {
			for _, b := range planes[0] {
				dots += bits.OnesCount8(b)
			}
		}
	}
	if f := float64(dots) / (2329 * 3208); f < 0.55 || f > 0.8 {
		t.Errorf("monochrome red page: %.2f of the dots black", f)
	}
	t.Logf("colour %d bytes, monochrome %d bytes", len(color), len(mono))
}

// Every colour sheet is released once its lines are written, turned and scaled ones included.
func TestColorReleasesBudget(t *testing.T) {
	d := newDriver(t, driver.Config{PPD: colorPPD, Options: map[string]string{"filter": "-"}})
	job := drivertest.Job(t, d.Capabilities())
	job.Budget = driver.NewBudget(driver.MaxSheetBytes)
	turned := redPage(job.Media, 300)
	turned.Width, turned.Height = turned.Height, turned.Width
	drivertest.Run(t, d, job, redPage(job.Media, 300), turned, redPage(job.Media, 150))
	if err := job.Budget.Charge(driver.MaxSheetBytes); err != nil {
		t.Errorf("budget not released: %v", err)
	}
}

// CUPS takes a printer's colour from *ColorDevice, so an explicit False wins over a colour
// ColorModel choice unless --color on overrides it.
func TestColorDeviceFalse(t *testing.T) {
	ppd, err := os.ReadFile(colorPPD)
	if err != nil {
		t.Fatal(err)
	}
	mono := bytes.Replace(ppd, []byte("*ColorDevice: True"), []byte("*ColorDevice: False"), 1)
	if bytes.Equal(mono, ppd) {
		t.Fatal("no *ColorDevice: True in " + colorPPD)
	}
	path := filepath.Join(t.TempDir(), "mono.ppd")
	if err := os.WriteFile(path, mono, 0o644); err != nil {
		t.Fatal(err)
	}
	auto := newDriver(t, driver.Config{PPD: path})
	caps := auto.Capabilities()
	if caps.Color || auto.colorChoice != "" {
		t.Errorf("auto: colour %v, choice %q", caps.Color, auto.colorChoice)
	}
	_, txt := drivertest.Attributes(t, caps)
	if txt["Color"] != "F" || strings.Contains(txt["URF"], "SRGB24") {
		t.Errorf("auto TXT %v", txt)
	}
	on := newDriver(t, driver.Config{PPD: path, Color: "on"})
	if !on.Capabilities().Color || on.colorChoice != "CMYK" {
		t.Errorf("--color on: colour %v, choice %q", on.Capabilities().Color, on.colorChoice)
	}

	// Without the keyword the ColorModel choices decide, as before.
	none := bytes.Replace(ppd, []byte("*ColorDevice: True\n"), nil, 1)
	path = filepath.Join(t.TempDir(), "none.ppd")
	if err := os.WriteFile(path, none, 0o644); err != nil {
		t.Fatal(err)
	}
	if d := newDriver(t, driver.Config{PPD: path}); !d.Capabilities().Color {
		t.Error("no *ColorDevice: colour ColorModel choice ignored")
	}
}
