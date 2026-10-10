package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUSBStream(t *testing.T) {
	port := &fakePort{}
	u, opened := fakeUSB(port, nil)
	s := u.Stream(context.Background())
	if len(*opened) != 0 {
		t.Fatal("opened before the first write")
	}
	data := bytes.Repeat([]byte{0x1b, 'E'}, 100000)
	for _, part := range [][]byte{data[:7], data[7:150000], data[150000:]} {
		if n, err := s.Write(part); err != nil || n != len(part) {
			t.Fatalf("write %d: %d %v", len(part), n, err)
		}
	}
	if st, _ := u.State(context.Background()); st.Condition != Busy {
		t.Fatalf("state while streaming %+v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(port.buf.Bytes(), data) || !port.closed || len(*opened) != 1 {
		t.Fatalf("wrote %d bytes, closed %v, opened %v", port.buf.Len(), port.closed, *opened)
	}
	if st, _ := u.State(context.Background()); st.Condition != Idle {
		t.Fatalf("state after the stream %+v", st)
	}

	// Nothing written: nothing opened.
	u, opened = fakeUSB(&fakePort{}, nil)
	if err := u.Stream(context.Background()).Close(); err != nil || len(*opened) != 0 {
		t.Fatalf("empty stream: %v, opened %v", err, *opened)
	}

	u, _ = fakeUSB(nil, errors.Join(errors.New("open"), ErrDeviceBusy))
	s = u.Stream(context.Background())
	if _, err := s.Write(data); !errors.Is(err, ErrRetryable) {
		t.Fatalf("busy printer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The first write moves nothing: still retryable. A later failure never is.
	u, _ = fakeUSB(nil, nil)
	u.open = func(string, time.Duration) (Port, error) { return deadPort{}, nil }
	if _, err := u.Stream(context.Background()).Write([]byte("x")); !errors.Is(err, ErrRetryable) {
		t.Fatalf("nothing written: %v", err)
	}
	mid := &fakePort{failAt: 70000}
	u, _ = fakeUSB(mid, nil)
	s = u.Stream(context.Background())
	if _, err := s.Write(data[:1000]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(data[1000:]); !errors.Is(err, ErrPartial) || errors.Is(err, ErrRetryable) {
		t.Fatalf("mid-stream failure: %v", err)
	}
	if _, err := s.Write([]byte("more")); !errors.Is(err, ErrPartial) {
		t.Fatalf("write after a failure: %v", err)
	}
	s.Close()
	if !mid.closed {
		t.Fatal("port left open")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u, opened = fakeUSB(&fakePort{}, nil)
	if _, err := u.Stream(ctx).Write(data); !errors.Is(err, context.Canceled) || len(*opened) != 0 {
		t.Fatalf("cancelled before opening: %v, opened %v", err, *opened)
	}
}

type deadPort struct{}

func (deadPort) Write([]byte) (int, error)               { return 0, errors.New("EIO") }
func (deadPort) Read([]byte, time.Duration) (int, error) { return 0, ErrTimeout }
func (deadPort) Close() error                            { return nil }

func TestTCPStream(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		b, _ := io.ReadAll(c)
		c.Close()
		got <- b
	}()
	tr := NewTCP("tcp://" + l.Addr().String()).(StreamTransport)
	s := tr.Stream(context.Background())
	data := bytes.Repeat([]byte("0123456789"), 20000)
	for i := 0; i < len(data); i += 30000 {
		if _, err := s.Write(data[i:min(i+30000, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if b := <-got; !bytes.Equal(b, data) {
		t.Fatalf("received %d bytes, want %d", len(b), len(data))
	}

	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l2.Addr().String()
	l2.Close()
	s = NewTCP(addr).(StreamTransport).Stream(context.Background())
	if _, err := s.Write(data); !errors.Is(err, ErrRetryable) || !errors.Is(err, ErrNoPrinter) {
		t.Fatalf("refused: %v", err)
	}
	s.Close()
}

func TestDryRunStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	d := NewDryRun(dir, func(b []byte) (string, error) { return "len " + string(rune('0'+len(b))), nil }).(StreamTransport)
	if err := d.Stream(context.Background()).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("an empty stream made %s: %v", dir, err)
	}
	s := d.Stream(context.Background())
	for _, p := range []string{"ab", "c"} {
		if _, err := s.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), []byte("xy")); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{"1": "abc", "2": "xy"} {
		bin, err := os.ReadFile(filepath.Join(dir, "job-"+n+".bin"))
		if err != nil || string(bin) != want {
			t.Fatalf("job-%s.bin %q %v", n, bin, err)
		}
		txt, err := os.ReadFile(filepath.Join(dir, "job-"+n+".txt"))
		if err != nil || string(txt) != "len "+string(rune('0'+len(want)))+"\n" {
			t.Fatalf("job-%s.txt %q %v", n, txt, err)
		}
		if st, _ := os.Stat(filepath.Join(dir, "job-"+n+".bin")); st.Mode().Perm() != 0o600 {
			t.Fatalf("job-%s.bin mode %v", n, st.Mode())
		}
	}

	big := NewDryRun(t.TempDir(), func([]byte) (string, error) {
		t.Error("a unit over MaxInspectBytes was decoded")
		return "", nil
	}).(StreamTransport)
	s = big.Stream(context.Background())
	chunk := make([]byte, 1<<20)
	for i := 0; i <= MaxInspectBytes>>20; i++ {
		if _, err := s.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	txt, _ := os.ReadFile(filepath.Join(big.(*dryRun).dir, "job-1.txt"))
	if !strings.HasPrefix(string(txt), "not decoded: ") {
		t.Fatalf("txt %q", txt)
	}
}
