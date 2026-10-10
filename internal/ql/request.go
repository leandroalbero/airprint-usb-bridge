package ql

import (
	"errors"
	"fmt"
	"time"
)

// requestStatus sends ESC i S and retries once: on the QL-700 the first request after opening the
// usblp node often gets no reply (seen on hardware 2026-10-02).
func requestStatus(dev Device, timeout time.Duration) (Status, error) {
	return requestStatusAttempts(dev, timeout, timeout)
}

func requestStatusTimeouts(dev Device, t Timeouts) (Status, error) {
	t = normalizeTimeouts(t)
	return requestStatusAttempts(dev, t.FirstStatusReply, t.StatusReply)
}

func requestStatusAttempts(dev Device, first, second time.Duration) (Status, error) {
	var st Status
	var err error
	limits := [2]time.Duration{first, second}
	for attempt := 0; attempt < 2; attempt++ {
		drain(dev)
		if _, werr := dev.Write([]byte{0x1b, 0x69, 0x53}); werr != nil {
			return Status{}, werr
		}
		st, err = dev.ReadStatus(limits[attempt])
		if !errors.Is(err, ErrFraming) && !errors.Is(err, ErrTimeout) {
			return st, err
		}
	}
	return st, err
}

// readSetting skips replies that are not a settings report: a status reply that arrived late
// from an earlier request may still be in flight.
func readSetting(dev Device, key byte, timeout time.Duration) (Status, error) {
	drain(dev)
	if _, err := dev.Write([]byte{0x1b, 0x69, 0x55, key, 0x01}); err != nil {
		return Status{}, err
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err := dev.ReadStatus(time.Until(deadline))
		if err != nil {
			return Status{}, err
		}
		if st.Type == StatusSettings {
			return st, nil
		}
		if time.Until(deadline) <= 0 {
			return Status{}, fmt.Errorf("settings: no settings report for key 0x%02x: %w", key, ErrFraming)
		}
	}
}

type drainer interface{ Drain() }

func drain(dev Device) {
	if d, ok := dev.(drainer); ok {
		d.Drain()
	}
}
