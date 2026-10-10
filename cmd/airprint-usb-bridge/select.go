package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/brotherql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
)

// legacyDriver is used when --driver is auto and no USB printer can be examined (dry runs,
// TCP, a printer that is switched off): earlier releases only drove the Brother QL.
const legacyDriver = brotherql.Name

// chooseDriver resolves --driver. With auto it reads the IEEE 1284 ID of the USB printer the
// target names and applies driver.Select. It also returns that printer, or a zero USBPrinter
// when none could be examined.
func chooseDriver(o *options, t backend.Target, logf logFunc) (string, backend.USBPrinter, error) {
	return chooseDriverFrom(o, t, logf, backend.DiscoverSystem)
}

func chooseDriverFrom(o *options, t backend.Target, logf logFunc, discover func() ([]backend.USBPrinter, error)) (string, backend.USBPrinter, error) {
	explicit := o.driver != "" && o.driver != "auto"
	var found *backend.USBPrinter
	var ippOnly []backend.USBPrinter
	if t.Kind == backend.TargetUSB {
		list, err := discover()
		if err != nil {
			logf("usb discovery: %v", err)
		}
		var usable []*backend.USBPrinter
		for i := range list {
			p := &list[i]
			switch {
			case t.Device == p.Path && p.Path != "":
				found = p
			case t.Device != "auto" || !p.Matches(t.Match):
			case p.IPPUSB:
				// Auto skips these, as backend.Pick does when it re-picks per job.
				ippOnly = append(ippOnly, *p)
			case p.Node != "":
				usable = append(usable, p)
			}
		}
		if t.Device == "auto" {
			found = pickAuto(o, usable, explicit, logf)
		}
		if found == nil && t.Device != "auto" {
			if id, err := backend.DeviceIDForPath(t.Device); err == nil {
				found = &backend.USBPrinter{Path: t.Device, DeviceID: id}
			}
		}
	}
	for _, p := range ippOnly {
		logf("warning: %s supports IPP-over-USB only through ipp-usb; it is not driven here", p.Name())
	}
	var id ieee1284.ID
	var dev backend.USBPrinter
	if found != nil {
		dev = *found
		id = found.DeviceID
		logf("printer %s: %s (device ID %q)", found.Path, found.Name(), id.Raw)
		if found.IPPUSB {
			if !explicit {
				return "", dev, fmt.Errorf("%s: %w; or pass --driver to drive it anyway", found.Name(), driver.ErrIPPOverUSB)
			}
			logf("warning: %s: %v", found.Name(), driver.ErrIPPOverUSB)
		}
	}
	if explicit {
		return o.driver, dev, nil
	}
	if found == nil || id.Empty() {
		why := "no USB printer to examine"
		switch {
		case t.Kind != backend.TargetUSB:
			why = "the " + string(t.Kind) + " target has no device ID"
		case found != nil:
			why = found.Path + " reports no device ID"
		}
		logf("driver auto: %s; using %s (pass --driver to choose)", why, legacyDriver)
		return legacyDriver, dev, nil
	}
	sel, err := driver.Select(id, false, o.ppd != "")
	for _, w := range sel.Warnings {
		logf("warning: %s", w)
	}
	if err != nil {
		return "", dev, err
	}
	if _, ok := driver.Lookup(sel.Driver); !ok {
		return "", dev, fmt.Errorf("%s needs the %s driver, which this build lacks: %w", found.Name(), sel.Driver, errors.ErrUnsupported)
	}
	logf("driver auto: %s (%s)", sel.Driver, sel.Reason)
	return sel.Driver, dev, nil
}

// pickAuto chooses among the printers --device auto may use. An explicit driver takes the first
// printer auto-selection would give that driver, so a second printer on the bus does not hide the
// one meant. brother-ql settles for none, because its sender finds a QL by probing each node.
func pickAuto(o *options, usable []*backend.USBPrinter, explicit bool, logf logFunc) *backend.USBPrinter {
	if len(usable) == 0 {
		return nil
	}
	pick := usable[0]
	if explicit {
		pick = nil
		for _, p := range usable {
			if sel, err := driver.Select(p.DeviceID, false, o.ppd != ""); err == nil && sel.Driver == o.driver {
				pick = p
				break
			}
		}
		if pick == nil && o.driver != brotherql.Name {
			pick = usable[0]
		}
	}
	if len(usable) > 1 {
		names := make([]string, len(usable))
		for i, p := range usable {
			names[i] = p.Path + " " + p.Name()
		}
		using := "the first that answers as a Brother QL"
		if pick != nil {
			using = pick.Path
		}
		logf("warning: %d USB printers found (%s); using %s; pass --match MFG:MDL to choose", len(usable), strings.Join(names, ", "), using)
	}
	return pick
}

// pinTarget makes --device auto keep sending to the printer the driver was chosen for: the generic
// USB transport picks a printer again for each job.
func pinTarget(t backend.Target, dev backend.USBPrinter) backend.Target {
	if t.Kind == backend.TargetUSB && t.Device == "auto" && dev.Node != "" {
		t.Pin = dev.Key()
	}
	return t
}
