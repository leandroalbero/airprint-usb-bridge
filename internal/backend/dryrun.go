package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type dryRun struct {
	dir     string
	inspect func([]byte) (string, error)

	mu      sync.Mutex
	last    int
	scanned bool
	closed  atomic.Bool
}

// NewDryRun writes each unit to dir as job-<n>.bin (the exact bytes a printer would get) and, when
// inspect is non-nil, job-<n>.txt with its output. Numbering continues after the highest
// job-<n>.bin already in dir.
func NewDryRun(dir string, inspect func([]byte) (string, error)) Transport {
	return &dryRun{dir: dir, inspect: inspect}
}

func (d *dryRun) Send(ctx context.Context, data []byte) error {
	if d.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var text string
	if d.inspect != nil {
		var err error
		if text, err = d.inspect(data); err != nil {
			text = "decode: " + err.Error()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	if !d.scanned {
		d.last = highestJob(d.dir)
		d.scanned = true
	}
	n := d.last + 1
	base := filepath.Join(d.dir, fmt.Sprintf("job-%d", n))
	if err := os.WriteFile(base+".bin", data, 0o600); err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	if d.inspect != nil {
		if err := os.WriteFile(base+".txt", []byte(text+"\n"), 0o600); err != nil {
			return fmt.Errorf("dry-run: %w", err)
		}
	}
	d.last = n
	return nil
}

func highestJob(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	hi := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "job-") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "job-"), ".bin"))
		if err == nil && n > hi {
			hi = n
		}
	}
	return hi
}

func (d *dryRun) State(ctx context.Context) (State, error) {
	if d.closed.Load() {
		return State{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	return State{Condition: Idle}, nil
}

func (d *dryRun) Close() error {
	d.closed.Store(true)
	return nil
}
