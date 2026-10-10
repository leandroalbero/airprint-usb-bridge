// Package ql implements the Brother QL raster protocol (QL-700 first) without third-party printer code.
package ql

import "time"

type Provenance string

const (
	Documented Provenance = "documented"
	Measured   Provenance = "measured"
)

type Value[T any] struct {
	V      T          `json:"value"`
	Source Provenance `json:"source"`
	Note   string     `json:"note,omitempty"`
}

// Profile is loaded from profiles/<model>.json; nothing model-specific is hard-coded.
type Profile struct {
	Model string `json:"model"`
	// Verified means printed with on real hardware; use IsVerified.
	Verified         *bool    `json:"verified,omitempty"`
	Reference        string   `json:"reference,omitempty"`
	Notes            []string `json:"notes,omitempty"`
	USBVendor        uint16   `json:"usb_vendor"`
	USBProduct       uint16   `json:"usb_product"`
	USBProductEditor uint16   `json:"usb_product_editor_lite"`
	StatusModelByte3 byte     `json:"status_model_byte3"`
	StatusModelByte4 byte     `json:"status_model_byte4"`
	Pins             int      `json:"pins"`
	BytesPerLine     int      `json:"bytes_per_line"`
	DPI              int      `json:"dpi"`
	SupportsModeCmd  bool     `json:"supports_esc_i_a"`
	SupportsCompress bool     `json:"supports_compression"`
	// The command-support flags are negative so that a profile written before they existed
	// (the QL-700, user JSON files) keeps sending every command.
	InvalidateBytes int  `json:"invalidate_bytes,omitempty"` // NULs before ESC @; 0 means 200
	NoAutoCut       bool `json:"no_auto_cut,omitempty"`      // no cutter: ESC i M never sets auto-cut
	NoCutEvery      bool `json:"no_cut_every,omitempty"`     // ESC i A not supported
	NoExpandedMode  bool `json:"no_expanded_mode,omitempty"` // ESC i K not supported
	NoHighRes       bool `json:"no_high_res,omitempty"`      // no 300x600 mode
	// StatusMediaKindBit is set in the status media type on models that report 4Ah/4Bh where
	// ESC i z takes 0Ah/0Bh (QL-600/7x0W/8xx); it is cleared before media matching.
	StatusMediaKindBit byte `json:"status_media_kind_flag,omitempty"`
	// TwoColor marks red/black capable heads. Only black is ever printed.
	TwoColor          bool                      `json:"two_color,omitempty"`
	MarginDots        Value[[2]int]             `json:"continuous_margin_dots"`
	PageLengthDots    Value[[2]int]             `json:"continuous_page_length_dots"`
	LeadingBlankDots  Value[int]                `json:"leading_blank_dots"`
	TrailingBlankDots Value[int]                `json:"trailing_blank_dots"`
	ValidErrorBits    map[string][]byte         `json:"valid_error_bits"`
	Media             []Media                   `json:"media"`
	Settings          []PrinterSetting          `json:"settings,omitempty"`
	Extra             map[string]Value[float64] `json:"extra,omitempty"`
	// Mirror reverses the head-width line after Brother ref packing.
	// false (JSON omit) is the ref order: pin 0 is byte 0 bit 7, the right edge of the label as it exits.
	Mirror bool `json:"mirror,omitempty"`
}

// PrinterSetting is one ESC i U key from the profile.
type PrinterSetting struct {
	ID     string              `json:"id"`
	Key    byte                `json:"key"`
	Values []PrinterSettingVal `json:"values"`
}

type PrinterSettingVal struct {
	ID    string `json:"id"`
	Value byte   `json:"value"`
}

// IsVerified reports a profile tested on hardware. A file without the key predates it and
// counts as verified only for the QL-700, the one model that was.
func (p *Profile) IsVerified() bool {
	if p == nil {
		return false
	}
	if p.Verified != nil {
		return *p.Verified
	}
	return p.Model == "QL-700"
}

func (p *Profile) SettingByID(id string) (PrinterSetting, bool) {
	if p == nil {
		return PrinterSetting{}, false
	}
	for _, s := range p.Settings {
		if s.ID == id {
			return s, true
		}
	}
	return PrinterSetting{}, false
}

func (s PrinterSetting) ValueOf(token string) (byte, bool) {
	for _, v := range s.Values {
		if v.ID == token {
			return v.Value, true
		}
	}
	return 0, false
}

func (s PrinterSetting) TokenOf(value byte) (string, bool) {
	for _, v := range s.Values {
		if v.Value == value {
			return v.ID, true
		}
	}
	return "", false
}

type MediaKind byte

const (
	MediaNone       MediaKind = 0x00
	MediaContinuous MediaKind = 0x0A
	MediaDieCut     MediaKind = 0x0B
)

type Media struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	RefID       int       `json:"ref_id"`
	Kind        MediaKind `json:"kind"`
	WidthMM     int       `json:"width_mm"`
	LengthMM    int       `json:"length_mm"`
	LeftPins    int       `json:"left_margin_pins"`
	PrintPins   int       `json:"print_pins"`
	RightPins   int       `json:"right_margin_pins"`
	PrintLength int       `json:"print_length_dots,omitempty"`
	FixedMargin int       `json:"fixed_margin_dots,omitempty"`
	XTrim       int       `json:"x_trim_dots"`
}

// Bitmap is a 1-bit image, row-major, true = black. Width is in dots across the tape.
type Bitmap struct {
	W, H int
	Bits []bool
}

func NewBitmap(w, h int) *Bitmap   { return &Bitmap{W: w, H: h, Bits: make([]bool, w*h)} }
func (b *Bitmap) At(x, y int) bool { return x >= 0 && y >= 0 && x < b.W && y < b.H && b.Bits[y*b.W+x] }
func (b *Bitmap) Set(x, y int, v bool) {
	if x >= 0 && y >= 0 && x < b.W && y < b.H {
		b.Bits[y*b.W+x] = v
	}
}

type Terminator byte

const (
	TermPrint        Terminator = 0x0C
	TermPrintAndFeed Terminator = 0x1A
)

type JobOptions struct {
	MarginDots      int        `json:"margin_dots"`
	AutoCut         bool       `json:"auto_cut"`
	CutEvery        int        `json:"cut_every"`
	CutAtEnd        bool       `json:"cut_at_end"`
	QualityPriority bool       `json:"quality_priority"`
	HighRes         bool       `json:"high_res"`
	ValidateMedia   bool       `json:"validate_media"`
	LastTerminator  Terminator `json:"last_terminator"`
	// Compress sends TIFF (PackBits) raster lines and Z for blank lines; the profile must support it.
	Compress bool `json:"compress,omitempty"`
}

func DefaultJobOptions() JobOptions {
	return JobOptions{MarginDots: 0, AutoCut: true, CutEvery: 1, CutAtEnd: true, ValidateMedia: true, LastTerminator: TermPrintAndFeed}
}

type Job struct {
	Media   Media
	Options JobOptions
	Pages   []*Bitmap
}

type StatusType byte

const (
	StatusReply     StatusType = 0x00
	StatusCompleted StatusType = 0x01
	StatusError     StatusType = 0x02
	StatusNotify    StatusType = 0x05
	StatusPhase     StatusType = 0x06
	StatusSettings  StatusType = 0xF0
)

type Status struct {
	Raw           [32]byte
	ModelByte3    byte
	ModelByte4    byte
	Err1, Err2    byte
	Errors        []string
	Unexpected    []string
	MediaWidthMM  int
	MediaKind     MediaKind
	MediaLengthMM int
	Type          StatusType
	Printing      bool
	PhaseNumber   uint16
	Notification  byte
	At            time.Time
}

// Encoder turns a Job into the exact byte stream for the printer.
type Encoder interface {
	Encode(p *Profile, j Job) ([]byte, error)
}

// Device is the transport. The Linux usblp implementation lives in device_linux.go; tests use fakes.
type Device interface {
	Write(b []byte) (int, error)
	// ReadStatus waits up to timeout for exactly one 32-byte status (buffering split/merged reads).
	ReadStatus(timeout time.Duration) (Status, error)
	Close() error
}
