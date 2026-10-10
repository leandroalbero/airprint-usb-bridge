package ql

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestScanDevices_skipsAbsentAndUsesNext(t *testing.T) {
	want := &fakeDevice{}
	var opened []string
	got, err := scanDevices([]string{"/dev/usb/lp0", "/dev/usb/lp1", "/dev/usb/lp2"},
		func(path string) (Device, error) {
			opened = append(opened, path)
			switch path {
			case "/dev/usb/lp0":
				return nil, fmt.Errorf("open %s: %w", path, syscall.ENODEV)
			case "/dev/usb/lp1":
				return nil, fmt.Errorf("open %s: %w", path, syscall.ENXIO)
			default:
				return want, nil
			}
		},
		func(Device) (bool, error) { return true, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("wrong device")
	}
	if len(opened) != 3 || opened[2] != "/dev/usb/lp2" {
		t.Fatalf("opened %v", opened)
	}
}

func TestScanDevices_noneConnected(t *testing.T) {
	_, err := scanDevices([]string{"/dev/usb/lp0", "/dev/usb/lp1", "/dev/usb/lp2", "/dev/usb/lp3"},
		func(path string) (Device, error) {
			if path == "/dev/usb/lp1" {
				return nil, fmt.Errorf("open %s: %w", path, syscall.ENXIO)
			}
			return nil, fmt.Errorf("open %s: %w", path, syscall.ENODEV)
		},
		func(Device) (bool, error) {
			t.Fatal("probed an absent node")
			return false, nil
		},
	)
	if !errors.Is(err, ErrNoPrinter) {
		t.Fatalf("err %v", err)
	}
	if err == nil || err.Error() == "" {
		t.Fatal("empty error")
	}
}

func TestScanDevices_busyWhenOnlyLocked(t *testing.T) {
	_, err := scanDevices([]string{"/dev/usb/lp0", "/dev/usb/lp1"},
		func(path string) (Device, error) {
			if path == "/dev/usb/lp0" {
				return nil, fmt.Errorf("open %s: %w", path, ErrDeviceBusy)
			}
			return nil, fmt.Errorf("open %s: %w", path, syscall.ENODEV)
		},
		func(Device) (bool, error) { return false, nil },
	)
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("err %v", err)
	}
}

func TestScanDevices_probeMissClosesAndContinues(t *testing.T) {
	miss := &fakeDevice{}
	hit := &fakeDevice{}
	got, err := scanDevices([]string{"/dev/usb/lp0", "/dev/usb/lp1"},
		func(path string) (Device, error) {
			if path == "/dev/usb/lp0" {
				return miss, nil
			}
			return hit, nil
		},
		func(d Device) (bool, error) { return d == hit, nil },
	)
	if err != nil || got != hit {
		t.Fatalf("got %v err %v", got, err)
	}
	if miss.closes != 1 {
		t.Fatalf("closes %d", miss.closes)
	}
}
