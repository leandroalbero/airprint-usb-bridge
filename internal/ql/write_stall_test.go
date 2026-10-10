package ql

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestWriteAll_progressResetsStall(t *testing.T) {
	const lines = 5000
	page := make([]byte, lines*90)
	t0 := time.Unix(1_700_000_000, 0)
	calls := 0
	n, err := writeAll(writeLoop{
		write: func(b []byte) (int, error) {
			chunk := 10 * 1024
			if chunk > len(b) {
				chunk = len(b)
			}
			return chunk, nil
		},
		wait: func(time.Duration) error { return nil },
		now: func() time.Time {
			calls++
			return t0.Add(time.Duration(calls) * time.Second)
		},
	}, page, 2*time.Second)
	if err != nil || n != len(page) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestWriteAll_stallWithoutProgressTimesOut(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	calls := 0
	_, err := writeAll(writeLoop{
		write: func([]byte) (int, error) { return 0, syscall.EAGAIN },
		wait:  func(time.Duration) error { return nil },
		now: func() time.Time {
			calls++
			if calls >= 3 {
				return t0.Add(2 * time.Second)
			}
			return t0
		},
	}, []byte{1, 2, 3}, 2*time.Second)
	if !errors.Is(err, ErrTimeout) && err == nil {
		t.Fatalf("err %v", err)
	}
}
