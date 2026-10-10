package main

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
)

// runDrivers lists the registered drivers with their verification status, media and options.
func runDrivers(out io.Writer) error {
	for i, info := range driver.Registered() {
		if i > 0 {
			fmt.Fprintln(out)
		}
		verified := "unverified (not tested on hardware)"
		if info.Verified != "" {
			verified = "verified on " + info.Verified
		}
		fmt.Fprintf(out, "%s: %s\n", info.Name, verified)
		if info.Description != "" {
			fmt.Fprintf(out, "  %s\n", info.Description)
		}
		if info.Media != "" {
			fmt.Fprintf(out, "  --media: %s\n", info.Media)
		}
		for _, o := range info.Options {
			def := ""
			if o.Default != "" {
				def = " (default " + o.Default + ")"
			}
			fmt.Fprintf(out, "  --driver-opt %s=...%s: %s\n", o.Key, def, o.Help)
		}
	}
	return nil
}

// runListPrinters prints every USB printer discovery finds, with its IEEE 1284 ID and the driver
// --driver auto would choose for it.
func runListPrinters(o *options, out io.Writer, discover func() ([]backend.USBPrinter, error)) error {
	list, err := discover()
	if err != nil {
		return fmt.Errorf("usb discovery: %w", err)
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "no USB printers found")
		if runtime.GOOS != "linux" {
			fmt.Fprintf(out, "(USB discovery reads Linux sysfs; it finds nothing on %s)\n", runtime.GOOS)
		}
		return nil
	}
	for i, p := range list {
		if i > 0 {
			fmt.Fprintln(out)
		}
		node := p.Path
		if node == "" {
			node = "(no usblp node)"
		}
		fmt.Fprintf(out, "%s: %s\n", node, p.Name())
		fmt.Fprintf(out, "  usb:       %04x:%04x", p.VendorID, p.ProductID)
		if p.Interface != "" {
			fmt.Fprintf(out, " (interface %s)", p.Interface)
		}
		fmt.Fprintln(out)
		id := "(none)"
		if !p.DeviceID.Empty() {
			id = p.DeviceID.Raw
		}
		fmt.Fprintf(out, "  device-id: %s\n", id)
		fmt.Fprintf(out, "  driver:    %s\n", suggestDriver(p, o.ppd != ""))
	}
	return nil
}

func suggestDriver(p backend.USBPrinter, havePPD bool) string {
	if p.DeviceID.Empty() && !p.IPPUSB {
		return "unknown (no device ID); pass --driver"
	}
	sel, err := driver.Select(p.DeviceID, p.IPPUSB, havePPD)
	var notes []string
	notes = append(notes, sel.Warnings...)
	switch {
	case errors.Is(err, driver.ErrIPPOverUSB):
		return "none: IPP-over-USB printer, use ipp-usb"
	case err != nil:
		return "none: " + err.Error()
	}
	s := sel.Driver + " (" + sel.Reason + ")"
	if p.Path == "" {
		notes = append(notes, "no usblp node: the kernel's usblp driver is not bound to it")
	}
	if len(notes) > 0 {
		s += "; " + strings.Join(notes, "; ")
	}
	return s
}
