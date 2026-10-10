package brotherql

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

func rawStatus(p *ql.Profile, typ ql.StatusType, phase, err1, err2 byte) []byte {
	b := make([]byte, 32)
	b[0], b[1], b[2] = 0x80, 0x20, 0x42
	b[3], b[4] = p.StatusModelByte3, p.StatusModelByte4
	b[8], b[9] = err1, err2
	b[10], b[11] = 62, byte(ql.MediaContinuous)
	b[18], b[19] = byte(typ), phase
	return b
}

type fakeQL struct {
	ln   net.Listener
	got  chan []byte
	done chan struct{}
}

func serve(t *testing.T, n int, reply func(net.Conn)) *fakeQL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeQL{ln: ln, got: make(chan []byte, 1), done: make(chan struct{})}
	t.Cleanup(func() { ln.Close(); <-f.done })
	go func() {
		defer close(f.done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err != nil {
			f.got <- nil
			return
		}
		f.got <- buf
		if reply != nil {
			reply(c)
		}
		_, _ = io.Copy(io.Discard, c)
	}()
	return f
}

func newFastTCP(p *ql.Profile, addr string) *tcp {
	b := NewTCP(p, addr).(*tcp)
	b.firstReply = 100 * time.Millisecond
	b.statusTimeout = 100 * time.Millisecond
	b.dialTimeout = time.Second
	b.timeouts = fastTimeouts()
	return b
}

func encode(t *testing.T, p *ql.Profile, job ql.Job) []byte {
	t.Helper()
	b, err := ql.NewEncoder().Encode(p, job)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTCPPrintSilentPrinter(t *testing.T) {
	p, job := label62(t)
	want := encode(t, p, job)
	f := serve(t, len(want), nil)
	if err := newFastTCP(p, f.ln.Addr().String()).Print(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if got := <-f.got; !bytes.Equal(got, want) {
		t.Fatal("job bytes differ")
	}
}

func TestTCPPrintWaitsForCompletion(t *testing.T) {
	p, job := label62(t)
	want := encode(t, p, job)
	f := serve(t, len(want), func(c net.Conn) {
		pr := rawStatus(p, ql.StatusPhase, ql.PhasePrinting, 0, 0)
		_, _ = c.Write(append([]byte{0x00, 0x01, 0x80}, pr[:10]...))
		time.Sleep(20 * time.Millisecond)
		_, _ = c.Write(pr[10:])
		time.Sleep(150 * time.Millisecond)
		_, _ = c.Write(rawStatus(p, ql.StatusCompleted, ql.PhaseWaiting, 0, 0))
	})
	start := time.Now()
	if err := newFastTCP(p, f.ln.Addr().String()).Print(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("returned before the completed status")
	}
}

func TestTCPPrintErrorStatus(t *testing.T) {
	p, job := label62(t)
	f := serve(t, len(encode(t, p, job)), func(c net.Conn) {
		_, _ = c.Write(rawStatus(p, ql.StatusError, ql.PhaseWaiting, 0, 0x10))
	})
	err := newFastTCP(p, f.ln.Addr().String()).Print(context.Background(), job)
	if !errors.Is(err, ql.ErrCoverOpen) || errors.Is(err, backend.ErrRetryable) {
		t.Fatalf("err %v", err)
	}
	if got := ReasonsForError(err); !reflect.DeepEqual(got, []string{"cover-open"}) {
		t.Fatalf("reasons %v", got)
	}
}

func TestTCPPrintNoCompletion(t *testing.T) {
	p, job := label62(t)
	f := serve(t, len(encode(t, p, job)), func(c net.Conn) {
		_, _ = c.Write(rawStatus(p, ql.StatusPhase, ql.PhasePrinting, 0, 0))
	})
	err := newFastTCP(p, f.ln.Addr().String()).Print(context.Background(), job)
	if !errors.Is(err, ql.ErrTimeout) || errors.Is(err, backend.ErrRetryable) {
		t.Fatalf("err %v", err)
	}
}

func TestTCPPrintPrinterClosesAfterJob(t *testing.T) {
	p, job := label62(t)
	f := serve(t, len(encode(t, p, job)), func(c net.Conn) { c.Close() })
	if err := newFastTCP(p, f.ln.Addr().String()).Print(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestTCPPrintCancelledAfterWrite(t *testing.T) {
	p, job := label62(t)
	f := serve(t, len(encode(t, p, job)), func(c net.Conn) {
		_, _ = c.Write(rawStatus(p, ql.StatusPhase, ql.PhasePrinting, 0, 0))
		time.Sleep(400 * time.Millisecond)
		_, _ = c.Write(rawStatus(p, ql.StatusPhase, ql.PhaseWaiting, 0, 0))
	})
	b := newFastTCP(p, f.ln.Addr().String())
	b.timeouts.CompletedBase = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.Print(ctx, job)
	if !errors.Is(err, ql.ErrPartial) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, backend.ErrRetryable) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d < 400*time.Millisecond || d > 5*time.Second {
		t.Fatalf("returned after %s; want once the printer is idle again", d)
	}
}

// A label the printer completes after a cancel came out whole and counts as printed.
func TestTCPPrintCancelledThenCompleted(t *testing.T) {
	p, job := label62(t)
	f := serve(t, len(encode(t, p, job)), func(c net.Conn) {
		_, _ = c.Write(rawStatus(p, ql.StatusPhase, ql.PhasePrinting, 0, 0))
		time.Sleep(300 * time.Millisecond)
		_, _ = c.Write(rawStatus(p, ql.StatusCompleted, ql.PhaseWaiting, 0, 0))
	})
	b := newFastTCP(p, f.ln.Addr().String())
	b.timeouts.CompletedBase = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := b.Print(ctx, job); err != nil {
		t.Fatalf("err %v", err)
	}
}

// Cancelling while the label is still being written must not cut the raster
// stream short: the printer gets every byte.
func TestTCPPrintCancelMidWriteSendsWholeLabel(t *testing.T) {
	p := profile(t)
	job := JobFor(p, media(t, p, "62"), label(696, 60000), false)
	want := encode(t, p, job)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan int, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer c.Close()
		time.Sleep(200 * time.Millisecond)
		cancel()
		time.Sleep(300 * time.Millisecond)
		n, _ := io.ReadFull(c, make([]byte, len(want)))
		got <- n
	}()
	b := newFastTCP(p, ln.Addr().String())
	if err := b.Print(ctx, job); err != nil {
		t.Fatalf("err %v", err)
	}
	if n := <-got; n != len(want) {
		t.Fatalf("printer received %d of %d bytes", n, len(want))
	}
}

func TestTCPPartialWriteAbandonsJob(t *testing.T) {
	p := profile(t)
	job := JobFor(p, media(t, p, "62"), label(696, 60000), false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	second := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.ReadFull(c, make([]byte, 1000))
		c2, err := ln.Accept()
		c.Close()
		if err != nil {
			return
		}
		defer c2.Close()
		b, _ := io.ReadAll(c2)
		second <- b
	}()
	b := newFastTCP(p, ln.Addr().String())
	b.writeTimeout = 200 * time.Millisecond
	err = b.Print(context.Background(), job)
	if !errors.Is(err, ql.ErrPartial) || errors.Is(err, backend.ErrRetryable) {
		t.Fatalf("err %v", err)
	}
	select {
	case got := <-second:
		if want := append(make([]byte, 200), 0x1b, 0x40); !bytes.Equal(got, want) {
			t.Fatalf("abandon sent %d bytes % x", len(got), got[len(got)-2:])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no invalidate after the partial write")
	}
}

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestTCPRefusedIsRetryable(t *testing.T) {
	p, job := label62(t)
	err := newFastTCP(p, closedAddr(t)).Print(context.Background(), job)
	if !errors.Is(err, backend.ErrRetryable) || !errors.Is(err, ql.ErrNoPrinter) {
		t.Fatalf("err %v", err)
	}
}

func TestTCPEncodeErrorIsFinal(t *testing.T) {
	p := profile(t)
	err := newFastTCP(p, closedAddr(t)).Print(context.Background(), ql.Job{Media: media(t, p, "62")})
	if err == nil || errors.Is(err, backend.ErrRetryable) {
		t.Fatalf("err %v", err)
	}
}

func TestTCPState(t *testing.T) {
	p := profile(t)
	f := serve(t, 3, func(c net.Conn) {
		_, _ = c.Write(rawStatus(p, ql.StatusReply, ql.PhaseWaiting, 0, 0))
	})
	s, err := newFastTCP(p, f.ln.Addr().String()).State(context.Background())
	if err != nil || s.Condition != backend.Idle || len(s.Details) == 0 || !strings.HasPrefix(s.Details[0].Value, "62 mm") {
		t.Fatalf("idle: %+v %v", s, err)
	}
	if got := <-f.got; !bytes.Equal(got, statusRequest) {
		t.Fatalf("request %x", got)
	}

	f = serve(t, 3, func(c net.Conn) {
		_, _ = c.Write(rawStatus(p, ql.StatusError, ql.PhaseWaiting, 0x01, 0))
	})
	s, _ = newFastTCP(p, f.ln.Addr().String()).State(context.Background())
	if s.Condition != backend.Error || !reflect.DeepEqual(s.Reasons, []string{"media-empty", "media-needed"}) {
		t.Fatalf("no media: %+v", s)
	}

	f = serve(t, 3, nil)
	s, _ = newFastTCP(p, f.ln.Addr().String()).State(context.Background())
	if s.Condition != backend.Unknown || s.IPPState() != 3 {
		t.Fatalf("silent: %+v", s)
	}

	s, _ = newFastTCP(p, closedAddr(t)).State(context.Background())
	if s.Condition != backend.Offline || !reflect.DeepEqual(s.Reasons, []string{"offline-report"}) {
		t.Fatalf("refused: %+v", s)
	}
}

func TestTCPStateWhilePrinting(t *testing.T) {
	p, job := label62(t)
	f := serve(t, len(encode(t, p, job)), func(c net.Conn) {
		_, _ = c.Write(rawStatus(p, ql.StatusPhase, ql.PhasePrinting, 0, 0))
		time.Sleep(200 * time.Millisecond)
		_, _ = c.Write(rawStatus(p, ql.StatusCompleted, ql.PhaseWaiting, 0, 0))
	})
	b := newFastTCP(p, f.ln.Addr().String())
	done := make(chan error, 1)
	go func() { done <- b.Print(context.Background(), job) }()
	deadline := time.Now().Add(time.Second)
	for b.mu.TryLock() {
		b.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("print never started")
		}
		time.Sleep(time.Millisecond)
	}
	s, err := b.State(context.Background())
	if err != nil || s.Condition != backend.Busy {
		t.Fatalf("state %+v %v", s, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTCPAddr(t *testing.T) {
	cases := map[string]string{
		"printer":                 "printer:9100",
		"printer:9101":            "printer:9101",
		"tcp://printer":           "printer:9100",
		"tcp://192.0.2.7:9100/":   "192.0.2.7:9100",
		"::1":                     "[::1]:9100",
		"[::1]:9100":              "[::1]:9100",
		"tcp://[2001:db8::1]":     "[2001:db8::1]:9100",
		"tcp://[2001:db8::1]:515": "[2001:db8::1]:515",
	}
	for in, want := range cases {
		if got := tcpAddr(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
