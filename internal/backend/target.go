package backend

import (
	"fmt"
	"net"
	"strings"
)

type TargetKind string

const (
	TargetUSB    TargetKind = "usb"
	TargetTCP    TargetKind = "tcp"
	TargetDryRun TargetKind = "dry-run"
)

// Target says where printer bytes go, from --device, --dry-run and --match.
type Target struct {
	Kind   TargetKind
	Device string // TargetUSB: "auto" or a usblp node such as /dev/usb/lp0
	Addr   string // TargetTCP: host:port
	Dir    string // TargetDryRun
	Match  string // TargetUSB with Device "auto": "MFG:MDL" substrings, see ieee1284.MatchNames
	// Pin, when set, limits Device "auto" to printers with this key when it picks one for each
	// job, so units keep going to the printer the driver was chosen for.
	Pin PrinterKey
}

// ParseTarget turns the command-line values into a Target. A dry-run directory wins over the device.
func ParseTarget(device, dryRunDir, match string) (Target, error) {
	switch {
	case dryRunDir != "":
		return Target{Kind: TargetDryRun, Dir: dryRunDir}, nil
	case strings.HasPrefix(device, "tcp://"):
		return Target{Kind: TargetTCP, Addr: TCPAddr(device)}, nil
	case device == "" || device == "auto":
		return Target{Kind: TargetUSB, Device: "auto", Match: match}, nil
	case strings.HasPrefix(device, "/"):
		return Target{Kind: TargetUSB, Device: device, Match: match}, nil
	}
	return Target{}, fmt.Errorf("device %q: want auto, a /dev/usb/lpN path or tcp://host:port", device)
}

// Accepts reports whether p has the target's Pin, or true when there is none.
func (t Target) Accepts(p USBPrinter) bool {
	return t.Pin == (PrinterKey{}) || p.Key() == t.Pin
}

func (t Target) String() string {
	switch t.Kind {
	case TargetDryRun:
		return "dry-run " + t.Dir
	case TargetTCP:
		return "tcp://" + t.Addr
	case TargetUSB:
		if t.Device == "auto" && t.Match != "" {
			return "auto (match " + t.Match + ")"
		}
		return t.Device
	}
	return string(t.Kind)
}

// TCPAddr normalises host, host:port or tcp://host:port[/] to host:port, port 9100 by default.
func TCPAddr(addr string) string {
	addr = strings.TrimPrefix(addr, "tcp://")
	addr = strings.TrimSuffix(addr, "/")
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), "9100")
}

// Open returns the generic transport for t. inspect, when non-nil, is used by the dry-run
// transport to write a human-readable job-<n>.txt next to each job-<n>.bin.
func Open(t Target, inspect func([]byte) (string, error)) (Transport, error) {
	switch t.Kind {
	case TargetDryRun:
		return NewDryRun(t.Dir, inspect), nil
	case TargetTCP:
		return NewTCP(t.Addr), nil
	case TargetUSB:
		var accept func(USBPrinter) bool
		if t.Pin != (PrinterKey{}) {
			accept = t.Accepts
		}
		return newUSB(t.Device, t.Match, accept), nil
	}
	return nil, fmt.Errorf("unknown target kind %q", t.Kind)
}
