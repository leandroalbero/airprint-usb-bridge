package cups

import (
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
)

func init() {
	driver.Register(driver.Info{
		Name:        "cups",
		Description: "any CUPS raster driver run without cupsd (needs --ppd)",
		Media:       "a PageSize from the PPD, by its PPD name (A4, w288h432) or PWG name; default: the PPD's",
		Options: []driver.Option{
			{Key: "options", Help: `PPD options fixed for every job, "Key=Choice Key2=Choice2" (e.g. "Darkness=10 InputSlot=Tray2")`},
			{Key: "filter", Help: "filter program (name or absolute path); default: the PPD's *cupsFilter2/*cupsFilter for raster, \"-\" sends the raster itself"},
			{Key: "filter-dir", Default: strings.Join(DefaultFilterDirs, ":"), Help: "colon-separated directories searched for the filter"},
			{Key: "timeout", Default: defaultTimeout.String(), Help: "longest a filter may go without reading input or writing output"},
			{Key: "max-output", Default: defaultMaxOutput, Help: "most filter output sent for one job (K, M, G suffixes); it streams to the printer"},
			{Key: "kind", Default: "auto", Help: "auto, document or label (auto: label when the filter or model name says label)"},
			{Key: "duplex", Default: "auto", Help: "auto (when the PPD has Duplex choices), true or false (no duplexer fitted)"},
			{Key: "full-page", Default: "false", Help: "send the whole page instead of the PPD's imageable area"},
			{Key: "device-uri", Default: defaultDeviceURI, Help: "DEVICE_URI given to the filter"},
		},
		New: New,
	})
}
