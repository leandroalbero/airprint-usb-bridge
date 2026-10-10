package ql

import (
	"sync"
	"time"
)

type readStep struct {
	st  Status
	err error
}

type fakeDevice struct {
	mu       sync.Mutex
	writes   [][]byte
	reads    []readStep
	idx      int
	timeouts []time.Duration
	closes   int
	delay    time.Duration
	writeErr error
	onRead   func(n int)
}

func (f *fakeDevice) Write(b []byte) (int, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.writes = append(f.writes, append([]byte(nil), b...))
	return len(b), nil
}

func (f *fakeDevice) ReadStatus(timeout time.Duration) (Status, error) {
	f.mu.Lock()
	f.timeouts = append(f.timeouts, timeout)
	n := f.idx + 1
	var step *readStep
	if f.idx < len(f.reads) {
		s := f.reads[f.idx]
		step = &s
		f.idx++
	}
	hook := f.onRead
	f.mu.Unlock()
	if step == nil {
		if timeout > 0 {
			time.Sleep(timeout)
		}
		return Status{}, ErrTimeout
	}
	if hook != nil {
		hook(n)
	}
	if step.err != nil {
		return Status{}, step.err
	}
	return step.st, nil
}

func (f *fakeDevice) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	return nil
}

func (f *fakeDevice) snapshot() (writes [][]byte, timeouts []time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writes {
		writes = append(writes, append([]byte(nil), w...))
	}
	timeouts = append(timeouts, f.timeouts...)
	return writes, timeouts
}

func doneStatus() Status { return Status{Type: StatusCompleted, Printing: false} }

func phaseStatus(printing bool) Status {
	return Status{Type: StatusPhase, Printing: printing}
}
