// Package ipp is an IPP/2.0 (IPP Everywhere / AirPrint) server. The printer it presents is
// described by driver.Capabilities: a continuous-roll label printer or a document printer.
package ipp

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
)

// PrinterState is the IPP printer-state enum.
type PrinterState int

const (
	StateIdle       PrinterState = 3
	StateProcessing PrinterState = 4
	StateStopped    PrinterState = 5
)

func (s PrinterState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateProcessing:
		return "processing"
	case StateStopped:
		return "stopped"
	}
	return strconv.Itoa(int(s))
}

// DeviceStatus is what the device reports. A zero State means "no opinion":
// the server then derives idle or processing from its own job queue.
type DeviceStatus struct {
	State   PrinterState
	Reasons []string // printer-state-reasons keywords, e.g. "media-empty-error"
	Message string
}

type Config struct {
	Name     string // printer-name, printer-info, printer-dns-sd-name
	Location string
	UUID     string // "urn:uuid:..." or a bare UUID
	Host     string // used in URIs only when a request has no Host header
	Port     int    // used in URIs when the Host header has no port; 0 = the listener's port

	// Caps describe the printer; every printer attribute and the TXT record come from them.
	Caps driver.Capabilities

	// StateDir holds the TLS key pair and the spool. Empty means an in-memory
	// certificate and os.TempDir for spooling.
	StateDir string
	// MaxDocumentBytes caps one document; 0 = 256 MiB.
	MaxDocumentBytes int64
	// MaxSpoolBytes caps the documents spooled for all unfinished jobs together;
	// 0 = 4 * MaxDocumentBytes.
	MaxSpoolBytes int64
	// MaxPendingJobs caps unfinished jobs, uploads in progress included; further
	// job requests get server-error-busy. 0 = 16.
	MaxPendingJobs int

	// Status is polled every 2 s (5 s timeout) while no job is processing, and
	// never concurrently with a job's start.
	Status func(ctx context.Context) DeviceStatus
	// Identify is called for Identify-Printer.
	Identify func(actions []string, message string)
	// Logf receives one line per request and per job transition; nil = silent.
	Logf func(format string, args ...any)
}

func (c *Config) normalize() error {
	if c.Name == "" {
		return fmt.Errorf("ipp: config: empty Name")
	}
	c.Caps = cloneCaps(c.Caps)
	if err := c.Caps.Validate(); err != nil {
		return fmt.Errorf("ipp: config: %w", err)
	}
	if c.UUID == "" {
		return fmt.Errorf("ipp: config: empty UUID")
	}
	if !strings.HasPrefix(c.UUID, "urn:uuid:") {
		c.UUID = "urn:uuid:" + c.UUID
	}
	if c.Caps.DeviceID == "" {
		c.Caps.DeviceID = deriveDeviceID(&c.Caps)
	}
	if c.MaxDocumentBytes <= 0 {
		c.MaxDocumentBytes = 256 << 20
	}
	if c.MaxSpoolBytes <= 0 {
		c.MaxSpoolBytes = 4 * c.MaxDocumentBytes
	}
	if c.MaxPendingJobs <= 0 {
		c.MaxPendingJobs = 16
	}
	return nil
}

// cloneCaps copies the slices so a caller changing its Capabilities later cannot change a
// running server.
func cloneCaps(c driver.Capabilities) driver.Capabilities {
	c.Kind = slices.Clone(c.Kind)
	c.Resolutions = slices.Clone(c.Resolutions)
	c.AlsoAccept = slices.Clone(c.AlsoAccept)
	c.Qualities = slices.Clone(c.Qualities)
	c.URF = slices.Clone(c.URF)
	c.PWGTypes = slices.Clone(c.PWGTypes)
	c.Formats = slices.Clone(c.Formats)
	c.Media = slices.Clone(c.Media)
	if c.Roll != nil {
		r := *c.Roll
		c.Roll = &r
	}
	return c
}

// deriveDeviceID builds "MFG:<first word>;MDL:<rest>;CMD:URF,PWG[,PDF][,PS];" from MakeModel.
func deriveDeviceID(c *driver.Capabilities) string {
	mfg, mdl, ok := strings.Cut(c.MakeModel, " ")
	if !ok {
		mfg, mdl = c.MakeModel, c.MakeModel
	}
	cmd := []string{"URF", "PWG"}
	for _, f := range c.Formats {
		switch f {
		case "application/pdf":
			cmd = append(cmd, "PDF")
		case "application/postscript":
			cmd = append(cmd, "PS")
		}
	}
	return "MFG:" + mfg + ";MDL:" + mdl + ";CMD:" + strings.Join(cmd, ",") + ";"
}

// documentFormats is document-format-supported: octet-stream (sniffed), the raster formats and the
// driver's pass-through formats, sorted.
func (c *Config) documentFormats() []string {
	out := []string{formatOctetStream, formatPWG, formatURF}
	for _, f := range c.Caps.Formats {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// TXT returns the DNS-SD TXT record for _ipp._tcp and _ipps._tcp. host and port
// name this server as clients reach it (the mDNS host name).
func (c Config) TXT(host string, port int) map[string]string {
	_ = c.normalize()
	var pdl []string
	for _, f := range c.documentFormats() {
		if f != formatOctetStream {
			pdl = append(pdl, f)
		}
	}
	txt := map[string]string{
		"txtvers":  "1",
		"qtotal":   "1",
		"rp":       "ipp/print",
		"ty":       c.Caps.MakeModel,
		"adminurl": fmt.Sprintf("https://%s:%d/", host, port),
		"note":     c.Location,
		"pdl":      strings.Join(pdl, ","),
		"Color":    tf(c.Caps.Color),
		"Duplex":   tf(c.Caps.Duplex),
		"UUID":     strings.TrimPrefix(c.UUID, "urn:uuid:"),
		"TLS":      "1.2",
		"URF":      strings.Join(c.Caps.URF, ","),
	}
	if c.Caps.IsDocument() {
		// Bonjour Printing keys for sheet printers; the label printer keeps the record iOS has
		// accepted.
		txt["kind"] = strings.Join(c.Caps.Kind, ",")
		txt["product"] = "(" + c.Caps.MakeModel + ")"
		txt["PaperMax"] = paperMax(c.Caps.Media)
	}
	return txt
}

// paperMax is the Bonjour Printing TXT PaperMax class of the largest fixed size.
func paperMax(media []driver.Media) string {
	w, l := 0, 0
	for _, m := range media {
		w, l = max(w, m.Width), max(l, m.Length)
	}
	switch {
	case w < 21000 || l < 29700:
		return "<legal-A4"
	case w < 29700 || l < 42000:
		return "legal-A4"
	case w < 42000 || l < 59400:
		return "tabloid-A3"
	case w < 59400 || l < 84100:
		return "isoC-A2"
	}
	return ">isoC-A2"
}

func tf(b bool) string {
	if b {
		return "T"
	}
	return "F"
}
