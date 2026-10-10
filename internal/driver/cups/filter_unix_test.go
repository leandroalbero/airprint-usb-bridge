//go:build unix

package cups

import (
	"context"
	"errors"
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
	_, err := run.run(context.Background(), strings.NewReader(""))
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
