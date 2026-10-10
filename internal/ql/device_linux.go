//go:build linux

package ql

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const lpWriteTimeout = 2 * time.Second

type lpDevice struct {
	f       *os.File
	profile *Profile
	frames  frameBuf
}

func openDevice(p *Profile, path string) (Device, error) {
	if p == nil {
		return nil, fmt.Errorf("open device: nil profile")
	}
	if path == "" || path == "auto" {
		dev, _, _, err := openAuto(p, DefaultTimeouts())
		return dev, err
	}
	return openPath(p, path)
}

func openAuto(p *Profile, t Timeouts) (Device, string, Status, error) {
	paths, err := lpPaths()
	if err != nil {
		return nil, "", Status{}, fmt.Errorf("open auto: %w", err)
	}
	return scanOpen(paths, func(path string) (Device, error) {
		return openPath(p, path)
	}, func(dev Device) (Status, bool, error) {
		return probeModel(dev, p, t)
	})
}

func lpPaths() ([]string, error) {
	static := []string{"/dev/usb/lp0", "/dev/usb/lp1", "/dev/usb/lp2", "/dev/usb/lp3"}
	matches, err := filepath.Glob("/dev/usb/lp*")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(static)+len(matches))
	out := make([]string, 0, len(static)+len(matches))
	for _, path := range append(static, matches...) {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func openPath(p *Profile, path string) (Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, classifyOpenErr(path, err)
	}
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		cerr := f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("open %s: %w", path, ErrDeviceBusy)
		}
		if cerr != nil {
			return nil, fmt.Errorf("open %s: flock: %w", path, cerr)
		}
		return nil, fmt.Errorf("open %s: flock: %w", path, err)
	}
	return &lpDevice{f: f, profile: p}, nil
}

func (d *lpDevice) Close() error {
	if d == nil || d.f == nil {
		return nil
	}
	ferr := syscall.Flock(int(d.f.Fd()), syscall.LOCK_UN)
	cerr := d.f.Close()
	d.f = nil
	if cerr != nil {
		return cerr
	}
	return ferr
}

func (d *lpDevice) Write(b []byte) (int, error) {
	if d == nil || d.f == nil {
		return 0, fmt.Errorf("write: %w", ErrDeviceGone)
	}
	fd := int(d.f.Fd())
	return writeAll(writeLoop{
		write: d.f.Write,
		wait: func(rem time.Duration) error {
			return pollFD(fd, pollOut, rem)
		},
	}, b, lpWriteTimeout)
}

func (d *lpDevice) ReadStatus(timeout time.Duration) (Status, error) {
	if d == nil || d.f == nil {
		return Status{}, fmt.Errorf("read status: %w", ErrDeviceGone)
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	sawData := false
	for {
		if raw, ok := d.frames.next(); ok {
			return parseStatus(d.profile, raw)
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			if sawData {
				return Status{}, fmt.Errorf("read status: %w", ErrFraming)
			}
			return Status{}, fmt.Errorf("read status: %w", ErrTimeout)
		}
		if err := pollFD(int(d.f.Fd()), pollIn, rem); err != nil {
			if errors.Is(err, ErrTimeout) && sawData {
				return Status{}, fmt.Errorf("read status: %w", ErrFraming)
			}
			return Status{}, err
		}
		n, err := d.f.Read(buf)
		if n > 0 {
			sawData = true
			d.frames.push(buf[:n])
		}
		if err != nil && deviceGone(err) {
			return Status{}, fmt.Errorf("read status: %w", ErrDeviceGone)
		}
		if n == 0 {
			// usblp reports readable and then returns 0 bytes while the printer has nothing to send.
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func deviceGone(err error) bool {
	return errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)
}

// Drain discards buffered and pending input so a late reply cannot be mistaken for the next one.
func (d *lpDevice) Drain() {
	if d == nil || d.f == nil {
		return
	}
	d.frames = frameBuf{}
	buf := make([]byte, 4096)
	for {
		if err := pollFD(int(d.f.Fd()), pollIn, 30*time.Millisecond); err != nil {
			return
		}
		n, err := d.f.Read(buf)
		if n == 0 || err != nil {
			return
		}
	}
}
