package postscript

import "github.com/leandroalbero/airprint-usb-bridge/internal/driver"

func init() {
	driver.Register(driver.Info{
		Name:        "postscript",
		Description: "PostScript level 2/3 printers (raster images)",
		Media:       "a4 (default), letter, legal, a5, executive, or their PWG names (iso_a4_210x297mm, na_letter_8.5x11in, ...)",
		Options: []driver.Option{
			{Key: "level", Default: "2", Help: "PostScript LanguageLevel: 2 (RunLengthDecode) or 3 (FlateDecode)"},
			{Key: "depth", Default: "1", Help: "image depth: 1 (binarized with --dither) or 8 (gray, halftoned by the printer)"},
			{Key: "encoding", Default: "ascii85", Help: "ascii85 (7-bit clean) or binary (smaller; needs an 8-bit clean channel)"},
			{Key: "resolution", Default: "300", Help: "raster resolution in dpi: 300 or 600"},
			{Key: "duplex", Default: "false", Help: "the printer has a duplex unit"},
			{Key: "passthrough", Default: "false", Help: "also accept application/postscript documents and send them unchanged"},
			{Key: "pjl", Default: "false", Help: "wrap jobs in PJL (UEL, ENTER LANGUAGE = POSTSCRIPT) for printers that switch languages"},
		},
		New: New,
	})
}
