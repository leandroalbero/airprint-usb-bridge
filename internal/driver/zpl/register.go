package zpl

import "github.com/leandroalbero/airprint-usb-bridge/internal/driver"

func init() {
	driver.Register(driver.Info{
		Name:        Name,
		Description: "Zebra ZPL II label printers (^GFA raster), die-cut, black-mark or continuous media",
		Media: "a label size in inches or mm: 4x6 (default), 4x4, 4x3, 4x2, 4x1, 3x2, 3x1, 2.25x1.25, 2x1, " +
			"or any WxH[in|mm] such as 100x150mm; continuous (4 in roll) or continuous-W[in|mm] such as continuous-57mm",
		Options: []driver.Option{
			{Key: "dpi", Default: "203", Help: "print head resolution: 203, 300 or 600"},
			{Key: "darkness", Default: "", Help: "~SD darkness 0-30; empty keeps the printer's setting"},
			{Key: "tracking", Default: TrackGap, Help: "media sensing for labels: gap (web/gap, ^MNY) or mark (black mark, ^MNM); continuous media always uses ^MNN"},
			{Key: "encoding", Default: EncodingZ64, Help: "^GFA data: z64 (zlib + base64 + CRC) or hex (ZPL ASCII hex compression, the fallback if a printer rejects z64)"},
			{Key: "print-width", Default: "", Help: "widest label in dots; default the 4 in head: 832 at 203 dpi, 1248 at 300, 2496 at 600"},
		},
		New: func(c driver.Config) (driver.Driver, error) { return NewFromConfig(c) },
	})
}
