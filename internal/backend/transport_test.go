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
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ieee1284"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s + "\n")} }

// sysfs builds a fake /sys with a QL on lp0, an HP that also has an IPP-over-USB interface on
// lp1, an IPP-over-USB-only printer, a mouse, and a node found only under class/usbmisc.
func sysfs() fstest.MapFS {
	return fstest.MapFS{
		"bus/usb/devices/1-1/idVendor":                       file("04f9"),
		"bus/usb/devices/1-1/idProduct":                      file("2042"),
		"bus/usb/devices/1-1/manufacturer":                   file("Brother"),
		"bus/usb/devices/1-1/product":                        file("QL-700"),
		"bus/usb/devices/1-1:1.0/bInterfaceClass":            file("07"),
		"bus/usb/devices/1-1:1.0/bInterfaceProtocol":         file("02"),
		"bus/usb/devices/1-1:1.0/ieee1284_id":                file("MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;"),
		"bus/usb/devices/1-1:1.0/usbmisc/lp0/dev":            file("180:0"),
		"bus/usb/devices/1-2/idVendor":                       file("03f0"),
		"bus/usb/devices/1-2/idProduct":                      file("1234"),
		"bus/usb/devices/1-2:1.0/bInterfaceClass":            file("07"),
		"bus/usb/devices/1-2:1.0/bInterfaceProtocol":         file("02"),
		"bus/usb/devices/1-2:1.0/ieee1284_id":                file("MFG:HP;MDL:LaserJet Pro M404;CMD:PJL,PCL,PCLXL,POSTSCRIPT,URF;"),
		"bus/usb/devices/1-2:1.0/usbmisc/lp1/dev":            file("180:1"),
		"bus/usb/devices/1-2:1.1/bInterfaceClass":            file("07"),
		"bus/usb/devices/1-2:1.1/bInterfaceProtocol":         file("04"),
		"bus/usb/devices/1-3/manufacturer":                   file("Canon"),
		"bus/usb/devices/1-3/product":                        file("PIXMA"),
		"bus/usb/devices/1-3:1.0/bInterfaceClass":            file("07"),
		"bus/usb/devices/1-3:1.0/bInterfaceProtocol":         file("04"),
		"bus/usb/devices/1-4:1.0/bInterfaceClass":            file("03"),
		"bus/usb/devices/usb1/idVendor":                      file("1d6b"),
		"class/usbmisc/lp0/device/ieee1284_id":               file("MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;"),
		"class/usbmisc/lp7/device/ieee1284_id":               file("MFG:Zebra Technologies;CMD:ZPL;MDL:ZD420;"),
		"class/usbmisc/lp1/device/bInterfaceClass":           file("07"),
		"class/usb/lp9/device/ieee1284_id":                   file("MFG:DYMO;MDL:LabelWriter 450;"),
		"class/usbmisc/notlp/device/ieee1284_id":             file("MFG:X;"),
		"bus/usb/devices/1-1:1.0/usbmisc/hiddev0/dev":        file("1:1"),
		"bus/usb/devices/1-2:1.0/usbmisc/lp1/device/unknown": file(""),
	}
}

func TestDiscover(t *testing.T) {
	list, err := Discover(sysfs())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range list {
		got = append(got, p.Node+"|"+p.Path+"|"+p.Interface+"|"+p.Name()+"|"+boolStr(p.IPPUSB))
	}
	want := []string{
		"lp0|/dev/usb/lp0|1-1:1.0|Brother QL-700|-",
		"lp1|/dev/usb/lp1|1-2:1.0|HP LaserJet Pro M404|ipp-usb",
		"lp7|/dev/usb/lp7||Zebra Technologies ZD420|-",
		"lp9|/dev/usb/lp9||DYMO LabelWriter 450|-",
		"||1-3|Canon PIXMA|ipp-usb",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if list[0].VendorID != 0x04f9 || list[0].ProductID != 0x2042 {
		t.Fatalf("ids %04x:%04x", list[0].VendorID, list[0].ProductID)
	}
	if list[0].DeviceID.Model() != "QL-700" {
		t.Fatalf("id %v", list[0].DeviceID)
	}

	p, err := Pick(list, "zebra")
	if err != nil || p.Node != "lp7" {
		t.Fatalf("pick zebra: %+v %v", p, err)
	}
	if p, err := Pick(list, ""); err != nil || p.Node != "lp0" {
		t.Fatalf("pick first: %+v %v", p, err)
	}
	if _, err := Pick(list, "laserjet"); !errors.Is(err, ErrNoPrinter) {
		t.Fatalf("an IPP-over-USB printer was picked: %v", err)
	}
	if _, err := Pick(list, "canon"); !errors.Is(err, ErrNoPrinter) {
		t.Fatalf("an interface without a node was picked: %v", err)
	}
	dymo := func(p USBPrinter) bool { return p.DeviceID.Manufacturer() == "DYMO" }
	if p, err := PickFunc(list, "", dymo); err != nil || p.Node != "lp9" {
		t.Fatalf("pick accepted: %+v %v", p, err)
	}
	if _, err := PickFunc(list, "zebra", dymo); !errors.Is(err, ErrNoPrinter) {
		t.Fatalf("accept ignored: %v", err)
	}
	if id, ok := NodeDeviceID(sysfs(), "/dev/usb/lp9"); !ok || id.Manufacturer() != "DYMO" {
		t.Fatalf("node id %v %v", id, ok)
	}
	if _, ok := NodeDeviceID(sysfs(), "lp3"); ok {
		t.Fatal("lp3 has no id")
	}
	if list, err := Discover(fstest.MapFS{}); err != nil || len(list) != 0 {
		t.Fatalf("empty sysfs: %v %v", list, err)
	}
}

func boolStr(b bool) string {
	if b {
		return "ipp-usb"
	}
	return "-"
}

func TestParseIoctlID(t *testing.T) {
	id := "MFG:Brother;MDL:QL-700;"
	buf := make([]byte, 64)
	buf[1] = byte(len(id) + 2)
	copy(buf[2:], id)
	if got := parseIoctlID(buf); got != id {
		t.Fatalf("got %q", got)
	}
	buf[0], buf[1] = 0xff, 0xff
	if got := parseIoctlID(buf); got != id {
		t.Fatalf("bad length: %q", got)
	}
	if parseIoctlID([]byte{0}) != "" {
		t.Fatal("short buffer")
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		device, dry, match string
		want               Target
	}{
		{"auto", "", "", Target{Kind: TargetUSB, Device: "auto"}},
		{"", "", "brother", Target{Kind: TargetUSB, Device: "auto", Match: "brother"}},
		{"/dev/usb/lp1", "", "", Target{Kind: TargetUSB, Device: "/dev/usb/lp1"}},
		{"tcp://printer.example", "", "", Target{Kind: TargetTCP, Addr: "printer.example:9100"}},
		{"tcp://[::1]:9101/", "", "", Target{Kind: TargetTCP, Addr: "[::1]:9101"}},
		{"auto", "/tmp/d", "", Target{Kind: TargetDryRun, Dir: "/tmp/d"}},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.device, c.dry, c.match)
		if err != nil || got != c.want {
			t.Errorf("%q %q: %+v %v", c.device, c.dry, got, err)
		}
	}
	if _, err := ParseTarget("lp0", "", ""); err == nil {
		t.Error("relative device accepted")
	}
	if TCPAddr("host") != "host:9100" || TCPAddr("tcp://h:1/") != "h:1" {
		t.Error("TCPAddr")
	}
}

func TestDryRunWritesUnits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	b := NewDryRun(dir, func(d []byte) (string, error) { return "len " + string(rune('0'+len(d))), nil })
	for i := 0; i < 2; i++ {
		if err := b.Send(context.Background(), []byte("abc")); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"1", "2"} {
		bin, err := os.ReadFile(filepath.Join(dir, "job-"+n+".bin"))
		if err != nil || string(bin) != "abc" {
			t.Fatalf("job-%s.bin %q %v", n, bin, err)
		}
		txt, err := os.ReadFile(filepath.Join(dir, "job-"+n+".txt"))
		if err != nil || string(txt) != "len 3\n" {
			t.Fatalf("job-%s.txt %q %v", n, txt, err)
		}
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v %v", st.Mode(), err)
	}
	s, err := b.State(context.Background())
	if err != nil || s.Condition != Idle {
		t.Fatalf("%+v %v", s, err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("err %v", err)
	}

	plain := t.TempDir()
	if err := NewDryRun(plain, func([]byte) (string, error) { return "", errors.New("bad") }).Send(context.Background(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if txt, _ := os.ReadFile(filepath.Join(plain, "job-1.txt")); string(txt) != "decode: bad\n" {
		t.Fatalf("txt %q", txt)
	}
	none := t.TempDir()
	if err := NewDryRun(none, nil).Send(context.Background(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(none); len(ents) != 1 {
		t.Fatalf("files %v", ents)
	}
}

func TestDryRunContinuesNumbering(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"job-7.bin", "job-3.bin", "job-x.bin", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := NewDryRun(dir, nil).Send(context.Background(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-8.bin")); err != nil {
		t.Fatal(err)
	}
}

func TestTCPSend(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan []byte, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(c)
			c.Close()
			got <- b
		}
	}()
	tr := NewTCP("tcp://" + l.Addr().String())
	data := bytes.Repeat([]byte("0123456789"), 20000)
	if err := tr.Send(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if b := <-got; !bytes.Equal(b, data) {
		t.Fatalf("received %d bytes, want %d", len(b), len(data))
	}
	if s, err := tr.State(context.Background()); err != nil || s.Condition != Idle {
		t.Fatalf("state %+v %v", s, err)
	}
	<-got

	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l2.Addr().String()
	l2.Close()
	dead := NewTCP(addr)
	if err := dead.Send(context.Background(), data); !errors.Is(err, ErrRetryable) || !errors.Is(err, ErrNoPrinter) {
		t.Fatalf("refused: %v", err)
	}
	if s, _ := dead.State(context.Background()); s.Condition != Offline {
		t.Fatalf("state %+v", s)
	}
}

type fakePort struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	failAt int // fail once this many bytes are written; 0 = never
	closed bool
	cerr   error
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failAt > 0 && p.buf.Len()+len(b) > p.failAt {
		n := p.failAt - p.buf.Len()
		p.buf.Write(b[:n])
		return n, errors.New("EIO")
	}
	return p.buf.Write(b)
}
func (p *fakePort) Read([]byte, time.Duration) (int, error) { return 0, ErrTimeout }
func (p *fakePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return p.cerr
}

func fakeUSB(port *fakePort, openErr error) (*usbTransport, *[]string) {
	var opened []string
	u := &usbTransport{
		device: "auto",
		stall:  time.Second,
		open: func(path string, _ time.Duration) (Port, error) {
			opened = append(opened, path)
			if openErr != nil {
				return nil, openErr
			}
			return port, nil
		},
		discover: func() ([]USBPrinter, error) {
			return []USBPrinter{{Node: "lp3", Path: "/dev/usb/lp3"}}, nil
		},
		status: func(string) (State, error) { return stateFromLPStatus(0x18), nil },
	}
	return u, &opened
}

// TestUSBTransportAccept checks that each unit goes to the printer Target.Accept names, not to the
// first one discovered.
func TestUSBTransportAccept(t *testing.T) {
	port := &fakePort{}
	u, opened := fakeUSB(port, nil)
	u.discover = func() ([]USBPrinter, error) {
		return []USBPrinter{
			{Node: "lp0", Path: "/dev/usb/lp0", DeviceID: ieee1284.Parse("MFG:Brother;MDL:QL-700;")},
			{Node: "lp1", Path: "/dev/usb/lp1", DeviceID: ieee1284.Parse("MFG:HP;MDL:LaserJet 1320;")},
		}, nil
	}
	u.accept = func(p USBPrinter) bool { return p.DeviceID.Model() == "LaserJet 1320" }
	if err := u.Send(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 1 || (*opened)[0] != "/dev/usb/lp1" {
		t.Fatalf("opened %v", *opened)
	}
	pinned, err := Open(Target{Kind: TargetUSB, Device: "auto", Pin: PrinterKey{Manufacturer: "HP", Model: "LaserJet 1320"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := pinned.(*usbTransport).accept; a == nil || a(USBPrinter{DeviceID: ieee1284.Parse("MFG:Brother;MDL:QL-700;")}) {
		t.Fatal("Open ignored the pin")
	}
	if unpinned, _ := Open(Target{Kind: TargetUSB, Device: "auto"}, nil); unpinned.(*usbTransport).accept != nil {
		t.Fatal("accept set without a pin")
	}
	u.accept = func(USBPrinter) bool { return false }
	if err := u.Send(context.Background(), []byte("x")); !errors.Is(err, ErrRetryable) || !errors.Is(err, ErrNoPrinter) {
		t.Fatalf("absent printer: %v", err)
	}
}

func TestUSBTransport(t *testing.T) {
	port := &fakePort{}
	u, opened := fakeUSB(port, nil)
	data := bytes.Repeat([]byte{0x1b, 'E'}, 100000)
	if err := u.Send(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(port.buf.Bytes(), data) || !port.closed || len(*opened) != 1 || (*opened)[0] != "/dev/usb/lp3" {
		t.Fatalf("wrote %d bytes, closed %v, opened %v", port.buf.Len(), port.closed, *opened)
	}
	if s, err := u.State(context.Background()); err != nil || s.Condition != Idle {
		t.Fatalf("state %+v %v", s, err)
	}

	for _, e := range []error{ErrDeviceBusy, ErrNoPrinter} {
		u, _ := fakeUSB(nil, errors.Join(errors.New("open"), e))
		if err := u.Send(context.Background(), data); !errors.Is(err, ErrRetryable) {
			t.Errorf("%v: not retryable: %v", e, err)
		}
	}
	u, _ = fakeUSB(nil, errors.New("permission denied"))
	if err := u.Send(context.Background(), data); err == nil || errors.Is(err, ErrRetryable) {
		t.Errorf("permission: %v", err)
	}

	mid := &fakePort{failAt: 70000}
	u, _ = fakeUSB(mid, nil)
	if err := u.Send(context.Background(), data); !errors.Is(err, ErrPartial) || errors.Is(err, ErrRetryable) {
		t.Errorf("mid-write failure: %v", err)
	}
	u, _ = fakeUSB(&fakePort{cerr: errors.New("flush: timeout")}, nil)
	if err := u.Send(context.Background(), data); !errors.Is(err, ErrPartial) {
		t.Errorf("flush failure: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u, opened = fakeUSB(&fakePort{}, nil)
	if err := u.Send(ctx, data); !errors.Is(err, context.Canceled) || len(*opened) != 0 {
		t.Errorf("cancelled: %v, opened %v", err, *opened)
	}
	u.Close()
	if err := u.Send(context.Background(), data); !errors.Is(err, ErrClosed) {
		t.Errorf("closed: %v", err)
	}
}

func TestStateFromLPStatus(t *testing.T) {
	cases := map[byte]Condition{0x18: Idle, 0x38: Error, 0x08: Offline, 0x10: Error}
	for b, want := range cases {
		if got := stateFromLPStatus(b); got.Condition != want {
			t.Errorf("0x%02x: %v, want %v", b, got.Condition, want)
		}
	}
	if s := stateFromLPStatus(0x38); s.IPPReasons()[0] != "media-empty-error" {
		t.Errorf("paper out reasons %v", s.IPPReasons())
	}
}
