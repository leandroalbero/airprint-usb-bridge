package ql

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPrinter_cancelBetweenPages(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDevice{
		reads: []readStep{{st: phaseStatus(true)}, {st: phaseStatus(false)}},
		onRead: func(n int) {
			if n == 1 {
				cancel()
			}
		},
	}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	job := Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(4, 4), NewBitmap(4, 4)}}
	res := pr.Print(ctx, job)
	if res.State != JobCancelled || !errors.Is(res.Err, ErrPartial) {
		t.Fatalf("%+v", res)
	}
	writes, _ := f.snapshot()
	if len(writes) != 2 {
		t.Fatalf("writes %d", len(writes))
	}
	if len(writes[1]) != 202 || writes[1][200] != 0x1b || writes[1][201] != 0x40 {
		t.Fatalf("invalidate %d", len(writes[1]))
	}
}

func TestPrinter_queuedCancel(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	opens := 0
	pr := NewPrinter(p, func() (Device, error) {
		opens++
		return &fakeDevice{reads: []readStep{{st: doneStatus()}}}, nil
	}, shortTimeouts())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := pr.Print(ctx, onePageJob(m))
	if res.State != JobCancelled || opens != 0 {
		t.Fatalf("%+v opens %d", res, opens)
	}
}

func TestPrinter_fifoNoInterleave(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{}
	for i := 0; i < 8; i++ {
		f.reads = append(f.reads, readStep{st: doneStatus()})
	}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	var wg sync.WaitGroup
	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func() {
			defer wg.Done()
			job := Job{Media: m, Options: DefaultJobOptions(), Pages: []*Bitmap{NewBitmap(4, 2), NewBitmap(4, 2)}}
			res := pr.Print(context.Background(), job)
			if res.State != JobDone {
				t.Errorf("%+v", res)
			}
		}()
	}
	wg.Wait()
	writes, _ := f.snapshot()
	if len(writes) != 8 {
		t.Fatalf("writes %d", len(writes))
	}
	for i := 0; i < len(writes); i += 2 {
		if !bytes.HasPrefix(writes[i], []byte{0, 0, 0, 0}) || writes[i+1][0] != 0x1b {
			t.Fatalf("interleaved at %d", i)
		}
	}
}

func TestPrinter_uncutTail(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{reads: []readStep{{st: doneStatus()}, {st: doneStatus()}}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	opts := DefaultJobOptions()
	opts.CutAtEnd = false
	res := pr.Print(context.Background(), Job{Media: m, Options: opts, Pages: []*Bitmap{NewBitmap(4, 2)}})
	if res.State != JobDone || !pr.UncutTailPending() {
		t.Fatalf("%+v tail %v", res, pr.UncutTailPending())
	}
	res = pr.Print(context.Background(), onePageJob(m))
	if res.State != JobDone || pr.UncutTailPending() {
		t.Fatalf("%+v tail %v", res, pr.UncutTailPending())
	}
}

func TestPrinter_statusAndSendRaw(t *testing.T) {
	p := testProfile(t)
	want := Status{Type: StatusReply, MediaWidthMM: 62, MediaKind: MediaContinuous}
	f := &fakeDevice{reads: []readStep{{st: want}, {st: want}, {st: doneStatus()}}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	st, err := pr.Status(context.Background())
	if err != nil || st.MediaWidthMM != 62 {
		t.Fatal(err, st)
	}
	got, err := pr.SendRaw(context.Background(), []byte{0x1b, 0x40}, 15*time.Millisecond)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %d", err, len(got))
	}
	writes, _ := f.snapshot()
	if !bytes.Equal(writes[0], []byte{0x1b, 0x69, 0x53}) {
		t.Fatalf("status write %x", writes[0])
	}
}

func TestPrinter_writeStallWarning(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	f := &fakeDevice{reads: []readStep{{st: doneStatus()}}, delay: 5 * time.Millisecond}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	pr.stall = time.Millisecond
	res := pr.Print(context.Background(), onePageJob(m))
	if res.State != JobDone {
		t.Fatal(res.Err)
	}
	found := false
	for _, w := range res.Warnings {
		if bytes.Contains([]byte(w), []byte("write stall")) {
			found = true
		}
	}
	if !found {
		t.Fatal(res.Warnings)
	}
}

func TestPrinter_deviceBusy(t *testing.T) {
	p := testProfile(t)
	m := mediaByID(t, p, "62")
	pr := NewPrinter(p, func() (Device, error) {
		return nil, ErrDeviceBusy
	}, shortTimeouts())
	res := pr.Print(context.Background(), onePageJob(m))
	if !errors.Is(res.Err, ErrDeviceBusy) {
		t.Fatal(res.Err)
	}
}
