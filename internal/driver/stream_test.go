package driver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

type brokenWriter struct {
	buf   bytes.Buffer
	limit int
	err   error
}

func (b *brokenWriter) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		return max(room, 0), b.err
	}
	return b.buf.Write(p)
}

func TestOutputAbort(t *testing.T) {
	failed := errors.New("page 3: bad")
	end := []byte("END")

	// Nothing sent: the error is returned as it is, and nothing is written.
	w := &brokenWriter{limit: 100}
	o := NewOutput(context.Background(), w)
	retry := backend.Retryable(errors.New("busy"))
	if err := o.Abort(retry, end); err != retry || w.buf.Len() != 0 {
		t.Fatalf("nothing sent: %v, wrote %q", err, w.buf.String())
	}

	// Part sent, stream working: the ending goes out even after a cancel.
	ctx, cancel := context.WithCancel(context.Background())
	o = NewOutput(ctx, w)
	if _, err := o.Write([]byte("page1")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if o.Err() == nil {
		t.Fatal("Err does not report the cancel")
	}
	err := o.Abort(o.Err(), end)
	if !errors.Is(err, backend.ErrPartial) || !errors.Is(err, context.Canceled) || w.buf.String() != "page1END" {
		t.Fatalf("after a cancel: %v, wrote %q", err, w.buf.String())
	}

	// Part sent, then the stream broke: no ending, and never a retry even if the transport
	// called its error retryable.
	w = &brokenWriter{limit: 3, err: backend.Retryable(errors.New("EIO"))}
	o = NewOutput(context.Background(), w)
	_, werr := o.Write([]byte("page1"))
	err = o.Abort(werr, end)
	if errors.Is(err, backend.ErrRetryable) || !errors.Is(err, backend.ErrPartial) || w.buf.String() != "pag" {
		t.Fatalf("broken stream: %v, wrote %q", err, w.buf.String())
	}
	if _, err := o.Write([]byte("x")); err == nil {
		t.Fatal("write after a failure")
	}

	// A render failure after a page: ending written, error kept.
	w = &brokenWriter{limit: 100}
	o = NewOutput(context.Background(), w)
	o.Write([]byte("p"))
	if err := o.Abort(failed, end); !errors.Is(err, failed) || !errors.Is(err, backend.ErrPartial) || o.Written() != 1 || w.buf.String() != "pEND" {
		t.Fatalf("render failure: %v, wrote %q", err, w.buf.String())
	}
}

type countStreamer struct {
	checked []int
	pages   int
}

func (c *countStreamer) CheckPage(_ JobInfo, n int, p raster.Page) error {
	c.checked = append(c.checked, n)
	if len(p.Gray) != p.Width*p.Height {
		return DocumentErrorf("page %d: malformed", n)
	}
	return nil
}

func (c *countStreamer) Plan(_ JobInfo, pages int) ([]Unit, error) {
	c.pages = pages
	return []Unit{{Label: "a"}, {Label: "b"}}, nil
}

func (c *countStreamer) Stream(_ context.Context, _ JobInfo, _ Unit, pages PageReader, w io.Writer) error {
	for {
		p, err := pages.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p.Gray[0] = 'X' // a driver may overwrite its page
		w.Write(p.Gray)
	}
}

func TestRenderStream(t *testing.T) {
	gray := []byte("abcd")
	pages := []raster.Page{{Width: 2, Height: 2, XDPI: 1, YDPI: 1, Gray: gray}, {Width: 1, Height: 1, XDPI: 1, YDPI: 1, Gray: []byte("z")}}
	s := &countStreamer{}
	units, err := RenderStream(context.Background(), s, JobInfo{}, pages)
	if err != nil || len(units) != 2 || s.pages != 2 {
		t.Fatalf("%d units, %d pages, %v", len(units), s.pages, err)
	}
	for _, u := range units {
		if string(u.Data) != "XbcdX" {
			t.Fatalf("unit %s data %q", u.Label, u.Data)
		}
	}
	if string(gray) != "abcd" {
		t.Fatalf("the caller's page was overwritten: %q", gray)
	}
	if pages[0].Gray != nil {
		t.Fatal("converted page kept")
	}
	if _, err := RenderStream(context.Background(), &countStreamer{}, JobInfo{}, []raster.Page{{Width: 2, Height: 2, XDPI: 1, YDPI: 1}}); !errors.Is(err, ErrDocument) {
		t.Fatalf("page without pixels: %v", err)
	}
}

// sendOnly is a transport without streams: SendStream must send the unit whole.
type sendOnly struct {
	backend.Transport
	got []byte
}

func (s *sendOnly) Send(_ context.Context, data []byte) error {
	s.got = append([]byte(nil), data...)
	return nil
}

func TestSendStreamWithoutStreams(t *testing.T) {
	tr := &sendOnly{}
	ss := TransportSender(tr).(StreamSender)
	err := ss.SendStream(context.Background(), Unit{}, func(w io.Writer) error {
		w.Write([]byte("ab"))
		w.Write([]byte("c"))
		return nil
	})
	if err != nil || string(tr.got) != "abc" {
		t.Fatalf("%q %v", tr.got, err)
	}
	failed := errors.New("render")
	tr.got = nil
	if err := ss.SendStream(context.Background(), Unit{}, func(io.Writer) error { return failed }); err != failed || tr.got != nil {
		t.Fatalf("failed render sent %q: %v", tr.got, err)
	}
}
