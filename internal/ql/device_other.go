//go:build !linux

package ql

import "fmt"

func openDevice(_ *Profile, path string) (Device, error) {
	return nil, fmt.Errorf("open %s: %w", path, ErrUnsupportedOS)
}

func openAuto(_ *Profile, _ Timeouts) (Device, string, Status, error) {
	return nil, "", Status{}, fmt.Errorf("open auto: %w", ErrUnsupportedOS)
}
