package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

type usb struct {
	prof     *ql.Profile
	open     func() (ql.Device, error)
	timeouts ql.Timeouts

	// mu holds one label or one status query at a time; State reports busy instead of waiting.
	mu     sync.Mutex
	closed atomic.Bool
}

// NewUSB prints through ql.NewPrinter and opens the usblp device ("auto" or a path) once per label.
func NewUSB(prof *ql.Profile, device string) Backend {
	return newUSB(prof, func() (ql.Device, error) { return ql.OpenDevice(prof, device) }, ql.DefaultTimeouts())
}

func newUSB(prof *ql.Profile, open func() (ql.Device, error), t ql.Timeouts) *usb {
	return &usb{prof: prof, open: open, timeouts: t}
}

func (u *usb) Print(ctx context.Context, job ql.Job) error {
	if u.closed.Load() {
		return ErrClosed
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	// Only a failed open proves nothing was sent: ql passes ErrDeviceBusy through from Write too.
	var opened atomic.Bool
	pr := ql.NewPrinter(u.prof, func() (ql.Device, error) {
		dev, err := u.open()
		if err == nil {
			opened.Store(true)
		}
		return dev, err
	}, u.timeouts)
	res := pr.Print(ctx, job)
	if res.Err == nil && res.State != ql.JobFailed && res.State != ql.JobCancelled {
		return nil
	}
	// A cancel that lands while ql waits for the printer to go idle still
	// reports ErrPartial, though the printer confirmed the whole label.
	if len(job.Pages) > 0 && res.PagesConfirmed >= len(job.Pages) {
		return nil
	}
	if res.Err == nil {
		return fmt.Errorf("print: %s", res.State)
	}
	err := fmt.Errorf("print: %w", res.Err)
	if !opened.Load() && (errors.Is(res.Err, ql.ErrDeviceBusy) || errors.Is(res.Err, ql.ErrNoPrinter)) {
		return retryable(err)
	}
	return err
}

func (u *usb) State(ctx context.Context) (State, error) {
	if u.closed.Load() {
		return State{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if !u.mu.TryLock() {
		return State{Condition: Busy, Message: "printing"}, nil
	}
	defer u.mu.Unlock()
	st, err := ql.NewPrinter(u.prof, u.open, u.timeouts).Status(ctx)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return State{}, cerr
		}
		return stateFromErr(err), nil
	}
	return StateFromStatus(st), nil
}

func (u *usb) Close() error {
	u.closed.Store(true)
	return nil
}
