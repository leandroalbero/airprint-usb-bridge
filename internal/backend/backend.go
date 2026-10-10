// Package backend sends encoded QL jobs to a printer: usblp through internal/ql, raw TCP 9100,
// or a dry-run directory.
//
// Printer conditions map to IPP printer-state-reasons keywords (without severity suffix) as follows.
//
//	ql status error bit    reasons
//	no media               media-empty, media-needed
//	end of media           media-empty, media-needed
//	media mismatch         media-needed
//	cutter jam             media-jam
//	cannot feed            media-jam
//	cover open             cover-open
//	main unit in use, fan, transmission, system error, unexpected bits
//	                       other
//	no printer / device gone / unsupported OS / no reply
//	                       offline-report (Condition Offline)
//
// Condition maps to printer-state: Idle and Unknown are idle (3), Busy is processing (4), Offline
// and Error are stopped (5).
package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

var (
	// ErrRetryable marks a failure where no byte of the job reached the printer.
	ErrRetryable = errors.New("retryable")
	ErrClosed    = errors.New("backend closed")
)

// DefaultRetryDeadline is used by PrintWithRetry when deadline <= 0.
const DefaultRetryDeadline = 120 * time.Second

type Backend interface {
	// Print sends one label as one job. It reports ErrRetryable-wrapped errors only when
	// nothing reached the printer.
	Print(ctx context.Context, job ql.Job) error
	State(ctx context.Context) (State, error)
	Close() error
}

type Condition int

const (
	Unknown Condition = iota
	Idle
	Busy
	Offline
	Error
)

func (c Condition) String() string {
	switch c {
	case Idle:
		return "idle"
	case Busy:
		return "busy"
	case Offline:
		return "offline"
	case Error:
		return "error"
	default:
		return "unknown"
	}
}

// State is a printer snapshot. Device failures are folded into Condition; State methods return a
// Go error only for a cancelled context or a closed backend.
type State struct {
	Condition Condition
	Reasons   []string // IPP keywords without severity suffix; empty means none
	Message   string
	Status    *ql.Status // the status reply behind this state, when there was one
}

// IPPState is the printer-state enum value.
func (s State) IPPState() int {
	switch s.Condition {
	case Busy:
		return 4
	case Offline, Error:
		return 5
	default:
		return 3
	}
}

// IPPReasons returns printer-state-reasons with severity suffixes, or ["none"].
func (s State) IPPReasons() []string {
	if len(s.Reasons) == 0 {
		return []string{"none"}
	}
	suffix := "-report"
	if s.Condition == Offline || s.Condition == Error {
		suffix = "-error"
	}
	out := make([]string, 0, len(s.Reasons))
	for _, r := range s.Reasons {
		if strings.HasSuffix(r, "-report") || strings.HasSuffix(r, "-warning") || strings.HasSuffix(r, "-error") {
			out = append(out, r)
			continue
		}
		out = append(out, r+suffix)
	}
	return out
}

var reasonTable = []struct {
	err     error
	reasons []string
}{
	{ql.ErrNoMedia, []string{"media-empty", "media-needed"}},
	{ql.ErrEndOfMedia, []string{"media-empty", "media-needed"}},
	{ql.ErrMediaMismatch, []string{"media-needed"}},
	{ql.ErrCutterJam, []string{"media-jam"}},
	{ql.ErrCannotFeed, []string{"media-jam"}},
	{ql.ErrCoverOpen, []string{"cover-open"}},
	{ql.ErrMainUnitInUse, []string{"other"}},
	{ql.ErrFan, []string{"other"}},
	{ql.ErrTransmission, []string{"other"}},
	{ql.ErrSystem, []string{"other"}},
	{ql.ErrPrinter, []string{"other"}},
	{ql.ErrNoPrinter, []string{"offline-report"}},
	{ql.ErrDeviceGone, []string{"offline-report"}},
	{ql.ErrUnsupportedOS, []string{"offline-report"}},
}

// ReasonsForStatus maps a status reply's error bits.
func ReasonsForStatus(st ql.Status) []string {
	var out []string
	for _, name := range st.Errors {
		out = appendReasons(out, reasonsForName(name)...)
	}
	if len(st.Unexpected) > 0 || (st.Type == ql.StatusError && len(out) == 0) {
		out = appendReasons(out, "other")
	}
	return out
}

// ReasonsForError maps a Print error from any backend; errors without a printer cause give nil.
func ReasonsForError(err error) []string {
	if err == nil {
		return nil
	}
	var out []string
	for _, r := range reasonTable {
		if errors.Is(err, r.err) {
			out = appendReasons(out, r.reasons...)
		}
	}
	return out
}

func reasonsForName(name string) []string {
	for _, r := range reasonTable {
		if r.err.Error() == name {
			return r.reasons
		}
	}
	return []string{"other"}
}

func appendReasons(dst []string, rs ...string) []string {
	for _, r := range rs {
		dup := false
		for _, d := range dst {
			if d == r {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, r)
		}
	}
	return dst
}

// StateFromStatus classifies a status reply.
func StateFromStatus(st ql.Status) State {
	s := State{Condition: Idle, Status: &st}
	if st.Type == ql.StatusError || len(st.Errors) > 0 {
		s.Condition = Error
		s.Reasons = ReasonsForStatus(st)
		msg := append(append([]string(nil), st.Errors...), st.Unexpected...)
		if len(msg) == 0 {
			msg = []string{ql.ErrPrinter.Error()}
		}
		s.Message = strings.Join(msg, ", ")
		return s
	}
	if st.Printing || (st.Type == ql.StatusNotify && st.Notification == ql.NotifyCoolingStart) {
		s.Condition = Busy
	}
	return s
}

func stateFromErr(err error) State {
	if errors.Is(err, ql.ErrDeviceBusy) {
		return State{Condition: Busy, Message: "in use by another process"}
	}
	return State{Condition: Offline, Reasons: []string{"offline-report"}, Message: err.Error()}
}

func statusErr(st ql.Status) error {
	var errs []error
	for _, name := range st.Errors {
		var e error
		for _, r := range reasonTable {
			if r.err.Error() == name {
				e = r.err
				break
			}
		}
		if e == nil {
			e = errors.New(name)
		}
		errs = append(errs, e)
	}
	for _, u := range st.Unexpected {
		errs = append(errs, errors.New(u))
	}
	if len(errs) == 0 {
		return ql.ErrPrinter
	}
	return errors.Join(errs...)
}

func retryable(err error) error {
	return fmt.Errorf("%w: %w", ErrRetryable, err)
}

// JobFor builds one label's job: the bitmap is left-aligned and padded on the right to
// media.PrintPins, die-cut media is padded in height to PrintLength, and the options are auto-cut
// after every label and at the end, margin 0, media validation on. A bitmap wider than PrintPins
// is left as is so Encode rejects it with ql.ErrBitmapWidth.
func JobFor(prof *ql.Profile, media ql.Media, bitmap *ql.Bitmap, highRes bool) ql.Job {
	o := ql.DefaultJobOptions()
	o.MarginDots = 0
	o.AutoCut = true
	o.CutEvery = 1
	o.CutAtEnd = true
	o.HighRes = highRes
	o.ValidateMedia = true
	job := ql.Job{Media: media, Options: o}
	if bitmap == nil {
		return job
	}
	b := bitmap
	if b.W < media.PrintPins {
		b = pad(b, media.PrintPins, b.H)
	}
	if media.PrintLength > 0 && b.W <= media.PrintPins && b.H < media.PrintLength {
		b = pad(b, b.W, media.PrintLength)
	}
	job.Pages = []*ql.Bitmap{b}
	return job
}

func pad(src *ql.Bitmap, w, h int) *ql.Bitmap {
	out := ql.NewBitmap(w, h)
	for y := 0; y < src.H; y++ {
		copy(out.Bits[y*w:y*w+src.W], src.Bits[y*src.W:(y+1)*src.W])
	}
	return out
}

// PrintWithRetry retries ErrRetryable failures with a 0.5 s backoff that doubles up to 8 s, until
// deadline (DefaultRetryDeadline when <= 0) has passed since the first attempt. When it gives up the
// returned error still wraps ErrRetryable: nothing was printed.
func PrintWithRetry(ctx context.Context, b Backend, job ql.Job, deadline time.Duration, log func(string)) error {
	return printWithRetry(ctx, b, job, deadline, log, 500*time.Millisecond, 8*time.Second)
}

func printWithRetry(ctx context.Context, b Backend, job ql.Job, deadline time.Duration, log func(string), base, max time.Duration) error {
	if b == nil {
		return fmt.Errorf("print: nil backend")
	}
	if deadline <= 0 {
		deadline = DefaultRetryDeadline
	}
	if log == nil {
		log = func(string) {}
	}
	start := time.Now()
	wait := base
	for attempt := 1; ; attempt++ {
		err := b.Print(ctx, job)
		if err == nil || !errors.Is(err, ErrRetryable) {
			return err
		}
		rem := deadline - time.Since(start)
		if rem <= 0 {
			return fmt.Errorf("gave up after %d attempts in %s: %w", attempt, time.Since(start).Round(time.Millisecond), err)
		}
		d := min(wait, rem)
		log(fmt.Sprintf("print attempt %d: %v; retrying in %s", attempt, err, d))
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (last attempt: %w)", ctx.Err(), err)
		case <-t.C:
		}
		wait = min(wait*2, max)
	}
}
