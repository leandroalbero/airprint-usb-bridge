package backend

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
)

// USBPrinter is one printer found in sysfs.
type USBPrinter struct {
	Node         string // usblp node name, e.g. "lp0"; empty when no usblp driver is bound
	Path         string // "/dev/usb/" + Node, or empty
	Interface    string // sysfs interface name, e.g. "1-1.2:1.0"
	DeviceID     ieee1284.ID
	VendorID     uint16
	ProductID    uint16
	Manufacturer string // USB string descriptors, when the kernel exposes them
	Product      string
	// IPPUSB is set when the device has an IPP-over-USB interface (class 7, protocol 4). Such
	// printers belong to ipp-usb; the bridge warns instead of driving them.
	IPPUSB bool
}

// Name returns the best human-readable make and model.
func (p USBPrinter) Name() string {
	mfg, mdl := p.DeviceID.Manufacturer(), p.DeviceID.Model()
	if mfg == "" {
		mfg = p.Manufacturer
	}
	if mdl == "" {
		mdl = p.Product
	}
	switch {
	case mfg == "" && mdl == "":
		return fmt.Sprintf("USB %04x:%04x", p.VendorID, p.ProductID)
	case mfg == "" || strings.HasPrefix(strings.ToLower(mdl), strings.ToLower(mfg)):
		return mdl
	}
	return mfg + " " + mdl
}

// PrinterKey tells a printer model apart from others on the bus: the 1284 manufacturer and model,
// or the USB vendor and product ID when the printer reports no device ID.
type PrinterKey struct {
	Manufacturer, Model string
	VendorID, ProductID uint16
}

// Key returns the printer's PrinterKey; it is zero when nothing identifies the printer.
func (p USBPrinter) Key() PrinterKey {
	if !p.DeviceID.Empty() {
		return PrinterKey{Manufacturer: p.DeviceID.Manufacturer(), Model: p.DeviceID.Model()}
	}
	return PrinterKey{VendorID: p.VendorID, ProductID: p.ProductID}
}

// Matches applies a --match pattern to the 1284 ID, or to the USB strings when there is none.
func (p USBPrinter) Matches(pattern string) bool {
	if !p.DeviceID.Empty() {
		return p.DeviceID.Match(pattern)
	}
	return ieee1284.MatchNames(p.Manufacturer, p.Product, pattern)
}

const usbPrinterClass = "07"

// Discover lists USB printers under a sysfs root (os.DirFS("/sys") on a live system). It walks
// bus/usb/devices for printer-class interfaces, then adds any class/usbmisc/lp* node the walk did
// not reach. The result is ordered by node name; interfaces without a node come last.
func Discover(sys fs.FS) ([]USBPrinter, error) {
	const devices = "bus/usb/devices"
	ents, err := fs.ReadDir(sys, devices)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	type usbDev struct {
		ippUSB bool
		ifaces []string
	}
	devs := map[string]*usbDev{}
	var order []string
	for _, e := range ents {
		name := e.Name()
		dev, _, ok := strings.Cut(name, ":")
		if !ok {
			continue
		}
		dir := path.Join(devices, name)
		if readAttr(sys, dir, "bInterfaceClass") != usbPrinterClass {
			continue
		}
		d := devs[dev]
		if d == nil {
			d = &usbDev{}
			devs[dev] = d
			order = append(order, dev)
		}
		if readAttr(sys, dir, "bInterfaceProtocol") == "04" {
			d.ippUSB = true
			continue
		}
		d.ifaces = append(d.ifaces, name)
	}

	var out []USBPrinter
	seen := map[string]bool{}
	for _, dev := range order {
		d := devs[dev]
		devDir := path.Join(devices, dev)
		base := USBPrinter{
			VendorID:     readHex16(sys, devDir, "idVendor"),
			ProductID:    readHex16(sys, devDir, "idProduct"),
			Manufacturer: readAttr(sys, devDir, "manufacturer"),
			Product:      readAttr(sys, devDir, "product"),
			IPPUSB:       d.ippUSB,
		}
		found := false
		for _, iface := range d.ifaces {
			dir := path.Join(devices, iface)
			id := ieee1284.Parse(readAttr(sys, dir, "ieee1284_id"))
			nodes, _ := fs.ReadDir(sys, path.Join(dir, "usbmisc"))
			for _, n := range nodes {
				if !strings.HasPrefix(n.Name(), "lp") {
					continue
				}
				p := base
				p.Node, p.Path, p.Interface, p.DeviceID = n.Name(), "/dev/usb/"+n.Name(), iface, id
				out = append(out, p)
				seen[n.Name()] = true
				found = true
			}
		}
		if !found {
			p := base
			if len(d.ifaces) > 0 {
				p.Interface = d.ifaces[0]
			} else {
				p.Interface = dev
			}
			out = append(out, p)
		}
	}

	for _, class := range []string{"class/usbmisc", "class/usb"} {
		nodes, _ := fs.ReadDir(sys, class)
		for _, n := range nodes {
			name := n.Name()
			if !strings.HasPrefix(name, "lp") || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, USBPrinter{
				Node:     name,
				Path:     "/dev/usb/" + name,
				DeviceID: ieee1284.Parse(readAttr(sys, path.Join(class, name, "device"), "ieee1284_id")),
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Node, out[j].Node
		if (a == "") != (b == "") {
			return b == ""
		}
		return nodeNumber(a) < nodeNumber(b)
	})
	return out, nil
}

func nodeNumber(node string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(node, "lp"))
	if err != nil {
		return 1 << 30
	}
	return n
}

// NodeDeviceID reads the 1284 ID of one usblp node ("lp0" or "/dev/usb/lp0") from sysfs.
func NodeDeviceID(sys fs.FS, node string) (ieee1284.ID, bool) {
	node = path.Base(node)
	for _, class := range []string{"class/usbmisc", "class/usb"} {
		if s := readAttr(sys, path.Join(class, node, "device"), "ieee1284_id"); s != "" {
			return ieee1284.Parse(s), true
		}
	}
	return ieee1284.ID{}, false
}

// Pick returns the first printer with a usblp node that matches pattern, skipping printers with
// an IPP-over-USB interface: those belong to ipp-usb.
func Pick(list []USBPrinter, pattern string) (USBPrinter, error) {
	return PickFunc(list, pattern, nil)
}

// PickFunc is Pick that also requires accept(p) when accept is non-nil.
func PickFunc(list []USBPrinter, pattern string, accept func(USBPrinter) bool) (USBPrinter, error) {
	for _, p := range list {
		if p.Node != "" && !p.IPPUSB && p.Matches(pattern) && (accept == nil || accept(p)) {
			return p, nil
		}
	}
	if accept != nil {
		return USBPrinter{}, fmt.Errorf("the USB printer the driver was chosen for is not connected: %w", ErrNoPrinter)
	}
	if pattern != "" {
		return USBPrinter{}, fmt.Errorf("no USB printer matches %q: %w", pattern, ErrNoPrinter)
	}
	return USBPrinter{}, fmt.Errorf("no USB printer found: %w", ErrNoPrinter)
}

// DiscoverSystem is Discover on /sys, with the LPIOC_GET_DEVICE_ID ioctl as a fallback for
// nodes whose sysfs entry has no ieee1284_id.
func DiscoverSystem() ([]USBPrinter, error) {
	list, err := Discover(os.DirFS("/sys"))
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Path != "" && list[i].DeviceID.Empty() {
			if s, err := ioctlDeviceID(list[i].Path); err == nil && s != "" {
				list[i].DeviceID = ieee1284.Parse(s)
			}
		}
	}
	return list, nil
}

// DeviceIDForPath returns the 1284 ID of a usblp node: sysfs first, then the ioctl.
func DeviceIDForPath(devPath string) (ieee1284.ID, error) {
	if id, ok := NodeDeviceID(os.DirFS("/sys"), devPath); ok {
		return id, nil
	}
	s, err := ioctlDeviceID(devPath)
	if err != nil {
		return ieee1284.ID{}, err
	}
	return ieee1284.Parse(s), nil
}

func readAttr(sys fs.FS, dir, name string) string {
	b, err := fs.ReadFile(sys, path.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

func readHex16(sys fs.FS, dir, name string) uint16 {
	v, err := strconv.ParseUint(readAttr(sys, dir, name), 16, 16)
	if err != nil {
		return 0
	}
	return uint16(v)
}

func parseIoctlID(buf []byte) string {
	if len(buf) < 2 {
		return ""
	}
	end := len(buf)
	if n := int(binary.BigEndian.Uint16(buf)); n >= 2 && n <= len(buf) {
		end = n
	}
	s := string(buf[2:end])
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
