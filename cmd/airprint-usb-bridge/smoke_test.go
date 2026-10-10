package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// stockIPPToolDirs hold the test files CUPS installs with ipptool.
var stockIPPToolDirs = []string{"/usr/share/cups/ipptool", "/usr/local/share/cups/ipptool"}

func stockIPPToolTest(t *testing.T, name string) string {
	t.Helper()
	for _, d := range stockIPPToolDirs {
		p := filepath.Join(d, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skipf("ipptool's %s is not installed", name)
	return ""
}

// smokeExpect checks what every driver must advertise, plus the printer-kind of this one.
const smokeExpect = `{
	NAME "Driver capabilities"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword requested-attributes all,media-col-database
	STATUS successful-ok
	EXPECT printer-kind OF-TYPE keyword WITH-VALUE "$kind"
	EXPECT document-format-supported WITH-VALUE "image/urf"
	EXPECT document-format-supported WITH-VALUE "image/pwg-raster"
	EXPECT urf-supported WITH-VALUE "V1.4"
	EXPECT urf-supported WITH-VALUE "W8"
	EXPECT pwg-raster-document-type-supported WITH-VALUE "sgray_8"
	EXPECT pwg-raster-document-resolution-supported OF-TYPE resolution
	EXPECT media-col-ready OF-TYPE collection
	EXPECT media-col-database OF-TYPE collection
	EXPECT media-ready OF-TYPE keyword|name
	EXPECT printer-device-id OF-TYPE text WITH-VALUE "/^([-A-Za-z ]+:[^;]*;)+$/"
	EXPECT printer-uuid OF-TYPE uri WITH-VALUE "/^urn:uuid:[0-9a-f-]{36}$/"
}
`

// smokePage is a white page with a black block, the size the driver's default media wants.
func smokePage(w, h, dpi int) []byte {
	g := make([]byte, w*h)
	for i := range g {
		g[i] = 0xff
	}
	for y := h / 4; y < h/2; y++ {
		for x := w / 4; x < 3*w/4; x++ {
			g[y*w+x] = 0
		}
	}
	return testutil.EncodeURF(testutil.GrayPage(w, h, dpi, g))
}

func findCUPSFilter(name string) string {
	for _, d := range []string{"/usr/lib/cups/filter", "/usr/libexec/cups/filter"} {
		p := filepath.Join(d, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestIPPToolSmokeEveryDriver runs a dry-run instance of each driver and drives it with the
// ipptool CUPS ships: Get-Printer-Attributes and Print-Job of a synthetic URF over IPP and IPPS.
// Each job must complete and leave job-<n>.bin and the driver's job-<n>.txt decode.
func TestIPPToolSmokeEveryDriver(t *testing.T) {
	ipptool, err := exec.LookPath("ipptool")
	if err != nil {
		t.Skip("ipptool not on PATH")
	}
	getAttrs := stockIPPToolTest(t, "get-printer-attributes.test")
	printJob := stockIPPToolTest(t, "print-job.test")
	ppd, err := filepath.Abs("../../internal/driver/cups/testdata/laser-pcl.ppd")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		driver     string
		kind       string
		w, h, dpi  int
		mod        func(*options)
		needFilter string
	}{
		{driver: "brother-ql", kind: "roll", w: 732, h: 300, dpi: 300},
		{driver: "pcl", kind: "document", w: 2480, h: 3508, dpi: 300},
		{driver: "postscript", kind: "document", w: 2480, h: 3508, dpi: 300},
		{driver: "cups", kind: "document", w: 2480, h: 3508, dpi: 300, mod: func(o *options) { o.ppd = ppd }, needFilter: "rastertohp"},
		{driver: "dymo", kind: "label", w: 425, h: 1051, dpi: 300},
		{driver: "zpl", kind: "label", w: 812, h: 1218, dpi: 203},
	}
	for _, c := range cases {
		t.Run(c.driver, func(t *testing.T) {
			if c.needFilter != "" && findCUPSFilter(c.needFilter) == "" {
				t.Skipf("CUPS filter %s not installed", c.needFilter)
			}
			ts := startService(t, func(o *options) {
				o.driver = c.driver
				if c.mod != nil {
					c.mod(o)
				}
			})
			dir := t.TempDir()
			urf := filepath.Join(dir, "page.urf")
			expect := filepath.Join(dir, "expect.test")
			if err := os.WriteFile(urf, smokePage(c.w, c.h, c.dpi), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(expect, []byte(smokeExpect), 0o644); err != nil {
				t.Fatal(err)
			}
			schemes := []string{"ipp", "ipps"}
			for _, scheme := range schemes {
				uri := scheme + "://" + ts.addr + "/ipp/print"
				out, err := exec.Command(ipptool, "-tv", "-T", "30", "-f", urf, "-d", "filetype=image/urf", "-d", "kind="+c.kind,
					uri, getAttrs, expect, printJob).CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", scheme, err, out)
				}
				if n := strings.Count(string(out), "[PASS]"); n != 3 {
					t.Fatalf("%s: %d tests passed, want 3\n%s", scheme, n, out)
				}
			}
			for id := 1; id <= len(schemes); id++ {
				job := ts.waitJob(t, "ipp", id)
				if st := attrInt(t, job, "job-state"); st != 9 {
					t.Fatalf("job %d: job-state %d, reasons %v, message %v", id, st,
						attrStrings(job, "job-state-reasons"), attrStrings(job, "job-state-message"))
				}
			}
			for n := 1; n <= len(schemes); n++ {
				for _, ext := range []string{"bin", "txt"} {
					p := filepath.Join(ts.dryDir, fmt.Sprintf("job-%d.%s", n, ext))
					if st, err := os.Stat(p); err != nil || st.Size() == 0 {
						t.Fatalf("%s: %v", p, err)
					}
				}
			}
			if bins := binFiles(t, ts.dryDir); len(bins) != len(schemes) {
				t.Fatalf("dry run wrote %v, want one unit per job", bins)
			}
		})
	}
}
