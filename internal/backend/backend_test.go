package backend

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStateIPP(t *testing.T) {
	cases := []struct {
		s       State
		state   int
		reasons []string
	}{
		{State{Condition: Idle}, 3, []string{"none"}},
		{State{Condition: Unknown}, 3, []string{"none"}},
		{State{Condition: Busy}, 4, []string{"none"}},
		{State{Condition: Offline, Reasons: []string{"offline-report"}}, 5, []string{"offline-report"}},
		{State{Condition: Error, Reasons: []string{"media-empty", "media-needed"}}, 5, []string{"media-empty-error", "media-needed-error"}},
		{State{Condition: Busy, Reasons: []string{"other"}}, 4, []string{"other-report"}},
	}
	for _, c := range cases {
		if c.s.IPPState() != c.state || !reflect.DeepEqual(c.s.IPPReasons(), c.reasons) {
			t.Errorf("%v: %d %v, want %d %v", c.s.Condition, c.s.IPPState(), c.s.IPPReasons(), c.state, c.reasons)
		}
	}
}

func TestStateFromError(t *testing.T) {
	if s := StateFromError(fmt.Errorf("open: %w", ErrDeviceBusy)); s.Condition != Busy {
		t.Fatalf("busy: %+v", s)
	}
	if s := StateFromError(fmt.Errorf("open: %w", ErrNoPrinter)); s.Condition != Offline || !reflect.DeepEqual(s.Reasons, []string{"offline-report"}) {
		t.Fatalf("absent: %+v", s)
	}
}

type scripted struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (s *scripted) send(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	if len(s.errs) > 1 {
		s.errs = s.errs[1:]
	}
	return err
}

func busyErr() error { return Retryable(fmt.Errorf("open: %w", ErrDeviceBusy)) }

func TestRetrySucceedsAfterRetryable(t *testing.T) {
	b := &scripted{errs: []error{busyErr(), busyErr(), nil}}
	var logs []string
	err := RetryBackoff(context.Background(), b.send, time.Second, func(s string) { logs = append(logs, s) }, time.Millisecond, 4*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if b.calls != 3 || len(logs) != 2 {
		t.Fatalf("calls %d logs %v", b.calls, logs)
	}
	if !strings.Contains(logs[0], "device busy") || !strings.Contains(logs[0], "retrying in 1ms") || !strings.Contains(logs[1], "retrying in 2ms") {
		t.Fatalf("logs %v", logs)
	}
}

func TestRetryNeverRetriesFinal(t *testing.T) {
	final := fmt.Errorf("print: %w", ErrPartial)
	b := &scripted{errs: []error{final}}
	err := RetryBackoff(context.Background(), b.send, time.Second, nil, time.Millisecond, time.Millisecond)
	if !errors.Is(err, ErrPartial) || b.calls != 1 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
	// Busy without ErrRetryable means bytes went out.
	b = &scripted{errs: []error{fmt.Errorf("print: %w", ErrDeviceBusy)}}
	if err := RetryBackoff(context.Background(), b.send, time.Second, nil, time.Millisecond, time.Millisecond); err == nil || b.calls != 1 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
}

func TestRetryDeadline(t *testing.T) {
	b := &scripted{errs: []error{busyErr()}}
	start := time.Now()
	err := RetryBackoff(context.Background(), b.send, 40*time.Millisecond, nil, 5*time.Millisecond, 10*time.Millisecond)
	el := time.Since(start)
	if !errors.Is(err, ErrRetryable) || !errors.Is(err, ErrDeviceBusy) || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("err %v", err)
	}
	if el < 40*time.Millisecond || el > time.Second {
		t.Fatalf("elapsed %s", el)
	}
	if b.calls < 3 {
		t.Fatalf("calls %d", b.calls)
	}
}

func TestRetryBackoffCaps(t *testing.T) {
	b := &scripted{errs: []error{busyErr(), busyErr(), busyErr(), busyErr(), busyErr(), nil}}
	var logs []string
	if err := RetryBackoff(context.Background(), b.send, time.Second, func(s string) { logs = append(logs, s) }, time.Millisecond, 4*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	want := []string{"1ms", "2ms", "4ms", "4ms", "4ms"}
	for i, w := range want {
		if !strings.HasSuffix(logs[i], "retrying in "+w) {
			t.Fatalf("log %d %q, want %s", i, logs[i], w)
		}
	}
}

func TestRetryContext(t *testing.T) {
	b := &scripted{errs: []error{busyErr()}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := RetryBackoff(ctx, b.send, time.Minute, nil, time.Hour, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if b.calls != 1 {
		t.Fatalf("calls %d", b.calls)
	}
}

func TestRetryDefaults(t *testing.T) {
	b := &scripted{}
	if err := Retry(context.Background(), b.send, 0, nil); err != nil || b.calls != 1 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
}
