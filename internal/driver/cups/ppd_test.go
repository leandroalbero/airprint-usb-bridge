package cups

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const parsePPD = `*PPD-Adobe: "4.3"
*% comment: *PageSize Bogus: "ignored"
*ModelName: "Test Model"
*NickName: "Test Model, 1.0"
*cupsFilter: "application/vnd.cups-postscript 0 foo"
*cupsFilter: "application/vnd.cups-raster 100 slowfilter"
*cupsFilter: "application/vnd.cups-raster 10 fastfilter"
*OpenUI *PageSize/Media Size: PickOne
*OrderDependency: 10 AnySetup *PageSize
*DefaultPageSize: A4
*PageSize Letter/US Letter: "<</PageSize[612 792]>>setpagedevice"
*PageSize A4/A4: "
  <</PageSize[595 842]
    /ImagingBBox null>>
  setpagedevice"
*End
*CloseUI: *PageSize
*fr.PageSize A4/A4 FR: ""
*PaperDimension A4/A4: "595 842"
*ImageableArea A4/A4: "18.5 36 576.5 806"
*JCLOpenUI *JCLToner/Toner Save: Boolean
*OrderDependency: 5 JCLSetup *JCLToner
*DefaultJCLToner: False
*JCLToner True/On: "@PJL SET ECONOMODE=ON<0A>"
*JCLToner False/Off: "@PJL SET ECONOMODE=OFF<0A>"
*JCLCloseUI: *JCLToner
*OpenUI *Darkness: PickOne
*Darkness 1/Light: "<</cupsCompression 1>>setpagedevice"
*Darkness 2/Dark: "<</cupsCompression 2>>setpagedevice"
*CloseUI: *Darkness
*DefaultResolution: 203dpi
`

func TestParsePPD(t *testing.T) {
	p, err := ParsePPD([]byte(strings.ReplaceAll(parsePPD, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := p.Attr("ModelName", ""); v != "Test Model" {
		t.Errorf("ModelName %q", v)
	}
	ps := p.Option("PageSize")
	if ps == nil || ps.Default != "A4" || ps.Order != 10 || len(ps.Choices) != 2 || ps.Text != "Media Size" {
		t.Fatalf("PageSize %+v", ps)
	}
	a4 := ps.Choice("A4")
	if a4 == nil || !strings.Contains(a4.Code, "/ImagingBBox null>>\n  setpagedevice") || a4.Text != "A4" {
		t.Fatalf("A4 choice %+v", a4)
	}
	if v, _ := p.Attr("ImageableArea", "A4"); v != "18.5 36 576.5 806" {
		t.Errorf("ImageableArea %q", v)
	}
	if _, ok := p.Attr("PageSize", "Bogus"); ok || p.Option("fr.PageSize") != nil {
		t.Error("comment or translation parsed as data")
	}
	toner := p.Option("JCLToner")
	if toner == nil || toner.Default != "False" || toner.Order != 5 || toner.UI != "Boolean" {
		t.Errorf("JCLToner %+v", toner)
	}
	dark := p.Option("Darkness")
	if dark == nil || dark.Default != "1" || dark.Order != 1e9 {
		t.Errorf("Darkness without default or order %+v", dark)
	}
	if order := p.ordered(); order[0].Keyword != "JCLToner" || order[2].Keyword != "Darkness" {
		t.Errorf("order %s %s %s", order[0].Keyword, order[1].Keyword, order[2].Keyword)
	}
	if v, _ := p.Attr("DefaultResolution", ""); v != "203dpi" {
		t.Errorf("DefaultResolution %q", v)
	}
	f, err := p.RasterFilter()
	if err != nil || f.Program != "fastfilter" || f.Cost != 10 {
		t.Errorf("RasterFilter %+v %v", f, err)
	}

	if _, err := ParsePPD([]byte("%!PS-Adobe-3.0\n")); err == nil {
		t.Error("non-PPD accepted")
	}
}

func TestRasterFilter(t *testing.T) {
	cases := []struct {
		ppd, program, dest, err string
	}{
		{`*cupsFilter: "application/vnd.cups-raster 0 a"
*cupsFilter2: "application/vnd.cups-raster application/vnd.hp-pcl 20 /opt/bin/b"`, "/opt/bin/b", "application/vnd.hp-pcl", ""},
		{`*cupsFilter2: "application/pdf application/vnd.x 0 c"`, "", "", "takes application/pdf"},
		{`*ModelName: "PostScript printer"`, "", "", "no *cupsFilter"},
		{`*cupsFilter: "application/vnd.cups-raster 0 -"`, "-", "", ""},
	}
	for _, c := range cases {
		p, err := ParsePPD([]byte("*PPD-Adobe: \"4.3\"\n" + c.ppd + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		f, err := p.RasterFilter()
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err %v, want %q", c.ppd, err, c.err)
			}
			continue
		}
		if err != nil || f.Program != c.program || f.Dest != c.dest {
			t.Errorf("%s: %+v %v", c.ppd, f, err)
		}
	}
}

func TestLoadPPDGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(parsePPD))
	zw.Close()
	path := filepath.Join(t.TempDir(), "x.ppd.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPPD(path)
	if err != nil || p.Option("PageSize") == nil {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := LoadPPD(filepath.Join(t.TempDir(), "missing.ppd")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestParseResolution(t *testing.T) {
	for in, want := range map[string][2]int{
		"300dpi": {300, 300}, "600x300dpi": {600, 300}, "8dpmm": {203, 203}, "203dpi": {203, 203},
	} {
		x, y, ok := parseResolution(in)
		if !ok || x != want[0] || y != want[1] {
			t.Errorf("%s: %d %d %v", in, x, y, ok)
		}
	}
	for _, bad := range []string{"", "300", "fastdpi", "0dpi", "-5dpi", "99999dpi"} {
		if _, _, ok := parseResolution(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestParsePPDMultiLine covers quoted values over several lines, including one never closed, which
// must cost linear time: a 16 MiB PPD of such lines took hours with line-by-line appending.
func TestParsePPDMultiLine(t *testing.T) {
	const ppdHead = "*PPD-Adobe: \"4.3\"\n"
	for _, c := range []struct{ in, key, want string }{
		{"*Foo: \"a\nb\nc\" tail\n*Bar: \"x\"\n", "Foo", "a\nb\nc"},
		{"*Foo: \"a\n\"\n*Bar: \"x\"\n", "Foo", "a\n"},
		{"*Foo: \"a\nb\n*Bar: \"x\"\n", "Foo", "a\nb\n*Bar: "},
		{"*Foo: \"a\nb", "Foo", "a\nb"},
		{"*Foo: \"", "Foo", ""},
	} {
		p, err := ParsePPD([]byte(ppdHead + c.in))
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := p.Attr(c.key, ""); got != c.want {
			t.Errorf("%q: %s = %q, want %q", c.in, c.key, got, c.want)
		}
	}

	const n = 1 << 21
	big := ppdHead + "*Foo: \"" + strings.Repeat("\nx", n) + "\n*Bar: \"y\"\n"
	p, err := ParsePPD([]byte(big))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Attr("Foo", ""); len(got) != 2*n+len("\n*Bar: ") {
		t.Fatalf("Foo is %d bytes", len(got))
	}
	big = ppdHead + "*Foo: \"" + strings.Repeat("\nx", n)
	if p, err = ParsePPD([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Attr("Foo", ""); len(got) != 2*n {
		t.Fatalf("unterminated Foo is %d bytes", len(got))
	}
}

func FuzzParsePPD(f *testing.F) {
	f.Add([]byte(parsePPD))
	f.Add([]byte("*PPD-Adobe: \"4.3\"\n*OpenUI *X: PickOne\n*X a/b: \"<</cupsInteger1 [1 2>>setpagedevice\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParsePPD(data)
		if err != nil {
			return
		}
		h := defaultHeader()
		for _, o := range p.Options {
			for _, c := range o.Choices {
				_ = applyCode(&h, c.Code)
			}
		}
		_, _ = p.RasterFilter()
	})
}
