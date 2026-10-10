package brotherql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

// Backend sends one QL label as one device job and reads the QL status replies. It is the
// status-aware path behind the driver's Sender: usblp through ql.Printer, or raw TCP 9100 with
// ESC i S status on network models.
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
type Backend interface {
	// Print sends one label as one job. It reports backend.ErrRetryable-wrapped errors only
	// when nothing reached the printer.
	Print(ctx context.Context, job ql.Job) error
	State(ctx context.Context) (backend.State, error)
	Close() error
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

// StateFromStatus classifies a status reply. Details carry the loaded media for the status command.
func StateFromStatus(st ql.Status) backend.State {
	s := backend.State{Condition: backend.Idle, Details: statusDetails(st)}
	if st.Type == ql.StatusError || len(st.Errors) > 0 {
		s.Condition = backend.Error
		s.Reasons = ReasonsForStatus(st)
		msg := append(append([]string(nil), st.Errors...), st.Unexpected...)
		if len(msg) == 0 {
			msg = []string{ql.ErrPrinter.Error()}
		}
		s.Message = strings.Join(msg, ", ")
		return s
	}
	if st.Printing || (st.Type == ql.StatusNotify && st.Notification == ql.NotifyCoolingStart) {
		s.Condition = backend.Busy
	}
	return s
}

func statusDetails(st ql.Status) []backend.Detail {
	media := fmt.Sprintf("%d mm", st.MediaWidthMM)
	if st.MediaLengthMM > 0 {
		media += fmt.Sprintf(" x %d mm", st.MediaLengthMM)
	}
	media += fmt.Sprintf(" (kind 0x%02x)", byte(st.MediaKind))
	out := []backend.Detail{{Name: "media", Value: media}}
	if len(st.Errors) > 0 {
		out = append(out, backend.Detail{Name: "errors", Value: strings.Join(st.Errors, ", ")})
	}
	return out
}

func stateFromErr(err error) backend.State {
	if errors.Is(err, ql.ErrDeviceBusy) {
		return backend.State{Condition: backend.Busy, Message: "in use by another process"}
	}
	return backend.State{Condition: backend.Offline, Reasons: []string{"offline-report"}, Message: err.Error()}
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

func retryable(err error) error { return backend.Retryable(err) }

// JobFor builds one label's job: the bitmap is left-aligned and padded on the right to
// media.PrintPins, die-cut media is padded in height to PrintLength, and the options are auto-cut
// after every label and at the end (unless the profile has no cutter), margin 0, media
// validation on. A bitmap wider than PrintPins is left as is so Encode rejects it with
// ql.ErrBitmapWidth.
func JobFor(prof *ql.Profile, media ql.Media, bitmap *ql.Bitmap, highRes bool) ql.Job {
	o := ql.DefaultJobOptions()
	o.MarginDots = 0
	o.AutoCut = true
	o.CutEvery = 1
	o.CutAtEnd = true
	o.HighRes = highRes
	o.ValidateMedia = true
	if prof != nil && prof.NoAutoCut {
		o.AutoCut, o.CutAtEnd = false, false
	}
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
