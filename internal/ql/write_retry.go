package ql

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// writeLoop is the usblp write retry. wait blocks until POLLOUT or the remaining timeout.
type writeLoop struct {
	write func([]byte) (int, error)
	wait  func(time.Duration) error
	now   func() time.Time
}

func writeAll(w writeLoop, b []byte, timeout time.Duration) (int, error) {
	now := w.now
	if now == nil {
		now = time.Now
	}
	wrote := 0
	deadline := now().Add(timeout)
	for wrote < len(b) {
		rem := deadline.Sub(now())
		if rem <= 0 {
			return wrote, fmt.Errorf("write: %w", ErrTimeout)
		}
		if err := w.wait(rem); err != nil {
			return wrote, err
		}
		n, err := w.write(b[wrote:])
		if n > 0 {
			// The QL prints as data arrives and throttles USB to print speed, so a long
			// page cannot finish inside one fixed window. The timeout is a stall: any
			// progress restarts it, and only 2 s with no bytes is a failure.
			wrote += n
			deadline = now().Add(timeout)
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EPIPE) {
				return wrote, fmt.Errorf("write: %w", ErrDeviceGone)
			}
			return wrote, fmt.Errorf("write: %w", err)
		}
		if n == 0 {
			return wrote, fmt.Errorf("write: %w", ErrDeviceGone)
		}
	}
	return wrote, nil
}
