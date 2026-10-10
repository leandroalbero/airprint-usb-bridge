package driver

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
)

// Config is what the bridge knows when it builds a driver.
type Config struct {
	Media   string // --media; "" means the driver's default
	Dither  string // --dither: "auto", "on" or "off" ("" = auto)
	HighRes bool   // --high-res: offer print-quality high where the driver can
	PPD     string // --ppd, for the cups driver
	// Options are driver-specific settings (--driver-opt key=value). The registry rejects keys
	// the driver's Info does not list.
	Options map[string]string
	// DeviceID is the printer's IEEE 1284 ID when it is known (USB discovery); empty otherwise.
	DeviceID ieee1284.ID
	// USBVendorID and USBProductID identify the USB printer when it is known; 0 otherwise.
	USBVendorID, USBProductID uint16
	Logf                      func(format string, args ...any)
}

// Option returns Options[key], or def when unset.
func (c Config) Option(key, def string) string {
	if v, ok := c.Options[key]; ok {
		return v
	}
	return def
}

// IntOption parses Options[key] as an integer, def when unset.
func (c Config) IntOption(key string, def int) (int, error) {
	v, ok := c.Options[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("option %s=%q: want an integer", key, v)
	}
	return n, nil
}

// BoolOption parses Options[key] as a boolean, def when unset.
func (c Config) BoolOption(key string, def bool) (bool, error) {
	v, ok := c.Options[key]
	if !ok {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("option %s=%q: want true or false", key, v)
	}
	return b, nil
}

// Log calls Logf when it is set.
func (c Config) Log(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Option documents one --driver-opt key.
type Option struct {
	Key, Default, Help string
}

// Info registers a driver.
type Info struct {
	Name        string
	Description string
	Verified    string // hardware it was verified on, e.g. "QL-700"; empty = unverified
	Media       string // help for --media, e.g. "62, 29, 38 (DK roll width in mm)"
	Options     []Option
	New         func(Config) (Driver, error)
}

var (
	regMu    sync.Mutex
	registry = map[string]Info{}
)

// Register adds a driver; call it from the driver package's init. It panics on an empty or
// duplicate name, which is a programming error.
func Register(info Info) {
	regMu.Lock()
	defer regMu.Unlock()
	if info.Name == "" || info.New == nil {
		panic("driver: Register needs Name and New")
	}
	if _, dup := registry[info.Name]; dup {
		panic("driver: duplicate registration of " + info.Name)
	}
	registry[info.Name] = info
}

// Lookup returns a registered driver.
func Lookup(name string) (Info, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	info, ok := registry[name]
	return info, ok
}

// Registered lists the drivers by name.
func Registered() []Info {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Info, 0, len(registry))
	for _, i := range registry {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Names lists the registered driver names.
func Names() []string {
	var out []string
	for _, i := range Registered() {
		out = append(out, i.Name)
	}
	return out
}

// New builds a registered driver, after checking c.Options against its Info, and validates its
// Capabilities.
func New(name string, c Config) (Driver, error) {
	info, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown driver %q (have %s)", name, strings.Join(Names(), ", "))
	}
	for k := range c.Options {
		known := false
		for _, o := range info.Options {
			if o.Key == k {
				known = true
				break
			}
		}
		if !known {
			var keys []string
			for _, o := range info.Options {
				keys = append(keys, o.Key)
			}
			have := "none"
			if len(keys) > 0 {
				have = strings.Join(keys, ", ")
			}
			return nil, fmt.Errorf("driver %s has no option %q (have %s)", name, k, have)
		}
	}
	d, err := info.New(c)
	if err != nil {
		return nil, fmt.Errorf("driver %s: %w", name, err)
	}
	caps := d.Capabilities()
	if err := caps.Validate(); err != nil {
		return nil, fmt.Errorf("driver %s: %w", name, err)
	}
	return d, nil
}
