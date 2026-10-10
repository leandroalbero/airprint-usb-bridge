package ql

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

func TestWriteAll_retriesEAGAINAndShortWrites(t *testing.T) {
	payload := []byte{1, 2, 3, 4, 5}
	t0 := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name    string
		steps   []writeStep
		now     func(int) time.Time
		waitErr error
		wantErr error
		wantN   int
	}{
		{
			name:  "eagain then full",
			steps: []writeStep{{err: syscall.EAGAIN}, {n: len(payload)}},
			wantN: len(payload),
		},
		{
			name:  "short then rest",
			steps: []writeStep{{n: 2}, {n: 3}},
			wantN: len(payload),
		},
		{
			name:  "eagain short then rest",
			steps: []writeStep{{n: 1, err: syscall.EAGAIN}, {n: 2}, {n: 2}},
			wantN: len(payload),
		},
		{
			name:    "stall past timeout",
			steps:   []writeStep{{err: syscall.EAGAIN}, {err: syscall.EAGAIN}},
			now:     func(call int) time.Time { return stallClock(t0, call) },
			wantErr: ErrTimeout,
		},
		{
			name:    "wait timeout",
			steps:   []writeStep{{n: len(payload)}},
			waitErr: fmt.Errorf("poll: %w", ErrTimeout),
			wantErr: ErrTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &scriptWriter{steps: tt.steps}
			now := func() time.Time { return t0 }
			if tt.now != nil {
				calls := 0
				now = func() time.Time {
					calls++
					return tt.now(calls)
				}
			}
			n, err := writeAll(writeLoop{
				write: w.write,
				wait: func(time.Duration) error {
					return tt.waitErr
				},
				now: now,
			}, payload, time.Second)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.wantN || string(w.got) != string(payload) {
				t.Fatalf("n=%d got %v", n, w.got)
			}
		})
	}
}

func stallClock(t0 time.Time, call int) time.Time {
	if call >= 3 {
		return t0.Add(2 * time.Second)
	}
	return t0
}

type writeStep struct {
	n   int
	err error
}

type scriptWriter struct {
	steps []writeStep
	i     int
	got   []byte
}

func (w *scriptWriter) write(b []byte) (int, error) {
	if w.i >= len(w.steps) {
		return 0, errors.New("unexpected write")
	}
	s := w.steps[w.i]
	w.i++
	n := s.n
	if n > len(b) {
		n = len(b)
	}
	w.got = append(w.got, b[:n]...)
	return n, s.err
}
