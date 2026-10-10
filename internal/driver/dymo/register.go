package dymo

import "github.com/leandroalbero/airprint-usb-bridge/internal/driver"

func init() {
	driver.Register(driver.Info{
		Name:        Name,
		Description: "DYMO LabelWriter 400/450/4XL, die-cut and continuous (not the 5xx series)",
		Media:       MediaHelp,
		Options: []driver.Option{
			{Key: "model", Default: "", Help: "printer model when the device ID is unknown, e.g. 450, 450 Turbo, 4XL (default: from the device ID, else 450)"},
			{Key: "density", Default: "normal", Help: "print density: light, medium, normal or dark"},
			{Key: "dot-tab", Default: "0", Help: "bytes (8 dots each) to shift the image right on the print head"},
			{Key: "trim", Default: "true", Help: "continuous paper: cut blank rows from both label ends"},
		},
		New: New,
	})
}
