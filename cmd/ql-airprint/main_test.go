package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/leandroalbero/ql-airprint/internal/ql"
	"github.com/leandroalbero/ql-airprint/internal/raster/testutil"
)

func TestParseArgsInterspersed(t *testing.T) {
	var o options
	fs := newFlagSet(&o, io.Discard)
	pos, err := parseArgs(fs, []string{"--media", "62", "print", "label.png", "--dry-run", "/x", "-v", "--", "--odd"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pos, []string{"print", "label.png", "--odd"}) || o.dryRun != "/x" || !o.verbose {
		t.Fatalf("pos %q dryRun %q verbose %v", pos, o.dryRun, o.verbose)
	}
}

func TestConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ql-airprint.conf")
	conf := `# comment
name = "Shelf Labels"
port=9000
high-res = true
trim = 24
`
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	var o options
	fs := newFlagSet(&o, io.Discard)
	if _, err := parseArgs(fs, []string{"--config", path, "--port", "1234"}); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigFile(fs, path); err != nil {
		t.Fatal(err)
	}
	if o.name != "Shelf Labels" || o.port != 1234 || !o.highRes || o.trim != "24" {
		t.Fatalf("got name=%q port=%d highRes=%v trim=%q", o.name, o.port, o.highRes, o.trim)
	}

	if err := os.WriteFile(path, []byte("colour = red\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigFile(fs, path); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestExampleConfigKeys(t *testing.T) {
	data, err := os.ReadFile("../../packaging/ql-airprint.conf")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		// Settings are commented out as "#key = value"; prose comments start with "# ".
		if len(l) > 1 && l[0] == '#' && l[1] != ' ' && strings.Contains(l, "=") {
			lines = append(lines, l[1:])
		}
	}
	if len(lines) < 10 {
		t.Fatalf("found only %d settings", len(lines))
	}
	path := filepath.Join(t.TempDir(), "conf")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	var o options
	fs := newFlagSet(&o, io.Discard)
	if err := applyConfigFile(fs, path); err != nil {
		t.Fatal(err)
	}
}

func TestParseValues(t *testing.T) {
	for in, want := range map[string]int{"max": -1, "unlimited": -1, "-1": -1, "0": 0, "24": 24} {
		if got, err := parseTrim(in); err != nil || got != want {
			t.Errorf("parseTrim(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := parseTrim("-3"); err == nil {
		t.Error("parseTrim(-3) accepted")
	}
	got, err := parsePresets("29, 50mm,62.5")
	if err != nil || !slices.Equal(got, []int{2900, 5000, 6250}) {
		t.Errorf("parsePresets = %v, %v", got, err)
	}
	if got, err := parsePresets(""); err != nil || len(got) != 0 {
		t.Errorf("empty presets = %v, %v", got, err)
	}
	if _, err := parsePresets("29,abc"); err == nil {
		t.Error("bad preset accepted")
	}
}

func TestStableUUID(t *testing.T) {
	dir := t.TempDir()
	a, err := stableUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := stableUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || len(a) != 36 || a[14] != '4' {
		t.Fatalf("uuids %q %q", a, b)
	}
	c, err := stableUUID(t.TempDir())
	if err != nil || c == a {
		t.Fatalf("second state dir gave %q, %v", c, err)
	}
	if err := os.WriteFile(filepath.Join(dir, seedFile), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stableUUID(dir); err == nil {
		t.Fatal("corrupt seed accepted")
	}
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return out.String(), errb.String(), code
}

func TestPrintCommandPNG(t *testing.T) {
	dir := t.TempDir()
	dry := filepath.Join(dir, "dry")
	img := image.NewGray(image.Rect(0, 0, 600, 40))
	want := ql.NewBitmap(696, 40)
	for y := 0; y < 40; y++ {
		for x := 0; x < 600; x++ {
			black := (x/10+y/10)%2 == 0
			img.SetGray(x, y, color.Gray{Y: 0xff})
			if black {
				img.SetGray(x, y, color.Gray{Y: 0x7f})
				want.Bits[y*696+x] = true
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "label.png")
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runCLI(t, "print", file, "--dry-run", dry)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "printed 1 label") {
		t.Fatalf("stdout %q", out)
	}
	got, err := os.ReadFile(filepath.Join(dry, "job-1.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, labelBytes(t, want)) {
		t.Fatal("PNG print differs from the reference encoding")
	}
}

func TestPrintCommandURFWithDebugDir(t *testing.T) {
	dir := t.TempDir()
	dry, dbg := filepath.Join(dir, "dry"), filepath.Join(dir, "debug")
	rects := []rect{{200, 20, 500, 70}}
	file := filepath.Join(dir, "doc.urf")
	doc := testutil.EncodeURF(testutil.GrayPage(pageW, 100, 300, grayPage(pageW, 100, rects...)))
	if err := os.WriteFile(file, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runCLI(t, "--dry-run", dry, "--debug-dir", dbg, "print", file); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(dry, "job-1.bin"))
	if err != nil {
		t.Fatal(err)
	}
	want := expectedLabel(t, pageW, 100, rects...)
	if !bytes.Equal(got, labelBytes(t, want)) {
		t.Fatal("URF print differs from the reference encoding")
	}
	pngs, _ := filepath.Glob(filepath.Join(dbg, "*-p1-label.png"))
	ins, _ := filepath.Glob(filepath.Join(dbg, "*-in.urf"))
	if len(pngs) != 1 || len(ins) != 1 {
		t.Fatalf("debug files: %v %v", pngs, ins)
	}
	data, err := os.ReadFile(pngs[0])
	if err != nil {
		t.Fatal(err)
	}
	back, err := readPNG(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.W != want.W || back.H != want.H || !slices.Equal(back.Bits, want.Bits) {
		t.Fatal("debug label PNG does not round-trip")
	}
}

func TestCommandErrors(t *testing.T) {
	if out, _, code := runCLI(t, "version"); code != 0 || !strings.HasPrefix(out, "ql-airprint ") {
		t.Fatalf("version: %d %q", code, out)
	}
	for _, args := range [][]string{
		{"frobnicate"},
		{"print"},
		{"status", "extra"},
		{"--trim", "x", "print", "f"},
		{"--no-such-flag"},
	} {
		if _, _, code := runCLI(t, args...); code == 0 {
			t.Errorf("%q succeeded", args)
		}
	}
	if _, errOut, code := runCLI(t, "--media", "23x23", "--dry-run", t.TempDir(), "status"); code == 0 || !strings.Contains(errOut, "continuous") {
		t.Errorf("die-cut media: %d %q", code, errOut)
	}
}

func TestStatusCommandDryRun(t *testing.T) {
	out, errOut, code := runCLI(t, "status", "--dry-run", t.TempDir())
	if code != 0 || !strings.Contains(out, "condition:      idle") {
		t.Fatalf("exit %d %q %q", code, out, errOut)
	}
}
