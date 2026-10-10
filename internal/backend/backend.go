// Package backend carries printer bytes: usblp (/dev/usb/lpN), raw TCP (port 9100) and a dry-run
// directory. It also discovers USB printers through sysfs and their IEEE 1284 device IDs.
//
// Drivers turn pages into bytes; a Transport only delivers them. A driver that needs a
// status-aware conversation with the printer (the Brother QL) supplies its own sender and may
// build it on Port.
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
)

var (
	// ErrRetryable marks a failure where no byte of the unit reached the printer.
	ErrRetryable = errors.New("retryable")
	ErrClosed    = errors.New("backend closed")

	ErrNoPrinter     = errors.New("no printer connected")
	ErrDeviceBusy    = errors.New("device busy")
	ErrPartial       = errors.New("may have printed partially")
	ErrTimeout       = errors.New("timeout")
	ErrUnsupportedOS = errors.New("unsupported on this OS")
)

// DefaultRetryDeadline is used by Retry when deadline <= 0.
const DefaultRetryDeadline = 120 * time.Second

// Transport delivers one unit of printer bytes at a time.
type Transport interface {
	// Send writes data as one job. It returns an ErrRetryable-wrapped error only when no byte
	// reached the printer.
	Send(ctx context.Context, data []byte) error
	// State reports the printer condition. Device failures are folded into the State; the error
	// is only for a cancelled context or a closed transport.
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

// Detail is one extra line for the status command, e.g. {"media", "62 mm"}.
type Detail struct {
	Name, Value string
}

// State is a printer snapshot.
type State struct {
	Condition Condition
	Reasons   []string // IPP keywords without severity suffix; empty means none
	Message   string
	Details   []Detail
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

// StateFromError classifies a failed open or status query.
func StateFromError(err error) State {
	if errors.Is(err, ErrDeviceBusy) {
		return State{Condition: Busy, Message: "in use by another process"}
	}
	return State{Condition: Offline, Reasons: []string{"offline-report"}, Message: err.Error()}
}

// Retryable wraps err with ErrRetryable. Use it only when nothing reached the printer.
func Retryable(err error) error {
	return fmt.Errorf("%w: %w", ErrRetryable, err)
}

// Retry calls send until it succeeds or fails with an error that does not wrap ErrRetryable.
// The backoff starts at 0.5 s and doubles up to 8 s, until deadline (DefaultRetryDeadline when
// <= 0) has passed since the first attempt. When it gives up the error still wraps ErrRetryable:
// nothing was printed.
func Retry(ctx context.Context, send func(context.Context) error, deadline time.Duration, log func(string)) error {
	return RetryBackoff(ctx, send, deadline, log, 500*time.Millisecond, 8*time.Second)
}

// RetryBackoff is Retry with the first wait base, doubling up to max.
func RetryBackoff(ctx context.Context, send func(context.Context) error, deadline time.Duration, log func(string), base, max time.Duration) error {
	if send == nil {
		return fmt.Errorf("print: nil sender")
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
		err := send(ctx)
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
