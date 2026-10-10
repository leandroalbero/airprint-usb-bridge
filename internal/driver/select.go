package driver

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
)

var (
	// ErrIPPOverUSB marks a printer with an IPP-over-USB interface: ipp-usb serves it better than
	// any driver here.
	ErrIPPOverUSB = errors.New("printer supports IPP-over-USB; use ipp-usb (https://github.com/OpenPrinting/ipp-usb) instead")
	// ErrNoDriver means auto-selection found nothing; pass --driver.
	ErrNoDriver = errors.New("no driver matches the printer")
)

// Selection is what auto-selection chose and why.
type Selection struct {
	Driver   string
	Model    string // the 1284 model, e.g. "QL-700"; brother-ql picks its profile from it
	Reason   string
	Warnings []string
}

var labelWriter5xx = regexp.MustCompile(`^LABELWRITER\s*5(\d\d|XL)`)

// Select picks a driver from an IEEE 1284 device ID:
//
//	MFG Brother and MDL QL-*            brother-ql
//	MFG DYMO and MDL LabelWriter*        dymo
//	a CMD token with ZPL, or MFG Zebra*  zpl (not when CMD lists only other languages, e.g. EPL)
//	a CMD token starting with PCL        pcl
//	a CMD token POSTSCRIPT* or PS        postscript
//	otherwise                            cups when havePPD, else ErrNoDriver
//
// CMD is compared token by token, case-insensitively. A printer with an IPP-over-USB interface
// (ippUSB) gets ErrIPPOverUSB together with the selection it would otherwise have had.
func Select(id ieee1284.ID, ippUSB, havePPD bool) (Selection, error) {
	sel, err := selectDriver(id, havePPD)
	if ippUSB {
		return sel, ErrIPPOverUSB
	}
	return sel, err
}

func selectDriver(id ieee1284.ID, havePPD bool) (Selection, error) {
	mfg := strings.ToUpper(id.Manufacturer())
	mdl := strings.ToUpper(id.Model())
	model := id.Model()
	switch {
	case strings.Contains(mfg, "BROTHER") && strings.HasPrefix(mdl, "QL-"):
		return Selection{Driver: "brother-ql", Model: model, Reason: "Brother QL label printer"}, nil
	case strings.Contains(mfg, "DYMO") && strings.HasPrefix(mdl, "LABELWRITER"):
		s := Selection{Driver: "dymo", Model: model, Reason: "DYMO LabelWriter"}
		if labelWriter5xx.MatchString(mdl) {
			s.Warnings = append(s.Warnings, "the LabelWriter 5xx series uses a different protocol and authenticated labels; it is not supported")
		}
		return s, nil
	case id.HasCommand(func(t string) bool { return strings.Contains(t, "ZPL") }) ||
		strings.HasPrefix(mfg, "ZEBRA") && len(id.CommandSet()) == 0:
		return Selection{Driver: "zpl", Model: model, Reason: "Zebra / ZPL label printer"}, nil
	case strings.HasPrefix(mfg, "ZEBRA"):
		// An EPL-only printer would print ZPL commands as text.
		if havePPD {
			return Selection{Driver: "cups", Model: model, Reason: "Zebra printer without ZPL; a PPD was given"}, nil
		}
		return Selection{}, fmt.Errorf("%w (device ID %s): its command set has no ZPL, and the zpl driver sends ZPL II only; "+
			"pass --ppd FILE for a CUPS driver (e.g. an EPL2 PPD for rastertolabel), or --driver zpl if it does accept ZPL",
			ErrNoDriver, id.String())
	case id.HasCommand(func(t string) bool { return strings.HasPrefix(t, "PCL") }):
		s := Selection{Driver: "pcl", Model: model, Reason: "command set includes PCL"}
		if !id.HasCommand(func(t string) bool { return strings.HasPrefix(t, "PCL") && !strings.HasPrefix(t, "PCL3") }) {
			s.Warnings = append(s.Warnings, "the printer lists only PCL 3 (GUI), which is vendor-specific; the pcl driver sends PCL 5 raster and may not work")
		}
		return s, nil
	case id.HasCommand(isPostScript):
		return Selection{Driver: "postscript", Model: model, Reason: "command set includes PostScript"}, nil
	case havePPD:
		return Selection{Driver: "cups", Model: model, Reason: "a PPD was given"}, nil
	}
	what := "no IEEE 1284 device ID"
	if !id.Empty() {
		what = "device ID " + id.String()
	}
	return Selection{}, fmt.Errorf("%w (%s): pass --driver %s, or --ppd FILE for a CUPS driver",
		ErrNoDriver, what, strings.Join(Names(), "|"))
}

func isPostScript(t string) bool {
	return t == "PS" || t == "PS2" || t == "PS3" || strings.HasPrefix(t, "POSTSCRIPT")
}
