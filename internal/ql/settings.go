package ql

import (
	"context"
	"fmt"
)

// GetSetting reads one ESC i U key. exclusive waits until the queue is idle.
func (pr *Printer) GetSetting(ctx context.Context, key byte) (byte, error) {
	if pr == nil {
		return 0, fmt.Errorf("settings: nil printer")
	}
	var value byte
	err := pr.exclusive(ctx, func(dev Device) error {
		st, rerr := readSetting(dev, key, pr.timeouts.StatusReply)
		if rerr != nil {
			return rerr
		}
		value = st.Raw[30]
		return nil
	})
	return value, err
}

// SetSetting writes ESC i U, requires a clean status reply, then reads the key back.
func (pr *Printer) SetSetting(ctx context.Context, key, value byte) error {
	if pr == nil {
		return fmt.Errorf("settings: nil printer")
	}
	return pr.exclusive(ctx, func(dev Device) error {
		if _, err := dev.Write([]byte{0x1b, 0x69, 0x55, key, 0x00, value}); err != nil {
			return err
		}
		st, err := requestStatusTimeouts(dev, pr.timeouts)
		if err != nil {
			return err
		}
		if st.Type != StatusReply || len(st.Errors) != 0 {
			return fmt.Errorf("settings: status %s errors %v", st.Type, st.Errors)
		}
		back, err := readSetting(dev, key, pr.timeouts.StatusReply)
		if err != nil {
			return err
		}
		if back.Type != StatusSettings || back.Raw[30] != value {
			return fmt.Errorf("settings: readback %s value 0x%02x", back.Type, back.Raw[30])
		}
		return nil
	})
}
