//go:build linux

package backend

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

const (
	pollIn   int16 = 0x1
	pollOut  int16 = 0x4
	pollErr  int16 = 0x8
	pollHup  int16 = 0x10
	pollNval int16 = 0x20

	lpGetStatus = 0x060b
	// LPIOC_GET_DEVICE_ID(len) is _IOC(_IOC_READ, 'P', 1, len); the asm-generic encoding is
	// shared by amd64 and arm64.
	lpDeviceIDLen = 1024
	lpGetDeviceID = 2<<30 | lpDeviceIDLen<<16 | 'P'<<8 | 1
)

type pollFd struct {
	Fd      int32
	Events  int16
	Revents int16
}

func pollFD(fd int, events int16, timeout time.Duration) error {
	if timeout < time.Millisecond {
		timeout = time.Millisecond
	}
	pfd := pollFd{Fd: int32(fd), Events: events}
	ts := syscall.Timespec{Sec: int64(timeout / time.Second), Nsec: int64(timeout % time.Second)}
	for {
		n, _, errno := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&pfd)), 1,
			uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
		if errno != 0 {
			if errno == syscall.EINTR {
				continue
			}
			return fmt.Errorf("poll: %w", errno)
		}
		if n == 0 {
			return ErrTimeout
		}
		if pfd.Revents&(pollHup|pollErr|pollNval) != 0 && pfd.Revents&events == 0 {
			return fmt.Errorf("poll: %w", ErrNoPrinter)
		}
		return nil
	}
}

type lpPort struct {
	fd    int
	stall time.Duration
}

// OpenUSB opens a usblp node for exclusive use. EBUSY or a held lock wraps ErrDeviceBusy; a
// missing or unbound node wraps ErrNoPrinter.
func OpenUSB(path string, stall time.Duration) (Port, error) {
	fd, err := openLP(path)
	if err != nil {
		return nil, err
	}
	if stall <= 0 {
		stall = DefaultStall
	}
	return &lpPort{fd: fd, stall: stall}, nil
}

func openLP(path string) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, classifyOpenErr(path, err)
	}
	// Another bridge or a CUPS backend may hold the node; usblp itself allows one open.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return -1, fmt.Errorf("open %s: %w", path, ErrDeviceBusy)
		}
		return -1, fmt.Errorf("open %s: flock: %w", path, err)
	}
	return fd, nil
}

func classifyOpenErr(path string, err error) error {
	switch {
	case errors.Is(err, syscall.EBUSY):
		return fmt.Errorf("open %s: %w", path, ErrDeviceBusy)
	case errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.ENOENT):
		return fmt.Errorf("open %s: %w", path, ErrNoPrinter)
	}
	return fmt.Errorf("open %s: %w", path, err)
}

func deviceGone(err error) bool {
	return errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENXIO)
}

func (p *lpPort) Write(b []byte) (int, error) {
	if p.fd < 0 {
		return 0, ErrClosed
	}
	wrote := 0
	deadline := time.Now().Add(p.stall)
	for wrote < len(b) {
		rem := time.Until(deadline)
		if rem <= 0 {
			return wrote, fmt.Errorf("write: %w", ErrTimeout)
		}
		if err := pollFD(p.fd, pollOut, rem); err != nil {
			if errors.Is(err, ErrTimeout) {
				continue
			}
			return wrote, fmt.Errorf("write: %w", err)
		}
		n, err := syscall.Write(p.fd, b[wrote:])
		if n > 0 {
			wrote += n
			deadline = time.Now().Add(p.stall)
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			if deviceGone(err) {
				return wrote, fmt.Errorf("write: %w (%v)", ErrNoPrinter, err)
			}
			return wrote, fmt.Errorf("write: %w", err)
		}
		if n == 0 {
			return wrote, fmt.Errorf("write: %w", ErrNoPrinter)
		}
	}
	return wrote, nil
}

func (p *lpPort) Read(b []byte, timeout time.Duration) (int, error) {
	if p.fd < 0 {
		return 0, ErrClosed
	}
	deadline := time.Now().Add(timeout)
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			return 0, ErrTimeout
		}
		if err := pollFD(p.fd, pollIn, rem); err != nil {
			return 0, err
		}
		n, err := syscall.Read(p.fd, b)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			if deviceGone(err) {
				return 0, fmt.Errorf("read: %w (%v)", ErrNoPrinter, err)
			}
			return 0, fmt.Errorf("read: %w", err)
		}
		// usblp reports readable and then returns 0 bytes while the printer has nothing to send.
		time.Sleep(10 * time.Millisecond)
	}
}

// Close waits for POLLOUT first: usblp completes non-blocking writes asynchronously and
// kills the pending URB on release, which would drop the end of the job.
func (p *lpPort) Close() error {
	if p.fd < 0 {
		return nil
	}
	werr := pollFD(p.fd, pollOut, p.stall)
	_ = syscall.Flock(p.fd, syscall.LOCK_UN)
	cerr := syscall.Close(p.fd)
	p.fd = -1
	if werr != nil {
		return fmt.Errorf("flush: %w", werr)
	}
	return cerr
}

func lpStatusByte(fd int) (byte, error) {
	var st int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), lpGetStatus, uintptr(unsafe.Pointer(&st))); errno != 0 {
		return 0, errno
	}
	return byte(st), nil
}

func usbStatus(path string) (State, error) {
	fd, err := openLP(path)
	if err != nil {
		return State{}, err
	}
	defer func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
	}()
	b, err := lpStatusByte(fd)
	if err != nil {
		return State{Condition: Unknown, Message: "status: " + err.Error()}, nil
	}
	return stateFromLPStatus(b), nil
}

// ioctlDeviceID asks usblp for the device ID with LPIOC_GET_DEVICE_ID. The reply starts with its
// own length, big-endian, the two length bytes included.
func ioctlDeviceID(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", classifyOpenErr(path, err)
	}
	defer syscall.Close(fd)
	buf := make([]byte, lpDeviceIDLen)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), lpGetDeviceID, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", fmt.Errorf("LPIOC_GET_DEVICE_ID %s: %w", path, errno)
	}
	return parseIoctlID(buf), nil
}
