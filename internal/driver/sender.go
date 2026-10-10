package driver

import (
	"bytes"
	"context"
	"io"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
)

// Sender delivers units to the printer.
type Sender interface {
	// Send delivers one unit. It returns a backend.ErrRetryable-wrapped error only when nothing
	// reached the printer; the bridge then retries with backoff (backend.Retry).
	Send(ctx context.Context, u Unit) error
	State(ctx context.Context) (backend.State, error)
	Close() error
}

// SenderProvider is implemented by a driver that needs its own conversation with the printer,
// for example status replies between labels. NewSender returns (nil, nil) for targets it leaves to
// the generic transport.
type SenderProvider interface {
	NewSender(t backend.Target) (Sender, error)
}

// NewSender returns the driver's own sender for t, or the generic transport (with the driver's
// Inspector, if any, for dry-run text).
func NewSender(d Driver, t backend.Target) (Sender, error) {
	if sp, ok := d.(SenderProvider); ok {
		s, err := sp.NewSender(t)
		if err != nil || s != nil {
			return s, err
		}
	}
	var inspect func([]byte) (string, error)
	if in, ok := d.(Inspector); ok {
		inspect = in.Inspect
	}
	tr, err := backend.Open(t, inspect)
	if err != nil {
		return nil, err
	}
	return TransportSender(tr), nil
}

// TransportSender sends each unit's Data through tr.
func TransportSender(tr backend.Transport) Sender { return transportSender{tr} }

type transportSender struct{ tr backend.Transport }

func (s transportSender) Send(ctx context.Context, u Unit) error { return s.tr.Send(ctx, u.Data) }

// SendStream streams through the transport when it can; otherwise it sends the unit whole.
func (s transportSender) SendStream(ctx context.Context, u Unit, write func(io.Writer) error) error {
	st, ok := s.tr.(backend.StreamTransport)
	if !ok {
		var buf bytes.Buffer
		if err := write(&buf); err != nil {
			return err
		}
		return s.tr.Send(ctx, buf.Bytes())
	}
	w := st.Stream(ctx)
	err := write(w)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s transportSender) State(ctx context.Context) (backend.State, error) {
	return s.tr.State(ctx)
}
func (s transportSender) Close() error { return s.tr.Close() }
