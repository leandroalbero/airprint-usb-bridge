package ql

import (
	"errors"
	"fmt"
	"syscall"
)

// classifyOpenErr maps usblp open(2) failures. EBUSY means another process holds the
// node. ENODEV, ENXIO and ENOENT mean the node is absent
// or unbound, which a fixed printer.device path must report as offline, like auto.
func classifyOpenErr(path string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDeviceBusy) || errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("open %s: %w", path, ErrDeviceBusy)
	}
	if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("open %s: %w", path, ErrNoPrinter)
	}
	return fmt.Errorf("open %s: %w", path, err)
}
