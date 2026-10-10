package ql

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func shortTimeouts() Timeouts {
	return Timeouts{
		StatusReply:    20 * time.Millisecond,
		PhasePrinting:  15 * time.Millisecond,
		CompletedBase:  25 * time.Millisecond,
		CompletedPerMM: time.Millisecond,
		Cooling:        40 * time.Millisecond,
	}
}

func onePageJob(m Media) Job {
	return Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(4, 8)}}
}

func TestPrinter_multiPageSuccess(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{reads: []readStep{{st: doneStatus()}, {st: doneStatus()}, {st: doneStatus()}}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	job := Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(4, 8), NewBitmap(4, 8), NewBitmap(4, 8)}}
	res := pr.Print(context.Background(), job)
	if res.State != JobDone || res.Err != nil || res.PagesConfirmed != 3 {
		t.Fatalf("%+v", res)
	}
	writes, timeouts := f.snapshot()
	if len(writes) != 3 {
		t.Fatalf("writes %d", len(writes))
	}
	if !bytes.Equal(bytes.Join(writes, nil), mustEncode(t, p, job)) {
		t.Fatal("page bursts do not match encoder output")
	}
	if bytes.Contains(bytes.Join(writes, nil), []byte{0x1b, 0x69, 0x53}) {
		t.Fatal("status request during print")
	}
	if len(timeouts) == 0 || timeouts[0] < 25*time.Millisecond {
		t.Fatalf("timeouts %v", timeouts)
	}
}

func TestPrinter_errorMidJob(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{reads: []readStep{
		{st: doneStatus()},
		{st: Status{Type: StatusError, Errors: []string{"no media"}}},
	}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	job := Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(4, 4), NewBitmap(4, 4)}}
	res := pr.Print(context.Background(), job)
	if res.State != JobFailed || res.PagesConfirmed != 1 || !errors.Is(res.Err, ErrNoMedia) {
		t.Fatalf("%+v", res)
	}
}

func TestPrinter_coolingExtendsThenTimesOut(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{reads: []readStep{{st: Status{Type: StatusNotify, Notification: NotifyCoolingStart}}}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	res := pr.Print(context.Background(), onePageJob(m))
	if res.State == JobFailed || !res.Cooling || !errors.Is(res.Err, ErrCooling) {
		t.Fatalf("%+v", res)
	}
}

func TestPrinter_coolingThenSuccess(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{reads: []readStep{
		{st: Status{Type: StatusNotify, Notification: NotifyCoolingStart}},
		{st: Status{Type: StatusNotify, Notification: NotifyCoolingFinish}},
		{st: doneStatus()},
	}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	res := pr.Print(context.Background(), onePageJob(m))
	if res.State != JobDone || res.Err != nil || res.Cooling {
		t.Fatalf("%+v", res)
	}
}

func TestPrinter_timeout(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	start := time.Now()
	res := pr.Print(context.Background(), onePageJob(m))
	if res.State != JobFailed || !errors.Is(res.Err, ErrTimeout) || res.PagesConfirmed != 0 {
		t.Fatalf("%+v", res)
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout waited too long")
	}
}

func TestPrinter_deviceDisappeared(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{reads: []readStep{{err: ErrDeviceGone}}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	res := pr.Print(context.Background(), onePageJob(m))
	if res.State != JobFailed || !errors.Is(res.Err, ErrDeviceGone) {
		t.Fatalf("%v", res.Err)
	}
}

func TestPrinter_encodeErrorDoesNotOpen(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	opens := 0
	pr := NewPrinter(p, func() (Device, error) {
		opens++
		return &fakeDevice{}, nil
	}, shortTimeouts())
	res := pr.Print(context.Background(), Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(m.PrintPins+1, 2)}})
	if opens != 0 || !errors.Is(res.Err, ErrBitmapWidth) {
		t.Fatalf("opens %d err %v", opens, res.Err)
	}
}

func mustEncode(t *testing.T, p *Profile, j Job) []byte {
	t.Helper()
	b, err := NewEncoder().Encode(p, j)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
