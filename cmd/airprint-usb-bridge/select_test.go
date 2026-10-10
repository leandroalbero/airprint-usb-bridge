package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
)

func usbPrinter(node, id string, ippUSB bool) backend.USBPrinter {
	p := backend.USBPrinter{Node: node, DeviceID: ieee1284.Parse(id), IPPUSB: ippUSB}
	if node != "" {
		p.Path = "/dev/usb/" + node
	}
	return p
}

func TestChooseDriver(t *testing.T) {
	ql := usbPrinter("lp0", "MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;", false)
	ql.VendorID, ql.ProductID = 0x04f9, 0x2042
	hp := usbPrinter("lp1", "MFG:HP;CMD:PJL,PCL,URF;MDL:LaserJet Pro M404;", true)
	zebra := usbPrinter("lp2", "MFG:Zebra Technologies;CMD:ZPL;MDL:ZD420;", false)
	canon := usbPrinter("", "", true)
	canon.Manufacturer, canon.Product = "Canon", "PIXMA"
	list := []backend.USBPrinter{ql, hp, zebra, canon}
	laser := usbPrinter("lp0", "MFG:HP;CMD:PJL,PCL,POSTSCRIPT;MDL:LaserJet 1320;", false)
	ql1 := ql
	ql1.Node, ql1.Path = "lp1", "/dev/usb/lp1"
	laserFirst := []backend.USBPrinter{laser, ql1}
	laser1 := usbPrinter("lp1", "MFG:HP;CMD:PJL,PCL,POSTSCRIPT;MDL:LaserJet 1320;", false)

	cases := []struct {
		name          string
		device, match string
		dry, drv      string
		list          []backend.USBPrinter
		want          string
		wantErr       error
		log           string
		wantPID       uint16
		wantNode      string
	}{
		{name: "auto picks the first", device: "auto", list: list, want: "brother-ql", log: "driver auto: brother-ql", wantPID: 0x2042},
		{name: "match", device: "auto", match: "zebra", list: list, want: "zpl"},
		{name: "path", device: "/dev/usb/lp2", list: list, want: "zpl"},
		{name: "ipp-usb refused", device: "/dev/usb/lp1", list: list, wantErr: driver.ErrIPPOverUSB},
		{name: "ipp-usb forced", device: "/dev/usb/lp1", drv: "pcl", list: list, want: "pcl", log: "ipp-usb"},
		{name: "ipp-usb first is skipped", device: "auto", list: []backend.USBPrinter{hp, zebra}, want: "zpl", log: "LaserJet Pro M404 supports IPP-over-USB"},
		{name: "ipp-usb matched is skipped", device: "auto", match: "hp", list: list, want: "brother-ql", log: "no USB printer"},
		{name: "ipp-usb only warns", device: "auto", list: []backend.USBPrinter{canon}, want: "brother-ql", log: "Canon PIXMA supports IPP-over-USB"},
		{name: "nothing plugged in", device: "auto", want: "brother-ql", log: "no USB printer"},
		{name: "dry run", device: "auto", dry: "/tmp/x", list: list, want: "brother-ql", log: "dry-run target"},
		{name: "explicit", device: "auto", drv: "zpl", list: list, want: "zpl", wantNode: "lp2"},
		{name: "explicit, no printer for it", device: "auto", drv: "pcl", list: []backend.USBPrinter{zebra}, want: "pcl", wantNode: "lp2"},
		{name: "auto, laser before QL", device: "auto", list: laserFirst, want: "pcl", wantNode: "lp0", log: "pass --match MFG:MDL"},
		{name: "brother-ql, laser before QL", device: "auto", drv: "brother-ql", list: laserFirst, want: "brother-ql", wantNode: "lp1", wantPID: 0x2042, log: "using /dev/usb/lp1"},
		{name: "brother-ql, no QL", device: "auto", drv: "brother-ql", list: []backend.USBPrinter{laser, zebra}, want: "brother-ql", log: "using the first that answers as a Brother QL"},
		{name: "pcl, QL before laser", device: "auto", drv: "pcl", list: []backend.USBPrinter{ql, laser1}, want: "pcl", wantNode: "lp1", log: "/dev/usb/lp1 HP LaserJet 1320"},
		{name: "unknown printer", device: "auto", list: []backend.USBPrinter{usbPrinter("lp0", "MFG:EPSON;CMD:ESCPL2;MDL:Stylus;", false)}, wantErr: driver.ErrNoDriver},
		{name: "no device id", device: "auto", list: []backend.USBPrinter{usbPrinter("lp0", "", false)}, want: "brother-ql", log: "reports no device ID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := defaultOptions(t)
			if c.drv != "" {
				o.driver = c.drv
			}
			target, err := backend.ParseTarget(c.device, c.dry, c.match)
			if err != nil {
				t.Fatal(err)
			}
			var logs []string
			logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			got, dev, err := chooseDriverFrom(&o, target, logf, func() ([]backend.USBPrinter, error) { return c.list, nil })
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
			if dev.ProductID != c.wantPID {
				t.Fatalf("usb product id %04x, want %04x", dev.ProductID, c.wantPID)
			}
			if c.wantNode != "" && dev.DeviceID.Raw != pickNode(c.list, c.wantNode).DeviceID.Raw {
				t.Fatalf("chose %s (%s), want %s", dev.Node, dev.Name(), c.wantNode)
			}
			// The generic transport picks again for each job; it must land on the same printer.
			if pinned := pinTarget(target, dev); c.wantNode != "" && got != "brother-ql" {
				p, err := backend.PickFunc(c.list, pinned.Match, pinned.Accepts)
				if err != nil || p.DeviceID.Raw != dev.DeviceID.Raw {
					t.Fatalf("per-job pick %s (%s), %v; want %s", p.Node, p.Name(), err, dev.Name())
				}
			}
			if c.log != "" && !strings.Contains(strings.Join(logs, "\n"), c.log) {
				t.Fatalf("logs %q lack %q", logs, c.log)
			}
		})
	}
}

func pickNode(list []backend.USBPrinter, node string) backend.USBPrinter {
	for _, p := range list {
		if p.Node == node {
			return p
		}
	}
	return backend.USBPrinter{}
}

func TestDriverFlagErrors(t *testing.T) {
	dry := t.TempDir()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--driver", "nope", "--dry-run", dry, "status"}, "unknown driver"},
		{[]string{"--driver", "cups", "--dry-run", dry, "status"}, "--ppd"},
		{[]string{"--driver", "pcl", "--media", "a3", "--dry-run", dry, "status"}, "a3"},
		{[]string{"--driver-opt", "colour=red", "--dry-run", dry, "status"}, "no option"},
		{[]string{"--driver-opt", "presets", "--dry-run", dry, "status"}, "key=value"},
		{[]string{"--device", "lp0", "status"}, "want auto"},
	} {
		_, errOut, code := runCLI(t, c.args...)
		if code == 0 || !strings.Contains(errOut, c.want) {
			t.Errorf("%q: exit %d %q", c.args, code, errOut)
		}
	}
	out, errOut, code := runCLI(t, "--driver-opt", "presets=40", "--dry-run", dry, "status")
	if code != 0 || !strings.Contains(out, "driver:         brother-ql") {
		t.Fatalf("exit %d %q %q", code, out, errOut)
	}
}

func TestListPrinters(t *testing.T) {
	ql := usbPrinter("lp0", "MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;", false)
	ql.VendorID, ql.ProductID, ql.Interface = 0x04f9, 0x2042, "1-1:1.0"
	hp := usbPrinter("lp1", "MFG:HP;CMD:PJL,PCL,URF;MDL:LaserJet Pro M404;", true)
	lp2844 := usbPrinter("lp2", "MFG:Zebra;CMD:EPL;MDL:LP2844;", false)
	unbound := usbPrinter("", "MFG:DYMO;MDL:LabelWriter 5XL;", false)
	unbound.Manufacturer = "DYMO"
	anon := usbPrinter("lp3", "", false)
	list := []backend.USBPrinter{ql, hp, lp2844, unbound, anon}

	o := defaultOptions(t)
	var out strings.Builder
	if err := runListPrinters(&o, &out, func() ([]backend.USBPrinter, error) { return list, nil }); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"/dev/usb/lp0: Brother QL-700\n  usb:       04f9:2042 (interface 1-1:1.0)\n  device-id: MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;\n  driver:    brother-ql (Brother QL label printer)\n",
		"/dev/usb/lp1: HP LaserJet Pro M404\n", "driver:    none: IPP-over-USB printer, use ipp-usb\n",
		"driver:    none: no driver matches the printer (device ID MFG:Zebra;MDL:LP2844;CMD:EPL;)",
		"(no usblp node): DYMO LabelWriter 5XL\n", "dymo (DYMO LabelWriter); the LabelWriter 5xx series",
		"usblp driver is not bound",
		"/dev/usb/lp3: USB 0000:0000\n  usb:       0000:0000\n  device-id: (none)\n  driver:    unknown (no device ID); pass --driver\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}

	o.ppd = "x.ppd"
	out.Reset()
	if err := runListPrinters(&o, &out, func() ([]backend.USBPrinter, error) { return []backend.USBPrinter{lp2844}, nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "driver:    cups (") {
		t.Errorf("with --ppd: %s", out.String())
	}

	out.Reset()
	if err := runListPrinters(&o, &out, func() ([]backend.USBPrinter, error) { return nil, nil }); err != nil || !strings.HasPrefix(out.String(), "no USB printers found\n") {
		t.Errorf("empty: %v %q", err, out.String())
	}
	if err := runListPrinters(&o, &out, func() ([]backend.USBPrinter, error) { return nil, errors.New("boom") }); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("discovery error: %v", err)
	}
}

func TestDriversCommand(t *testing.T) {
	out, errOut, code := runCLI(t, "drivers")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"brother-ql: verified on QL-700",
		"pcl: unverified", "postscript: unverified", "cups: unverified", "dymo: unverified", "zpl: unverified",
		"--driver-opt profile=...", "ql1110nwb", "--driver-opt dpi=... (default 203)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drivers output lacks %q", want)
		}
	}
	for _, info := range driver.Registered() {
		if info.Verified != "" && info.Name != "brother-ql" {
			t.Errorf("%s claims hardware verification: %s", info.Name, info.Verified)
		}
	}
}
