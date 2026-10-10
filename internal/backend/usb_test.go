package backend

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql/qltest"
)

func fastTimeouts() ql.Timeouts {
	return ql.Timeouts{
		FirstStatusReply: 20 * time.Millisecond,
		StatusReply:      20 * time.Millisecond,
		PhasePrinting:    300 * time.Millisecond,
		CompletedBase:    300 * time.Millisecond,
		CompletedPerMM:   time.Millisecond,
		Cooling:          time.Second,
	}
}

func hubUSB(t *testing.T, p *ql.Profile, hub *qltest.Hub) *usb {
	t.Helper()
	return newUSB(p, func() (ql.Device, error) { return hub.Open("/dev/usb/lp0") }, fastTimeouts())
}

func written(d *qltest.Device) []byte {
	var out []byte
	for _, w := range d.Writes() {
		out = append(out, w.Bytes...)
	}
	return out
}

func TestUSBPrintWritesEncodedJob(t *testing.T) {
	p, job := label62(t)
	dev := &qltest.Device{}
	dev.Enqueue(qltest.Read{Status: qltest.Printing()}, qltest.Read{Status: qltest.Completed()})
	hub := &qltest.Hub{Device: dev}
	b := hubUSB(t, p, hub)
	if err := b.Print(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	want, _ := ql.NewEncoder().Encode(p, job)
	if !bytes.Equal(written(dev), want) {
		t.Fatalf("wrote %d bytes, want %d", len(written(dev)), len(want))
	}
	if hub.OpenCount() != 1 {
		t.Fatalf("opens %d", hub.OpenCount())
	}
	dev.Enqueue(qltest.Read{Status: qltest.Completed()})
	if err := b.Print(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if hub.OpenCount() != 2 {
		t.Fatalf("one open per label: opens %d", hub.OpenCount())
	}
}

func TestUSBOpenFailuresAreRetryable(t *testing.T) {
	p, job := label62(t)
	for _, sentinel := range []error{ql.ErrDeviceBusy, ql.ErrNoPrinter} {
		hub := &qltest.Hub{Device: &qltest.Device{}}
		hub.FailOpen(sentinel)
		err := hubUSB(t, p, hub).Print(context.Background(), job)
		if !errors.Is(err, ErrRetryable) || !errors.Is(err, sentinel) {
			t.Fatalf("%v: err %v", sentinel, err)
		}
		if len(hub.Device.Writes()) != 0 {
			t.Fatal("bytes written")
		}
	}
	hub := &qltest.Hub{}
	hub.FailOpen(ql.ErrUnsupportedOS)
	if err := hubUSB(t, p, hub).Print(context.Background(), job); err == nil || errors.Is(err, ErrRetryable) {
		t.Fatalf("unsupported OS: %v", err)
	}
}

func TestUSBMidWriteFailureIsFinal(t *testing.T) {
	p, job := label62(t)
	dev := &qltest.Device{FailOnWrite: 1}
	err := hubUSB(t, p, &qltest.Hub{Device: dev}).Print(context.Background(), job)
	if err == nil || errors.Is(err, ErrRetryable) {
		t.Fatalf("err %v", err)
	}
}

type busyWriter struct{ qltest.Device }

func (b *busyWriter) Write([]byte) (int, error) { return 0, ql.ErrDeviceBusy }

func TestUSBBusyAfterOpenIsFinal(t *testing.T) {
	p, job := label62(t)
	b := newUSB(p, func() (ql.Device, error) { return &busyWriter{}, nil }, fastTimeouts())
	err := b.Print(context.Background(), job)
	if !errors.Is(err, ql.ErrDeviceBusy) || errors.Is(err, ErrRetryable) {
		t.Fatalf("err %v", err)
	}
}

func TestUSBErrorStatusIsFinal(t *testing.T) {
	p, job := label62(t)
	dev := &qltest.Device{}
	dev.Enqueue(qltest.Read{Status: ql.Status{Type: ql.StatusError, Errors: []string{"cover open"}}})
	err := hubUSB(t, p, &qltest.Hub{Device: dev}).Print(context.Background(), job)
	if !errors.Is(err, ql.ErrCoverOpen) || errors.Is(err, ErrRetryable) {
		t.Fatalf("err %v", err)
	}
	if got := ReasonsForError(err); !reflect.DeepEqual(got, []string{"cover-open"}) {
		t.Fatalf("reasons %v", got)
	}
}

func TestUSBEncodeErrorIsFinal(t *testing.T) {
	p := profile(t)
	hub := &qltest.Hub{}
	err := hubUSB(t, p, hub).Print(context.Background(), ql.Job{Media: media(t, p, "62"), Options: ql.DefaultJobOptions()})
	if err == nil || errors.Is(err, ErrRetryable) || hub.OpenCount() != 0 {
		t.Fatalf("err %v opens %d", err, hub.OpenCount())
	}
}

func TestUSBPrintWithRetryRecovers(t *testing.T) {
	p, job := label62(t)
	dev := &qltest.Device{}
	dev.Enqueue(qltest.Read{Status: qltest.Completed()})
	hub := &qltest.Hub{Device: dev}
	hub.FailOpen(ql.ErrDeviceBusy, ql.ErrNoPrinter)
	var logs []string
	err := printWithRetry(context.Background(), hubUSB(t, p, hub), job, time.Second, func(s string) { logs = append(logs, s) }, time.Millisecond, 8*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if hub.OpenCount() != 3 || len(logs) != 2 {
		t.Fatalf("opens %d logs %v", hub.OpenCount(), logs)
	}
	want, _ := ql.NewEncoder().Encode(p, job)
	if !bytes.Equal(written(dev), want) {
		t.Fatal("label bytes differ")
	}
}

func TestUSBState(t *testing.T) {
	p := profile(t)
	dev := &qltest.Device{}
	dev.Enqueue(qltest.Read{Status: qltest.StatusReply(62, 0, ql.MediaContinuous)})
	hub := &qltest.Hub{Device: dev}
	b := hubUSB(t, p, hub)
	s, err := b.State(context.Background())
	if err != nil || s.Condition != Idle || s.Status == nil || s.Status.MediaWidthMM != 62 {
		t.Fatalf("idle: %+v %v", s, err)
	}
	if ws := dev.Writes(); len(ws) != 1 || !qltest.IsStatusRequest(ws[0].Bytes) {
		t.Fatalf("writes %v", ws)
	}

	dev.Enqueue(qltest.Read{Status: ql.Status{Type: ql.StatusError, Errors: []string{"no media"}}})
	s, _ = b.State(context.Background())
	if s.Condition != Error || !reflect.DeepEqual(s.Reasons, []string{"media-empty", "media-needed"}) {
		t.Fatalf("no media: %+v", s)
	}

	s, _ = b.State(context.Background())
	if s.Condition != Offline || !reflect.DeepEqual(s.Reasons, []string{"offline-report"}) {
		t.Fatalf("no reply: %+v", s)
	}

	hub.FailOpen(ql.ErrNoPrinter)
	s, _ = b.State(context.Background())
	if s.Condition != Offline || !reflect.DeepEqual(s.IPPReasons(), []string{"offline-report"}) || s.IPPState() != 5 {
		t.Fatalf("no printer: %+v", s)
	}

	hub.FailOpen(ql.ErrDeviceBusy)
	s, _ = b.State(context.Background())
	if s.Condition != Busy || len(s.Reasons) != 0 {
		t.Fatalf("busy: %+v", s)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.State(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestUSBStateWhilePrintingDoesNotTouchDevice(t *testing.T) {
	p, job := label62(t)
	dev := &qltest.Device{}
	dev.Enqueue(qltest.Read{Status: qltest.Completed(), After: 150 * time.Millisecond})
	hub := &qltest.Hub{Device: dev}
	b := hubUSB(t, p, hub)
	done := make(chan error, 1)
	go func() { done <- b.Print(context.Background(), job) }()
	deadline := time.Now().Add(time.Second)
	for len(dev.Writes()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("print never wrote")
		}
		time.Sleep(time.Millisecond)
	}
	s, err := b.State(context.Background())
	if err != nil || s.Condition != Busy {
		t.Fatalf("state %+v %v", s, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if hub.OpenCount() != 1 {
		t.Fatalf("opens %d", hub.OpenCount())
	}
}

func TestUSBClosed(t *testing.T) {
	p, job := label62(t)
	b := hubUSB(t, p, &qltest.Hub{})
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Print(context.Background(), job); !errors.Is(err, ErrClosed) {
		t.Fatalf("err %v", err)
	}
	if _, err := b.State(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("err %v", err)
	}
}

func TestNewUSBOffOS(t *testing.T) {
	p, _ := label62(t)
	b := NewUSB(p, "/nonexistent/lp9")
	s, err := b.State(context.Background())
	if err != nil || s.Condition != Offline {
		t.Fatalf("%+v %v", s, err)
	}
}

// cancelOnCompleted cancels the job just as the printer confirms the label.
type cancelOnCompleted struct {
	*qltest.Device
	cancel context.CancelFunc
}

func (d cancelOnCompleted) ReadStatus(timeout time.Duration) (ql.Status, error) {
	st, err := d.Device.ReadStatus(timeout)
	if err == nil && st.Type == ql.StatusCompleted {
		d.cancel()
	}
	return st, err
}

func TestUSBCancelAfterConfirmationCountsLabel(t *testing.T) {
	p, job := label62(t)
	dev := &qltest.Device{}
	dev.Enqueue(qltest.Read{Status: qltest.Printing()}, qltest.Read{Status: qltest.Completed()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newUSB(p, func() (ql.Device, error) { return cancelOnCompleted{dev, cancel}, nil }, fastTimeouts())
	if err := b.Print(ctx, job); err != nil {
		t.Fatalf("confirmed label reported as %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("ctx was not cancelled during the wait")
	}
}
