//go:build unix

package cups

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFilterTimeoutKillsChildren checks that a timed-out wrapper filter takes the programs it
// started down with it.
func TestFilterTimeoutKillsChildren(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "wrapper")
	pidFile := filepath.Join(dir, "pid")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 600 &\necho $! > \"$PIDFILE\"\nsleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := filterRun{
		path:      script,
		args:      []string{"1", "user", "title", "1", ""},
		env:       []string{"PATH=/usr/bin:/bin", "PIDFILE=" + pidFile},
		timeout:   500 * time.Millisecond,
		maxOutput: 1 << 20,
	}
	_, err := run.run(context.Background(), strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the filter's child %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitGone fails the test unless process pid ends within a few seconds.
func waitGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A wrapper filter that exits (here with a failure) does not leave the programs it started
// running.
func TestFilterExitKillsChildren(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "wrapper")
	pidFile := filepath.Join(dir, "pid")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 600 </dev/null >/dev/null 2>&1 &\necho $! > \"$PIDFILE\"\necho out\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := filterRun{
		path:      script,
		args:      []string{"1", "user", "title", "1", ""},
		env:       []string{"PATH=/usr/bin:/bin", "PIDFILE=" + pidFile},
		timeout:   time.Minute,
		maxOutput: 1 << 20,
	}
	_, err := run.run(context.Background(), strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err %v", err)
	}
	waitGone(t, pidFile)
}

// A filter that has sent output gets a grace period after a cancel to end the device job, and is
// killed with its children when it does not.
func TestFilterCancelGrace(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "wrapper")
	pidFile := filepath.Join(dir, "pid")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho started\nsleep 600 &\necho $! > \"$PIDFILE\"\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := filterRun{
		path:      script,
		args:      []string{"1", "user", "title", "1", ""},
		env:       []string{"PATH=/usr/bin:/bin", "PIDFILE=" + pidFile},
		timeout:   time.Minute,
		maxOutput: 1 << 20,
		grace:     500 * time.Millisecond,
	}
	var out bytes.Buffer
	start := time.Now()
	n, err := run.run(ctx, strings.NewReader(""), writerFunc(func(p []byte) (int, error) {
		cancel()
		return out.Write(p)
	}))
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "after the job was cancelled") {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d < 400*time.Millisecond || d > 8*time.Second {
		t.Fatalf("killed after %v, want the 500ms grace", d)
	}
	if n != int64(out.Len()) || out.String() != "started\n" {
		t.Fatalf("passed on %d bytes %q", n, out.String())
	}
	waitGone(t, pidFile)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
