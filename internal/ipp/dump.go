package ipp

import (
	"fmt"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
)

// DumpHost is the host the per-request URIs of PrinterAttributes name.
const DumpHost = "printer.local:8631"

// PrinterAttributes returns what Get-Printer-Attributes with requested-attributes
// "all,media-col-database" answers for cfg, as an idle printer reached as DumpHost at a fixed
// time, without starting a server. Name and UUID get placeholders when empty. It is meant for
// golden tests of driver Capabilities.
func PrinterAttributes(cfg Config) (goipp.Attributes, error) {
	if cfg.UUID == "" {
		cfg.UUID = "urn:uuid:00000000-0000-4000-8000-000000000000"
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Caps.MakeModel
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v := &view{host: DumpHost, now: t, status: printerStatus{
		state: StateIdle, reasons: []string{"none"}, message: "Idle.", changed: t, configTime: t,
	}}
	want := requested{all: true, names: map[string]bool{"media-col-database": true}}
	var out goipp.Attributes
	for _, pa := range buildPrinterAttrs(&cfg, int(cfg.MaxDocumentBytes/1024)) {
		if !want.wants(pa.name, pa.group) {
			continue
		}
		if pa.dyn != nil {
			out.Add(pa.dyn(v))
		} else {
			out.Add(pa.attr)
		}
	}
	return out, nil
}

// FormatAttributes renders attributes one per line as "name (syntax) = value", in the spirit of
// ipptool -v, for golden files and diffs.
func FormatAttributes(attrs goipp.Attributes) string {
	var b strings.Builder
	for _, a := range attrs {
		syntax := "no-value"
		if len(a.Values) > 0 {
			syntax = a.Values[0].T.String()
			if len(a.Values) > 1 {
				syntax = "1setOf " + syntax
			}
		}
		vals := make([]string, len(a.Values))
		for i, v := range a.Values {
			vals[i] = formatValue(v.V)
		}
		fmt.Fprintf(&b, "%s (%s) = %s\n", a.Name, syntax, strings.Join(vals, ","))
	}
	return b.String()
}

func formatValue(v goipp.Value) string {
	switch x := v.(type) {
	case goipp.Collection:
		parts := make([]string, len(x))
		for i, m := range x {
			vals := make([]string, len(m.Values))
			for k, mv := range m.Values {
				vals[k] = formatValue(mv.V)
			}
			parts[i] = m.Name + "=" + strings.Join(vals, ",")
		}
		return "{" + strings.Join(parts, " ") + "}"
	case goipp.Binary:
		return string(x)
	}
	return v.String()
}
