package ql

import (
	"errors"
	"fmt"
)

// scanDevices opens candidates until one answers. ENODEV and ENXIO mean the
// static node exists but no printer is bound, so the scan continues.
func scanDevices(paths []string, open func(string) (Device, error), probe func(Device) (bool, error)) (Device, error) {
	var busy, last error
	for _, path := range paths {
		dev, err := open(path)
		if err != nil {
			if errors.Is(err, ErrDeviceBusy) {
				busy = err
			}
			continue
		}
		ok, perr := probe(dev)
		if perr != nil || !ok {
			_ = dev.Close()
			if perr != nil {
				last = fmt.Errorf("%s: %w", path, perr)
			} else {
				last = fmt.Errorf("%s: not a %s", path, "matching model")
			}
			continue
		}
		return dev, nil
	}
	if busy != nil {
		return nil, busy
	}
	if last != nil {
		return nil, fmt.Errorf("open auto: %w (last probe: %v)", ErrNoPrinter, last)
	}
	return nil, fmt.Errorf("open auto: %w", ErrNoPrinter)
}

// scanOpen is scanDevices that also returns the winning path and the probe status.
func scanOpen(paths []string, open func(string) (Device, error), probe func(Device) (Status, bool, error)) (Device, string, Status, error) {
	var busy, last error
	for _, path := range paths {
		dev, err := open(path)
		if err != nil {
			if errors.Is(err, ErrDeviceBusy) {
				busy = err
			}
			continue
		}
		st, ok, perr := probe(dev)
		if perr != nil || !ok {
			_ = dev.Close()
			if perr != nil {
				last = fmt.Errorf("%s: %w", path, perr)
			} else {
				last = fmt.Errorf("%s: not a matching model", path)
			}
			continue
		}
		return dev, path, st, nil
	}
	if busy != nil {
		return nil, "", Status{}, busy
	}
	if last != nil {
		return nil, "", Status{}, fmt.Errorf("open auto: %w (last probe: %v)", ErrNoPrinter, last)
	}
	return nil, "", Status{}, fmt.Errorf("open auto: %w", ErrNoPrinter)
}
