//go:build !linux

package backend

import (
	"fmt"
	"time"
)

// OpenUSB needs Linux usblp.
func OpenUSB(path string, _ time.Duration) (Port, error) {
	return nil, fmt.Errorf("open %s: %w", path, ErrUnsupportedOS)
}

func usbStatus(path string) (State, error) {
	return State{}, fmt.Errorf("status %s: %w", path, ErrUnsupportedOS)
}

func ioctlDeviceID(path string) (string, error) {
	return "", fmt.Errorf("device id %s: %w", path, ErrUnsupportedOS)
}
