package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type usbTransport struct {
	device string // a node path, or "auto"
	match  string
	accept func(USBPrinter) bool
	stall  time.Duration

	open     func(path string, stall time.Duration) (Port, error)
	discover func() ([]USBPrinter, error)
	status   func(path string) (State, error)

	// mu holds one unit or one status query at a time; State reports busy instead of waiting.
	mu     sync.Mutex
	closed atomic.Bool
}

// NewUSB writes each unit to a usblp node, opened for that unit only so other tools can use the
// printer in between. device is a path or "auto", which picks the first discovered printer whose
// manufacturer and model satisfy match (see ieee1284.MatchNames) each time a unit is sent.
// Only a failed open, or a write that moved no byte, is retryable.
func NewUSB(device, match string) Transport { return newUSB(device, match, nil) }

func newUSB(device, match string, accept func(USBPrinter) bool) *usbTransport {
	return &usbTransport{
		device:   device,
		match:    match,
		accept:   accept,
		stall:    DefaultStall,
		open:     OpenUSB,
		discover: DiscoverSystem,
		status:   usbStatus,
	}
}

func (u *usbTransport) path() (string, error) {
	if u.device != "" && u.device != "auto" {
		return u.device, nil
	}
	list, err := u.discover()
	if err != nil {
		return "", err
	}
	p, err := PickFunc(list, u.match, u.accept)
	if err != nil {
		return "", err
	}
	return p.Path, nil
}

const usbChunk = 64 << 10

func (u *usbTransport) Send(ctx context.Context, data []byte) error {
	if u.closed.Load() {
		return ErrClosed
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := u.path()
	if err != nil {
		return Retryable(err)
	}
	port, err := u.open(path, u.stall)
	if err != nil {
		if errors.Is(err, ErrDeviceBusy) || errors.Is(err, ErrNoPrinter) {
			return Retryable(err)
		}
		return err
	}
	wrote := 0
	for wrote < len(data) {
		if err := ctx.Err(); err != nil {
			port.Close()
			if wrote == 0 {
				return err
			}
			return fmt.Errorf("%s: %w after %d bytes: %w", path, ErrPartial, wrote, err)
		}
		end := min(wrote+usbChunk, len(data))
		n, err := port.Write(data[wrote:end])
		wrote += n
		if err != nil {
			port.Close()
			if wrote == 0 {
				return Retryable(fmt.Errorf("%s: %w", path, err))
			}
			return fmt.Errorf("%s: %w after %d bytes: %w", path, ErrPartial, wrote, err)
		}
	}
	if err := port.Close(); err != nil {
		return fmt.Errorf("%s: %w: %w", path, ErrPartial, err)
	}
	return nil
}

func (u *usbTransport) State(ctx context.Context) (State, error) {
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
	path, err := u.path()
	if err != nil {
		return StateFromError(err), nil
	}
	st, err := u.status(path)
	if err != nil {
		return StateFromError(err), nil
	}
	return st, nil
}

func (u *usbTransport) Close() error {
	u.closed.Store(true)
	return nil
}

// IEEE 1284 status bits as LPGETSTATUS reports them.
const (
	lpErrorBit    = 0x08 // set when there is no error
	lpSelectedBit = 0x10 // set when online
	lpPaperOutBit = 0x20
)

// stateFromLPStatus maps the parallel-port status byte usblp emulates.
func stateFromLPStatus(b byte) State {
	switch {
	case b&lpPaperOutBit != 0:
		return State{Condition: Error, Reasons: []string{"media-empty", "media-needed"}, Message: "out of paper"}
	case b&lpSelectedBit == 0:
		return State{Condition: Offline, Reasons: []string{"offline-report"}, Message: "printer offline"}
	case b&lpErrorBit == 0:
		return State{Condition: Error, Reasons: []string{"other"}, Message: "printer error"}
	}
	return State{Condition: Idle}
}
