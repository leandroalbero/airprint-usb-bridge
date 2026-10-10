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

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

type dryRun struct {
	prof *ql.Profile
	dir  string

	mu      sync.Mutex
	last    int
	scanned bool
	closed  atomic.Bool
}

// NewDryRun writes each job to dir as job-<n>.bin (the exact bytes a printer would get) and
// job-<n>.txt (ql.Decode). Numbering continues after the highest job-<n>.bin already in dir.
func NewDryRun(prof *ql.Profile, dir string) Backend {
	return &dryRun{prof: prof, dir: dir}
}

func (d *dryRun) Print(ctx context.Context, job ql.Job) error {
	if d.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bin, err := ql.NewEncoder().Encode(d.prof, job)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	text, err := ql.Decode(bin)
	if err != nil {
		text = "decode: " + err.Error()
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
	if err := os.WriteFile(base+".bin", bin, 0o600); err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	if err := os.WriteFile(base+".txt", []byte(text+"\n"), 0o600); err != nil {
		return fmt.Errorf("dry-run: %w", err)
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
