package pcl

import "github.com/leandroalbero/airprint-usb-bridge/internal/driver"

func init() {
	driver.Register(driver.Info{
		Name:        "pcl",
		Description: "PCL 5 lasers (raster graphics, PJL; PCL 5c colour with --color on)",
		Media:       mediaHelp() + "; default a4",
		Options: []driver.Option{
			{Key: "dpi", Default: "300", Help: "raster resolution: 300, 600, or both (300,600 or 600,300; the first is the default, draft prints at 300 and high at 600). 600 needs a 600 dpi printer, and clients then send 600 dpi pages: about 35 MB each for A4, four times the work of 300 dpi"},
			{Key: "duplex", Default: "false", Help: "the printer has a duplex unit: offer two-sided printing"},
			{Key: "compression", Default: "2", Help: "raster compression: 2 (TIFF PackBits) or 3 (delta row); colour pages always use 3"},
			{Key: "pjl", Default: "true", Help: "wrap the job in PJL (UEL, JOB, ENTER LANGUAGE=PCL); false for printers without PJL"},
		},
		New: New,
	})
}
