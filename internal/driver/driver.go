// Package driver turns decoded raster pages into printer bytes. A Driver describes the printer
// (Capabilities, which drive every IPP attribute and the DNS-SD TXT record) and renders a job
// into Units that a Sender delivers one at a time.
//
// See docs/specs/DRIVER-API.md (local) for the contract; the short version:
//   - Render converts and validates every page before returning, so a bad page never leaves a
//     half-printed job.
//   - A document driver also implements Streamer: the bridge checks every page first, then
//     streams the job to the printer page by page, so its length is not bounded by memory.
//   - Optional interfaces add behaviour: Streamer, DocumentStreamer, PageLimiter,
//     DocumentRenderer, ImageRenderer, Inspector and SenderProvider.
//   - Drivers register themselves from init with Register.
package driver

import (
	"context"
	"errors"
	"fmt"
	"image"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

type Driver interface {
	Name() string               // registry name: "brother-ql", "pcl", "postscript", "cups", "dymo", "zpl"
	Capabilities() Capabilities // drives every IPP attribute and the TXT record
	// Render validates and converts ALL pages before anything is sent, so a bad page never
	// leaves a half-printed job. It returns the units to send in order: a label driver returns
	// one unit per label, a document driver usually one unit for the job. Render may set
	// pages[i].Gray to nil once page i is converted, to free memory early.
	Render(ctx context.Context, job JobInfo, pages []raster.Page) ([]Unit, error)
}

// Unit is one piece of printer data sent as one device job (one open of the usblp node, one
// TCP connection, one job-<n>.bin in a dry run).
type Unit struct {
	Data  []byte
	Label string // for logs, e.g. "label 2 of 3 (page 2)"
	Meta  any    // driver-private, e.g. the ql.Job for the Brother QL sender
	// Impressions is what this unit adds to job-impressions-completed once sent; 0 counts as 1.
	Impressions int
}

// JobInfo is the accepted job template of one IPP job, resolved against the Capabilities.
type JobInfo struct {
	ID     int
	Name   string // job-name
	User   string // requesting-user-name
	Format string // document-format of the data being rendered

	Copies int // >= 1; see Capabilities.MaxCopies and CopiesNative
	// Media is the media the client asked for, matched against Capabilities: a fixed size from
	// Capabilities.Media, or the roll with Length set to the requested length (0 when the
	// client sent a range or no length). It is the default media when the client sent none.
	Media       Media
	Quality     int    // print-quality: 3 draft, 4 normal, 5 high
	ColorMode   string // print-color-mode: "monochrome", "color", "auto", "auto-monochrome"
	Sides       string // "one-sided", "two-sided-long-edge", "two-sided-short-edge"
	Orientation int    // orientation-requested: 3 portrait, 4 landscape, 5 reverse-landscape, 6 reverse-portrait; 0 absent
	Scaling     string // print-scaling: "auto", "auto-fit", "fill", "fit", "none"; "" absent
	Resolution  Resolution

	// Logf receives progress lines for the job log; nil means silent. Use Log.
	Logf func(format string, args ...any)
	// StateMessage, when non-nil, sets the job's job-state-message (for example a filter's
	// STATE or ERROR text). Use SetStateMessage.
	StateMessage func(msg string)
	// Budget, when non-nil, limits the sheets the job may place (see MaxSheetBytes); drivers
	// pass it to imaging.Place.
	Budget *Budget
	// Debug, when non-nil, stores an image for troubleshooting (--debug-dir), e.g.
	// Debug("p1-label", img) becomes <job>-p1-label.png.
	Debug func(name string, img image.Image)
}

// Log calls Logf when it is set.
func (j JobInfo) Log(format string, args ...any) {
	if j.Logf != nil {
		j.Logf(format, args...)
	}
}

// SetStateMessage calls StateMessage when it is set.
func (j JobInfo) SetStateMessage(msg string) {
	if j.StateMessage != nil {
		j.StateMessage(msg)
	}
}

// DebugImage calls Debug when it is set.
func (j JobInfo) DebugImage(name string, img image.Image) {
	if j.Debug != nil {
		j.Debug(name, img)
	}
}

// PageLimiter lets the bridge refuse a page from its header, before a highly compressed body is
// expanded. Only WidthMin and WidthMax are used; the bridge sets the memory cap.
type PageLimiter interface {
	PageLimits(job JobInfo) raster.Limits
}

// DocumentRenderer renders a pass-through format listed in Capabilities.Formats (for example
// application/pdf for a PDF-native printer). Raster formats always go through Render.
type DocumentRenderer interface {
	RenderDocument(ctx context.Context, job JobInfo, format string, data []byte) ([]Unit, error)
}

// ImageRenderer prints an image file given to the "print" command. Without it the bridge turns the
// image into a raster.Page at the default resolution and calls Render.
type ImageRenderer interface {
	RenderImage(ctx context.Context, job JobInfo, img image.Image) ([]Unit, error)
}

// Inspector explains printer bytes for dry runs (job-<n>.txt) and tests.
type Inspector interface {
	Inspect(data []byte) (string, error)
}

var (
	// ErrDocument marks a document the driver cannot print as sent (wrong size, resolution,
	// format). The job gets job-state-reasons document-format-error. Wrap with DocumentError.
	ErrDocument = errors.New("document not printable")
	// ErrNotImplemented is returned by placeholder drivers.
	ErrNotImplemented = errors.New("driver not implemented yet")
)

type documentError struct{ err error }

func (e documentError) Error() string   { return e.err.Error() }
func (e documentError) Unwrap() []error { return []error{e.err, ErrDocument} }

// DocumentError marks err as ErrDocument without changing its message.
func DocumentError(err error) error {
	if err == nil {
		return nil
	}
	return documentError{err}
}

// DocumentErrorf is fmt.Errorf wrapped with DocumentError.
func DocumentErrorf(format string, args ...any) error {
	return DocumentError(fmt.Errorf(format, args...))
}

// RepeatUnits returns units repeated copies times, for drivers whose printer cannot make copies
// itself (the bridge does this when Capabilities.CopiesNative is false).
func RepeatUnits(units []Unit, copies int) []Unit {
	if copies <= 1 || len(units) == 0 {
		return units
	}
	out := make([]Unit, 0, len(units)*copies)
	for c := 0; c < copies; c++ {
		out = append(out, units...)
	}
	return out
}
