package ipp

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/brotherql"
)

const privateDir = "../../testdata/private"

// dumpLine is one "name (syntax) = value" line of ipptool -v output.
type dumpLine struct{ name, syntax, value string }

var dumpRe = regexp.MustCompile(`^        ([a-z0-9-]+) \(([^)]*)\) = ?(.*)$`)

// parseDump returns the response attributes of an ipptool -v transcript.
func parseDump(text string) []dumpLine {
	var out []dumpLine
	inResp := false
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if strings.Contains(l, "status-code = ") {
			inResp = true
			continue
		}
		if !inResp {
			continue
		}
		if m := dumpRe.FindStringSubmatch(l); m != nil {
			if m[1] == "attributes-charset" || m[1] == "attributes-natural-language" {
				continue
			}
			out = append(out, dumpLine{m[1], m[2], m[3]})
		}
	}
	return out
}

func lookupDump(lines []dumpLine, name string) string {
	for _, l := range lines {
		if l.name == name {
			return l.value
		}
	}
	return ""
}

var uriHostRe = regexp.MustCompile(`(ipps?|https?)://[^/,]+/`)

// Attributes whose values are generated per host or per moment; ippeveprinter
// derives job-k-octets-supported from free spool space, we from MaxDocumentBytes.
var volatileAttrs = map[string]bool{
	"printer-config-change-date-time": true,
	"printer-current-time":            true,
	"printer-state-change-date-time":  true,
	"printer-up-time":                 true,
	"job-k-octets-supported":          true,
}

// extraAttrs are ours only: the spec requires printer-device-id.
var extraAttrs = map[string]bool{
	"printer-device-id": true,
}

func requireIPPTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ipptool")
	if err != nil {
		t.Skip("ipptool not on PATH")
	}
	return p
}

const getAttrsTest = `{
	NAME "Get printer attributes"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword requested-attributes all,media-col-database
	STATUS successful-ok
}
`

func TestAttributeParityLive(t *testing.T) {
	parityAgainst(t, "get-printer-attributes-live.txt", testConfig())
}

// A2 is the same printer with high-res, a 100 mm preset and zero margins.
func TestAttributeParityHighRes(t *testing.T) {
	cfg := testConfig()
	cfg.Caps = qlCaps(func(o *brotherql.Options) {
		o.HighRes = true
		o.Presets = []int{2900, 5000, 10000}
	})
	cfg.Caps.Roll.Margins = driver.Margins{}
	for i := range cfg.Caps.Media {
		cfg.Caps.Media[i].Margins = driver.Margins{}
	}
	parityAgainst(t, "get-printer-attributes-A2.txt", cfg)
}

// parityAgainst serves cfg with the identity values taken from the capture, so
// none of them have to live in this file.
func parityAgainst(t *testing.T, file string, cfg Config) {
	raw, err := os.ReadFile(filepath.Join(privateDir, file))
	if err != nil {
		t.Skipf("private capture absent: %v", err)
	}
	ipptool := requireIPPTool(t)
	want := parseDump(string(raw))
	if len(want) < 50 {
		t.Fatalf("parsed only %d attributes from %s", len(want), file)
	}
	cfg.Name = lookupDump(want, "printer-name")
	cfg.Location = lookupDump(want, "printer-location")
	cfg.Caps.MakeModel = lookupDump(want, "printer-make-and-model")
	cfg.UUID = lookupDump(want, "printer-uuid")
	ts := startServer(t, cfg, (&recorder{}).process)

	testFile := filepath.Join(t.TempDir(), "get-attrs.test")
	if err := os.WriteFile(testFile, []byte(getAttrsTest), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"ipp", "ipps"} {
		out, err := exec.Command(ipptool, "-tv", ts.uri(scheme), testFile).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: ipptool: %v\n%s", scheme, err, out)
		}
		got := parseDump(string(out))
		diffs, allowed := compareDumps(want, got)
		for _, d := range allowed {
			t.Logf("%s allowed: %s", scheme, d)
		}
		for _, d := range diffs {
			t.Errorf("%s: %s", scheme, d)
		}
	}
}

func compareDumps(want, got []dumpLine) (diffs, allowed []string) {
	norm := func(l dumpLine) string {
		return l.name + " (" + l.syntax + ") = " + uriHostRe.ReplaceAllString(l.value, "$1://HOST/")
	}
	var g []dumpLine
	for _, l := range got {
		if extraAttrs[l.name] {
			allowed = append(allowed, "extra "+l.name+" = "+l.value)
			continue
		}
		g = append(g, l)
	}
	for i := 0; i < len(want) || i < len(g); i++ {
		switch {
		case i >= len(want):
			diffs = append(diffs, "unexpected "+norm(g[i]))
		case i >= len(g):
			diffs = append(diffs, "missing "+norm(want[i]))
		case want[i].name != g[i].name:
			diffs = append(diffs, "order: want "+want[i].name+", got "+g[i].name)
		case norm(want[i]) == norm(g[i]):
			if want[i].value != g[i].value {
				allowed = append(allowed, "host-specific URI "+want[i].name)
			}
		case volatileAttrs[want[i].name] && want[i].syntax == g[i].syntax:
			allowed = append(allowed, want[i].name+": "+want[i].value+" vs "+g[i].value)
		default:
			diffs = append(diffs, "want "+norm(want[i])+"\n\tgot  "+norm(g[i]))
		}
	}
	return diffs, allowed
}
