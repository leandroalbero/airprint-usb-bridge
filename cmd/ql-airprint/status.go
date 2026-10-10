package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ipp"
)

// statusCache rate-limits device queries: the ipp server polls every 2 s, but a USB status
// query opens the usblp node (probing every node for "auto") and would keep it busy for
// other tools.
type statusCache struct {
	be    backend.Backend
	every time.Duration
	logf  logFunc

	mu    sync.Mutex
	last  ipp.DeviceStatus
	at    time.Time // zero = stale
	known bool
}

func (c *statusCache) poll(ctx context.Context) ipp.DeviceStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.every <= 0 || (!c.at.IsZero() && time.Since(c.at) < c.every) {
		return c.last
	}
	st, err := c.be.State(ctx)
	if err != nil {
		return c.last
	}
	ds := deviceStatus(st)
	if !c.known || ds.State != c.last.State || !slices.Equal(ds.Reasons, c.last.Reasons) {
		c.logf("printer: %s", describeState(st))
	}
	c.last, c.at, c.known = ds, time.Now(), true
	return ds
}

// invalidate makes the next poll query the device, e.g. after a job.
func (c *statusCache) invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

func deviceStatus(st backend.State) ipp.DeviceStatus {
	ds := ipp.DeviceStatus{Message: st.Message}
	switch st.Condition {
	case backend.Busy:
		ds.State = ipp.StateProcessing
	case backend.Offline, backend.Error:
		ds.State = ipp.StateStopped
	}
	for _, r := range st.IPPReasons() {
		if r != "none" {
			ds.Reasons = append(ds.Reasons, r)
		}
	}
	return ds
}

func describeState(st backend.State) string {
	s := st.Condition.String()
	if r := st.IPPReasons(); len(r) > 0 && r[0] != "none" {
		s += " (" + strings.Join(r, ", ") + ")"
	}
	if st.Message != "" {
		s += ": " + st.Message
	}
	return s
}

func runStatus(ctx context.Context, o *options, out io.Writer, logf logFunc) error {
	p, err := newPrinter(o, logf)
	if err != nil {
		return err
	}
	defer p.be.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st, err := p.be.State(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "condition:      %s\n", st.Condition)
	fmt.Fprintf(out, "printer-state:  %d\n", st.IPPState())
	fmt.Fprintf(out, "state-reasons:  %s\n", strings.Join(st.IPPReasons(), ", "))
	if st.Message != "" {
		fmt.Fprintf(out, "message:        %s\n", st.Message)
	}
	if s := st.Status; s != nil {
		fmt.Fprintf(out, "media:          %d mm", s.MediaWidthMM)
		if s.MediaLengthMM > 0 {
			fmt.Fprintf(out, " x %d mm", s.MediaLengthMM)
		}
		fmt.Fprintf(out, " (kind 0x%02x)\n", byte(s.MediaKind))
		if len(s.Errors) > 0 {
			fmt.Fprintf(out, "errors:         %s\n", strings.Join(s.Errors, ", "))
		}
	}
	return nil
}
