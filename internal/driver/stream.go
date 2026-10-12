package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// PageReader yields a job's pages one at a time, in order.
type PageReader interface {
	// Next returns the next page, or io.EOF after the last one. The page's Gray belongs to the
	// driver until the next call: it may overwrite it, and must not keep it.
	Next() (raster.Page, error)
}

// Streamer is a document driver that renders a job page by page straight to the printer, so the
// length of a document is not bounded by memory. For a raster job the bridge:
//
//  1. reads past every page, checking its data, and calls CheckPage for each (Gray is nil);
//  2. calls Plan with the page count;
//  3. sends each unit through a StreamSender, calling Stream with the pages from the first one.
//
// Nothing reaches the printer before step 3, so every document error must come from step 1 or
// 2. Render is still required (tests, single images); RenderStream implements it.
type Streamer interface {
	// CheckPage reports whether page n (from 1) can be printed, from its size and resolution
	// alone. p.Gray is nil in the bridge's check pass, and a whole page under RenderStream.
	CheckPage(job JobInfo, n int, p raster.Page) error
	// Plan returns the device jobs for a document of pages pages, with Data nil. Every unit
	// carries the same bytes (the copies of one device job), and nothing has been sent yet.
	Plan(job JobInfo, pages int) ([]Unit, error)
	// Stream writes unit u, reading every page from pages. Use NewOutput on w: it decides
	// where a failed job can still be ended.
	Stream(ctx context.Context, job JobInfo, u Unit, pages PageReader, w io.Writer) error
}

// DocumentStreamer is the streaming DocumentRenderer: a pass-through document is read from the
// spool file while it is sent instead of being held in memory.
type DocumentStreamer interface {
	// PlanDocument checks the document and returns its device jobs with Data nil, as Plan does.
	PlanDocument(job JobInfo, format string, doc *io.SectionReader) ([]Unit, error)
	// StreamDocument writes unit u of the document to w.
	StreamDocument(ctx context.Context, job JobInfo, u Unit, doc *io.SectionReader, w io.Writer) error
}

// StreamSender is a Sender that can deliver a unit as it is produced.
type StreamSender interface {
	Sender
	// SendStream sends one unit whose bytes write produces; write may be called again for a
	// retry. The printer is opened on the first byte written. The error wraps
	// backend.ErrRetryable only when nothing reached the printer.
	SendStream(ctx context.Context, u Unit, write func(w io.Writer) error) error
}

// Output is a Streamer's view of the device job. Writes pass straight through, whatever the
// context says: a driver checks Err between pages, where the language lets a job end cleanly,
// and calls Abort when it gives up.
type Output struct {
	ctx  context.Context
	w    io.Writer
	n    int64
	werr error
}

// NewOutput wraps the device stream w of a job running under ctx.
func NewOutput(ctx context.Context, w io.Writer) *Output { return &Output{ctx: ctx, w: w} }

func (o *Output) Write(p []byte) (int, error) {
	if o.werr != nil {
		return 0, o.werr
	}
	n, err := o.w.Write(p)
	o.n += int64(n)
	if err != nil {
		o.werr = err
	}
	return n, err
}

// Err is the job's cancellation, if any. Call it between pages.
func (o *Output) Err() error { return o.ctx.Err() }

// Written is the number of bytes that reached the device stream.
func (o *Output) Written() int64 { return o.n }

// Abort ends a job that failed with err. When part of it was sent and the stream still works,
// it writes end, the language's job ending, even after a cancel, so the printer does not wait
// for the rest of the job. The result wraps backend.ErrRetryable only when nothing was sent.
func (o *Output) Abort(err error, end []byte) error {
	if o.n == 0 {
		return err
	}
	if o.werr == nil && len(end) > 0 {
		if _, werr := o.w.Write(end); werr != nil {
			err = errors.Join(err, werr)
		}
	}
	if errors.Is(err, backend.ErrRetryable) {
		return fmt.Errorf("%w after %d bytes: %v", backend.ErrPartial, o.n, err)
	}
	if errors.Is(err, backend.ErrPartial) {
		return err
	}
	return fmt.Errorf("%w after %d bytes: %w", backend.ErrPartial, o.n, err)
}

// RenderStream implements Driver.Render for a Streamer: it checks every page, plans, and
// streams the first unit into memory. pages[i].Gray is set to nil once page i is converted.
func RenderStream(ctx context.Context, s Streamer, job JobInfo, pages []raster.Page) ([]Unit, error) {
	for i, p := range pages {
		if p.Gray == nil && p.RGB == nil {
			// A nil Gray tells CheckPage the page is not decoded yet; these pages are, so it
			// is a page without pixels.
			p.Gray = []byte{}
		}
		if err := s.CheckPage(job, i+1, p); err != nil {
			return nil, err
		}
	}
	units, err := s.Plan(job, len(pages))
	if err != nil || len(units) == 0 {
		return units, err
	}
	var buf bytes.Buffer
	if err := s.Stream(ctx, job, units[0], &slicePages{pages: pages}, &buf); err != nil {
		return nil, err
	}
	for i := range units {
		units[i].Data = buf.Bytes()
	}
	return units, nil
}

// slicePages hands out copies, so a driver writing over a page cannot change the caller's.
type slicePages struct {
	pages []raster.Page
	i     int
}

func (s *slicePages) Next() (raster.Page, error) {
	if s.i >= len(s.pages) {
		return raster.Page{}, io.EOF
	}
	p := s.pages[s.i]
	p.Gray = bytes.Clone(p.Gray)
	p.RGB = bytes.Clone(p.RGB)
	s.pages[s.i].Gray = nil
	s.pages[s.i].RGB = nil
	s.i++
	return p, nil
}
