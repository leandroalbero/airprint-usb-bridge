package ql

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestClassifyOpenErr(t *testing.T) {
	busy := classifyOpenErr("/dev/usb/lp0", syscall.EBUSY)
	if !errors.Is(busy, ErrDeviceBusy) {
		t.Fatalf("EBUSY: %v", busy)
	}
	for _, err := range []error{syscall.ENODEV, syscall.ENXIO, syscall.ENOENT} {
		got := classifyOpenErr("/dev/usb/lp0", err)
		if !errors.Is(got, ErrNoPrinter) {
			t.Fatalf("%v -> %v, want ErrNoPrinter", err, got)
		}
		if errors.Is(got, ErrDeviceBusy) {
			t.Fatalf("%v mapped to busy", err)
		}
	}
}

func TestRequestStatus_firstAttemptUsesShorterTimeout(t *testing.T) {
	dev := &fakeDevice{reads: []readStep{{err: ErrTimeout}, {st: Status{Type: StatusReply}}}}
	_, err := requestStatusAttempts(dev, 600*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, timeouts := dev.snapshot()
	if len(timeouts) != 2 || timeouts[0] != 600*time.Millisecond || timeouts[1] != 2*time.Second {
		t.Fatalf("timeouts %v", timeouts)
	}
}

func TestProbe_firstAttemptUsesFirstStatusReply(t *testing.T) {
	p := testProfile(t)
	p.StatusModelByte3 = 0x34
	p.StatusModelByte4 = 0x35
	dev := &fakeDevice{reads: []readStep{
		{err: ErrTimeout},
		{st: Status{Type: StatusReply, ModelByte3: 0x34, ModelByte4: 0x35}},
	}}
	tmo := DefaultTimeouts()
	tmo.FirstStatusReply = 600 * time.Millisecond
	tmo.StatusReply = 2 * time.Second
	st, ok, err := probeModel(dev, p, tmo)
	if err != nil || !ok || st.ModelByte3 != 0x34 {
		t.Fatalf("st=%+v ok=%v err=%v", st, ok, err)
	}
	_, timeouts := dev.snapshot()
	if len(timeouts) != 2 || timeouts[0] != 600*time.Millisecond || timeouts[1] != 2*time.Second {
		t.Fatalf("timeouts %v", timeouts)
	}
}

type orderDev struct {
	fakeDevice
	pending []Status
	drains  int
	order   []string
}

func (d *orderDev) Drain() {
	d.mu.Lock()
	d.drains++
	d.pending = nil
	d.order = append(d.order, "drain")
	d.mu.Unlock()
}

func (d *orderDev) Write(b []byte) (int, error) {
	kind := "write"
	if len(b) >= 2 && b[len(b)-2] == 0x1b && b[len(b)-1] == 0x40 && len(b) >= 200 {
		kind = "invalidate"
	} else if len(b) == 3 && b[0] == 0x1b && b[1] == 0x69 && b[2] == 'S' {
		kind = "status-req"
	} else if len(b) > 3 {
		kind = "page"
	}
	d.mu.Lock()
	d.order = append(d.order, kind)
	d.mu.Unlock()
	return d.fakeDevice.Write(b)
}

func (d *orderDev) ReadStatus(timeout time.Duration) (Status, error) {
	d.mu.Lock()
	if len(d.pending) > 0 {
		st := d.pending[0]
		d.pending = d.pending[1:]
		d.order = append(d.order, "stale")
		d.mu.Unlock()
		return st, nil
	}
	d.mu.Unlock()
	st, err := d.fakeDevice.ReadStatus(timeout)
	if err == nil && waitingToReceive(st) {
		d.mu.Lock()
		d.order = append(d.order, "waiting")
		d.mu.Unlock()
	}
	return st, err
}

func (d *orderDev) events() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.order...)
}

type recObs struct {
	calls      []string
	stopAt     int
	request    bool
	late       *orderDev
	prechecked bool
}

func (o *recObs) Precheck(request func() (Status, error)) error {
	o.calls = append(o.calls, "precheck")
	o.prechecked = true
	if o.request {
		if _, err := request(); err != nil {
			return err
		}
	}
	if o.late != nil {
		o.late.mu.Lock()
		o.late.pending = append(o.late.pending, Status{Type: StatusError, Errors: []string{"cover open"}})
		o.late.mu.Unlock()
	}
	return nil
}
func (o *recObs) Sending(page int)  { o.calls = append(o.calls, "sending") }
func (o *recObs) Printing(page int) { o.calls = append(o.calls, "printing") }
func (o *recObs) Status(Status)     {}
func (o *recObs) PageDone(page int) { o.calls = append(o.calls, "pagedone") }
func (o *recObs) StopBefore(page int) bool {
	o.calls = append(o.calls, "stop")
	return o.stopAt >= 0 && page == o.stopAt
}

func waitingReply() Status { return Status{Type: StatusPhase, Printing: false} }

func TestPrintObserved_orderAndStopAfterWaiting(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	pages := []*Bitmap{NewBitmap(8, 2), NewBitmap(8, 2), NewBitmap(8, 2)}
	dev := &orderDev{fakeDevice: fakeDevice{reads: []readStep{
		{st: doneStatus()}, {st: waitingReply()},
		{st: doneStatus()}, {st: waitingReply()},
		{st: doneStatus()}, {st: waitingReply()},
	}}}
	obs := &recObs{stopAt: -1}
	pr := NewPrinter(p, func() (Device, error) { return dev, nil }, DefaultTimeouts())
	res := pr.PrintObserved(context.Background(), Job{Media: m, Options: DefaultJobOptions(), Pages: pages}, obs)
	if res.Err != nil || res.State != JobDone || res.PagesWritten != 3 || res.PagesConfirmed != 3 {
		t.Fatalf("%+v", res)
	}
	want := []string{"precheck", "sending", "printing", "pagedone", "stop", "sending", "printing", "pagedone", "stop", "sending", "printing", "pagedone"}
	if len(obs.calls) != len(want) {
		t.Fatalf("calls %v", obs.calls)
	}
	for i := range want {
		if obs.calls[i] != want[i] {
			t.Fatalf("calls %v", obs.calls)
		}
	}
	ev := dev.events()
	if !seqHas(ev, "page", "waiting") {
		t.Fatalf("order %v", ev)
	}
	if writesBetween(ev, "page", "waiting") {
		t.Fatalf("wrote during page wait: %v", ev)
	}

	dev2 := &orderDev{fakeDevice: fakeDevice{reads: []readStep{
		{st: doneStatus()}, {st: waitingReply()},
	}}}
	obs2 := &recObs{stopAt: 1}
	pr2 := NewPrinter(p, func() (Device, error) { return dev2, nil }, DefaultTimeouts())
	res = pr2.PrintObserved(context.Background(), Job{Media: m, Options: DefaultJobOptions(), Pages: pages}, obs2)
	if !res.Stopped || res.PagesWritten != 1 || res.PagesConfirmed != 1 {
		t.Fatalf("stop %+v calls %v", res, obs2.calls)
	}
	ev = dev2.events()
	inv := indexOf(ev, "invalidate")
	wait := indexOf(ev, "waiting")
	if inv < 0 || wait < 0 || inv < wait {
		t.Fatalf("invalidate before waiting: %v", ev)
	}
	if count(ev, "page") != 1 {
		t.Fatalf("pages written %v", ev)
	}
}

func TestPrintObserved_drainsStaleBeforePage0(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	page := NewBitmap(8, 1)
	dev := &orderDev{
		pending: []Status{{Type: StatusError, Errors: []string{"cover open"}}},
		fakeDevice: fakeDevice{reads: []readStep{
			{st: doneStatus()}, {st: waitingReply()},
		}},
	}
	obs := &recObs{stopAt: -1, late: dev}
	pr := NewPrinter(p, func() (Device, error) { return dev, nil }, DefaultTimeouts())
	res := pr.PrintObserved(context.Background(), Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{page}}, obs)
	if res.Err != nil || res.State != JobDone {
		t.Fatalf("stale reply was read as the page result: %+v events %v", res, dev.events())
	}
	if dev.drains < 2 {
		t.Fatalf("drains %d", dev.drains)
	}
	if count(dev.events(), "stale") != 0 {
		t.Fatalf("stale status reached the waiter: %v", dev.events())
	}
}

func TestPrintObserved_openErrorsUnchanged(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	job := Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(4, 1)}}
	for _, openErr := range []error{ErrDeviceBusy, ErrNoPrinter} {
		pr := NewPrinter(p, func() (Device, error) { return nil, openErr }, DefaultTimeouts())
		res := pr.PrintObserved(context.Background(), job, &recObs{stopAt: -1})
		if !errors.Is(res.Err, openErr) || res.PagesWritten != 0 {
			t.Fatalf("%v -> %+v", openErr, res)
		}
	}
}

func seqHas(ev []string, a, b string) bool {
	ia, ib := indexOf(ev, a), indexOf(ev, b)
	return ia >= 0 && ib > ia
}

func writesBetween(ev []string, start, end string) bool {
	from := indexOf(ev, start)
	to := indexOf(ev, end)
	if from < 0 || to < 0 {
		return true
	}
	for _, e := range ev[from+1 : to] {
		if e == "page" || e == "invalidate" || e == "write" || e == "status-req" {
			return true
		}
	}
	return false
}

func indexOf(ev []string, want string) int {
	for i, e := range ev {
		if e == want {
			return i
		}
	}
	return -1
}

func count(ev []string, want string) int {
	n := 0
	for _, e := range ev {
		if e == want {
			n++
		}
	}
	return n
}
