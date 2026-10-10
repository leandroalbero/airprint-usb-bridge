//go:build linux

package ql

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// Go 1.26's syscall package no longer exports Poll. ppoll is the kernel call.
const (
	pollIn   int16 = 0x1
	pollOut  int16 = 0x4
	pollErr  int16 = 0x8
	pollHup  int16 = 0x10
	pollNval int16 = 0x20
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
	ts := syscall.Timespec{
		Sec:  int64(timeout / time.Second),
		Nsec: int64(timeout % time.Second),
	}
	for {
		n, _, errno := syscall.Syscall6(
			syscall.SYS_PPOLL,
			uintptr(unsafe.Pointer(&pfd)),
			1,
			uintptr(unsafe.Pointer(&ts)),
			0, 0, 0,
		)
		if errno != 0 {
			if errno == syscall.EINTR {
				continue
			}
			return fmt.Errorf("poll: %w", errno)
		}
		if n == 0 {
			return fmt.Errorf("poll: %w", ErrTimeout)
		}
		if pfd.Revents&(pollHup|pollErr|pollNval) != 0 && pfd.Revents&events == 0 {
			return fmt.Errorf("poll: %w", ErrDeviceGone)
		}
		return nil
	}
}
