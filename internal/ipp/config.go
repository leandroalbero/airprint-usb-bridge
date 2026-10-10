// Package ipp is an IPP/2.0 (IPP Everywhere / AirPrint) server for a
// continuous-roll label printer.
package ipp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type MediaPolicy string

const (
	// MediaRoll advertises only the roll as loaded media.
	MediaRoll MediaPolicy = "roll"
	// MediaRollPresets also advertises the preset label lengths as loaded media.
	// iOS then shows a paper menu, but apps that pick the "best" loaded paper
	// stop using the roll.
	MediaRollPresets MediaPolicy = "roll+presets"
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

// Margins are in hundredths of a millimetre.
type Margins struct {
	Top, Bottom, Left, Right int
}

type Config struct {
	Name      string // printer-name, printer-info, printer-dns-sd-name
	Location  string
	MakeModel string // e.g. "Brother QL-700"
	DeviceID  string // IEEE 1284 device ID; derived from MakeModel when empty
	UUID      string // "urn:uuid:..." or a bare UUID
	Host      string // used in URIs only when a request has no Host header
	Port      int    // used in URIs when the Host header has no port; 0 = the listener's port

	// Media dimensions are in hundredths of a millimetre, as in media-col.
	RollWidth     int   // 6200 for 62 mm
	RollMinLength int   // 800
	RollMaxLength int   // 100000
	DefaultLength int   // media-col-default y-dimension, 10000
	Presets       []int // fixed label lengths, e.g. 2900, 5000
	MediaPolicy   MediaPolicy
	Margins       Margins
	TrayName      string // printer-input-tray medianame; "<width>mm" when empty

	// HighRes advertises print-quality high and 600 dpi (300x600 on the head).
	HighRes bool

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

// DefaultConfig returns the attribute values the live 62 mm setup was verified with.
func DefaultConfig() Config {
	return Config{
		Name:          "Brother QL-700",
		MakeModel:     "Brother QL-700",
		RollWidth:     6200,
		RollMinLength: 800,
		RollMaxLength: 100000,
		DefaultLength: 10000,
		Presets:       []int{2900, 5000},
		MediaPolicy:   MediaRoll,
		Margins:       Margins{Left: 152, Right: 152},
	}
}

func (c *Config) normalize() error {
	if c.Name == "" {
		return fmt.Errorf("ipp: config: empty Name")
	}
	if c.MakeModel == "" {
		c.MakeModel = c.Name
	}
	if c.RollWidth <= 0 {
		return fmt.Errorf("ipp: config: RollWidth %d", c.RollWidth)
	}
	if c.RollMinLength <= 0 {
		c.RollMinLength = 800
	}
	if c.RollMaxLength <= 0 {
		c.RollMaxLength = 100000
	}
	if c.RollMinLength > c.RollMaxLength {
		return fmt.Errorf("ipp: config: roll length range %d-%d", c.RollMinLength, c.RollMaxLength)
	}
	if c.DefaultLength <= 0 {
		c.DefaultLength = 10000
	}
	for _, p := range c.Presets {
		if p <= 0 {
			return fmt.Errorf("ipp: config: preset length %d", p)
		}
	}
	switch c.MediaPolicy {
	case "":
		c.MediaPolicy = MediaRoll
	case MediaRoll, MediaRollPresets:
	default:
		return fmt.Errorf("ipp: config: media policy %q (want roll or roll+presets)", c.MediaPolicy)
	}
	if c.UUID == "" {
		return fmt.Errorf("ipp: config: empty UUID")
	}
	if !strings.HasPrefix(c.UUID, "urn:uuid:") {
		c.UUID = "urn:uuid:" + c.UUID
	}
	if c.DeviceID == "" {
		mfg, mdl, ok := strings.Cut(c.MakeModel, " ")
		if !ok {
			mfg, mdl = c.MakeModel, c.MakeModel
		}
		c.DeviceID = "MFG:" + mfg + ";MDL:" + mdl + ";CMD:URF,PWG;"
	}
	if c.TrayName == "" {
		c.TrayName = mmString(c.RollWidth) + "mm"
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

// mmString formats hundredths of a millimetre as millimetres for PWG media names.
func mmString(v int) string {
	s := strconv.Itoa(v / 100)
	if f := v % 100; f != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%02d", f), "0")
	}
	return s
}

// URFSupported is the urf-supported value, also used for the TXT record's URF key.
func (c *Config) URFSupported() []string {
	if c.HighRes {
		return []string{"V1.4", "W8", "CP1", "PQ4-5", "RS300-600"}
	}
	return []string{"V1.4", "W8", "CP1", "RS300"}
}

// TXT returns the DNS-SD TXT record for _ipp._tcp and _ipps._tcp. host and port
// name this server as clients reach it (the mDNS host name).
func (c Config) TXT(host string, port int) map[string]string {
	_ = c.normalize()
	return map[string]string{
		"txtvers":  "1",
		"qtotal":   "1",
		"rp":       "ipp/print",
		"ty":       c.MakeModel,
		"adminurl": fmt.Sprintf("https://%s:%d/", host, port),
		"note":     c.Location,
		"pdl":      "image/pwg-raster,image/urf",
		"Color":    "F",
		"Duplex":   "F",
		"UUID":     strings.TrimPrefix(c.UUID, "urn:uuid:"),
		"TLS":      "1.2",
		"URF":      strings.Join(c.URFSupported(), ","),
	}
}
