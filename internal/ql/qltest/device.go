// Package qltest is a scriptable printer for printq tests.
package qltest

import (
	"sync"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

// Read is one scripted ReadStatus result.
type Read struct {
	Status ql.Status
	Err    error
	After  time.Duration
}

// Write is one recorded write.
type Write struct {
	At    time.Time
	Bytes []byte
}

// Device is a scriptable ql.Device. Open errors are returned by Hub.Open.
type Device struct {
	mu sync.Mutex

	Now func() time.Time

	reads   []Read
	pending []ql.Status
	writes  []Write
	drains  int
	closes  int

	// GoneAfter, if > 0, fails the write that crosses that many bytes.
	GoneAfter int
	// FailOnWrite, if > 0, fails that 1-based Write call after one byte (mid-page).
	FailOnWrite int
	writeIndex  int
	written     int
	gone        bool

	// BytesPerSec throttles Write. 0 means instant. Tests use a fake Now.
	BytesPerSec int
}

func (d *Device) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Device) Write(b []byte) (int, error) {
	if d == nil {
		return 0, ql.ErrDeviceGone
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gone {
		return 0, ql.ErrDeviceGone
	}
	d.writeIndex++
	if d.FailOnWrite > 0 && d.writeIndex == d.FailOnWrite {
		d.gone = true
		if len(b) == 0 {
			return 0, ql.ErrDeviceGone
		}
		d.written++
		d.writes = append(d.writes, Write{At: d.now(), Bytes: append([]byte(nil), b[:1]...)})
		return 1, ql.ErrDeviceGone
	}
	n := len(b)
	if d.GoneAfter > 0 && d.written+n > d.GoneAfter {
		d.gone = true
		took := d.GoneAfter - d.written
		if took < 0 {
			took = 0
		}
		d.written += took
		if took > 0 {
			d.writes = append(d.writes, Write{At: d.now(), Bytes: append([]byte(nil), b[:took]...)})
		}
		return took, ql.ErrDeviceGone
	}
	d.written += n
	d.writes = append(d.writes, Write{At: d.now(), Bytes: append([]byte(nil), b...)})
	return n, nil
}

func (d *Device) ReadStatus(timeout time.Duration) (ql.Status, error) {
	if d == nil {
		return ql.Status{}, ql.ErrDeviceGone
	}
	d.mu.Lock()
	if len(d.pending) > 0 {
		st := d.pending[0]
		d.pending = d.pending[1:]
		d.mu.Unlock()
		return st, nil
	}
	if len(d.reads) == 0 {
		d.mu.Unlock()
		if timeout > 0 {
			time.Sleep(timeout)
		}
		return ql.Status{}, ql.ErrTimeout
	}
	step := d.reads[0]
	d.reads = d.reads[1:]
	d.mu.Unlock()
	if step.After > 0 {
		time.Sleep(step.After)
	}
	if step.Err != nil {
		return ql.Status{}, step.Err
	}
	return step.Status, nil
}

func (d *Device) Drain() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.drains++
	d.pending = nil
	d.mu.Unlock()
}

func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	d.closes++
	d.mu.Unlock()
	return nil
}

func (d *Device) Push(st ql.Status) {
	d.mu.Lock()
	d.pending = append(d.pending, st)
	d.mu.Unlock()
}

func (d *Device) Enqueue(steps ...Read) {
	d.mu.Lock()
	d.reads = append(d.reads, steps...)
	d.mu.Unlock()
}

func (d *Device) Writes() []Write {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Write, len(d.writes))
	for i, w := range d.writes {
		out[i] = Write{At: w.At, Bytes: append([]byte(nil), w.Bytes...)}
	}
	return out
}

func (d *Device) Drains() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.drains
}

// Hub scripts Open. A nil step opens Device (or a fresh one).
type Hub struct {
	mu      sync.Mutex
	Device  *Device
	opens   []error
	Opened  []string
	Scans   int
	ScanErr error
	ScanSt  ql.Status
	Path    string
}

func (h *Hub) Open(path string) (ql.Device, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Opened = append(h.Opened, path)
	if len(h.opens) > 0 {
		err := h.opens[0]
		h.opens = h.opens[1:]
		if err != nil {
			return nil, err
		}
	}
	if h.Device == nil {
		h.Device = &Device{}
	}
	return h.Device, nil
}

func (h *Hub) FailOpen(errs ...error) {
	h.mu.Lock()
	h.opens = append(h.opens, errs...)
	h.mu.Unlock()
}

func (h *Hub) OpenCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.Opened)
}

func (h *Hub) ScanCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Scans
}

func (h *Hub) SetScanErr(err error) {
	h.mu.Lock()
	h.ScanErr = err
	h.mu.Unlock()
}

func (h *Hub) Scan() (ql.Device, string, ql.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Scans++
	if h.ScanErr != nil {
		err := h.ScanErr
		return nil, "", ql.Status{}, err
	}
	if h.Device == nil {
		h.Device = &Device{}
	}
	path := h.Path
	if path == "" {
		path = "/dev/usb/lp0"
	}
	return h.Device, path, h.ScanSt, nil
}

// StatusReply is a ready status for media width/kind/length.
func StatusReply(width, length int, kind ql.MediaKind) ql.Status {
	return ql.Status{
		Type:          ql.StatusReply,
		MediaWidthMM:  width,
		MediaLengthMM: length,
		MediaKind:     kind,
		Printing:      false,
	}
}

func Waiting() ql.Status {
	return ql.Status{Type: ql.StatusPhase, Printing: false}
}

func Completed() ql.Status {
	return ql.Status{Type: ql.StatusCompleted, Printing: false}
}

func Printing() ql.Status {
	return ql.Status{Type: ql.StatusPhase, Printing: true}
}

func IsStatusRequest(b []byte) bool {
	return len(b) == 3 && b[0] == 0x1b && b[1] == 0x69 && b[2] == 'S'
}

func IsSettingRead(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x1b && b[1] == 0x69 && b[2] == 'U'
}

func IsJobByte(b []byte) bool {
	return len(b) > 3 && !IsSettingRead(b)
}
