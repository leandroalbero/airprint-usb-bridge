package backend

import (
	"context"
	"fmt"
	"io"
	"os"
)

// StreamTransport is a Transport that also takes a unit as a stream, so a long document goes to
// the printer as it is rendered instead of being held in memory.
type StreamTransport interface {
	Transport
	// Stream starts one unit. The printer is opened on the first Write, which holds the
	// transport until Close. A Write error wraps ErrRetryable only while no byte of the unit
	// has reached the printer; afterwards it wraps ErrPartial. Close ends the unit and must be
	// called once; it fails with ErrPartial when the end may not have reached the printer.
	// Cancelling ctx after the first Write does not stop the stream: the writer decides where
	// the job can end.
	Stream(ctx context.Context) io.WriteCloser
}

// MaxInspectBytes is the largest dry-run unit decoded into job-<n>.txt; the decoder needs the
// whole unit in memory.
const MaxInspectBytes = 16 << 20

// writeChunks writes data to port in pieces of chunk bytes and adds what was written to *wrote.
func writeChunks(port Port, data []byte, chunk int, wrote *int64) error {
	for len(data) > 0 {
		n, err := port.Write(data[:min(chunk, len(data))])
		*wrote += int64(n)
		data = data[n:]
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *usbTransport) Stream(ctx context.Context) io.WriteCloser {
	return &usbStream{u: u, ctx: ctx}
}

type usbStream struct {
	u      *usbTransport
	ctx    context.Context
	locked bool
	port   Port
	path   string
	wrote  int64
	err    error
}

func (s *usbStream) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.port == nil {
		if err := s.open(); err != nil {
			s.err = err
			return 0, err
		}
	}
	before := s.wrote
	if err := writeChunks(s.port, p, usbChunk, &s.wrote); err != nil {
		if s.wrote == 0 {
			s.err = Retryable(fmt.Errorf("%s: %w", s.path, err))
		} else {
			s.err = fmt.Errorf("%s: %w after %d bytes: %w", s.path, ErrPartial, s.wrote, err)
		}
		return int(s.wrote - before), s.err
	}
	return len(p), nil
}

func (s *usbStream) open() error {
	if s.u.closed.Load() {
		return ErrClosed
	}
	s.u.mu.Lock()
	s.locked = true
	if err := s.ctx.Err(); err != nil {
		return err
	}
	port, path, err := s.u.openPort()
	if err != nil {
		return err
	}
	s.port, s.path = port, path
	return nil
}

func (s *usbStream) Close() error {
	defer func() {
		if s.locked {
			s.locked = false
			s.u.mu.Unlock()
		}
	}()
	if s.port == nil {
		return nil
	}
	err := s.port.Close()
	s.port = nil
	if err != nil && s.err == nil {
		return fmt.Errorf("%s: %w: %w", s.path, ErrPartial, err)
	}
	return nil
}

func (t *tcpTransport) Stream(ctx context.Context) io.WriteCloser {
	return &tcpStream{t: t, ctx: ctx}
}

type tcpStream struct {
	t      *tcpTransport
	ctx    context.Context
	locked bool
	port   *tcpPort
	wrote  int64
	err    error
}

func (s *tcpStream) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.port == nil {
		if s.t.closed.Load() {
			s.err = ErrClosed
			return 0, s.err
		}
		s.t.mu.Lock()
		s.locked = true
		if err := s.ctx.Err(); err != nil {
			s.err = err
			return 0, err
		}
		port, err := s.t.dial(s.ctx)
		if err != nil {
			s.err = err
			return 0, err
		}
		s.port = port
	}
	before := s.wrote
	if err := writeChunks(s.port, p, tcpChunk, &s.wrote); err != nil {
		if s.wrote == 0 {
			s.err = Retryable(fmt.Errorf("%s: %w (%v)", s.t.addr, ErrNoPrinter, err))
		} else {
			s.err = fmt.Errorf("%s: %w after %d bytes: %w", s.t.addr, ErrPartial, s.wrote, err)
		}
		return int(s.wrote - before), s.err
	}
	return len(p), nil
}

func (s *tcpStream) Close() error {
	defer func() {
		if s.locked {
			s.locked = false
			s.t.mu.Unlock()
		}
	}()
	if s.port == nil {
		return nil
	}
	if s.err == nil {
		s.t.finish(s.port)
	}
	s.port.Close()
	s.port = nil
	return nil
}

func (d *dryRun) Stream(ctx context.Context) io.WriteCloser {
	return &dryStream{d: d, ctx: ctx}
}

type dryStream struct {
	d      *dryRun
	ctx    context.Context
	locked bool
	f      *os.File
	base   string
	err    error
}

func (s *dryStream) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.f == nil {
		if err := s.open(); err != nil {
			s.err = err
			return 0, err
		}
	}
	n, err := s.f.Write(p)
	if err != nil {
		s.err = fmt.Errorf("dry-run: %w after %d bytes: %w", ErrPartial, n, err)
		return n, s.err
	}
	return n, nil
}

func (s *dryStream) open() error {
	if s.d.closed.Load() {
		return ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.d.mu.Lock()
	s.locked = true
	base, n, err := s.d.nextBase()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(base+".bin", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	s.d.last = n
	s.f, s.base = f, base
	return nil
}

func (s *dryStream) Close() error {
	defer func() {
		if s.locked {
			s.locked = false
			s.d.mu.Unlock()
		}
	}()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	if err != nil {
		return fmt.Errorf("dry-run: %w: %w", ErrPartial, err)
	}
	if s.d.inspect == nil {
		return nil
	}
	if err := os.WriteFile(s.base+".txt", []byte(s.inspect()+"\n"), 0o600); err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	return nil
}

func (s *dryStream) inspect() string {
	st, err := os.Stat(s.base + ".bin")
	if err != nil {
		return "decode: " + err.Error()
	}
	if st.Size() > MaxInspectBytes {
		return fmt.Sprintf("not decoded: %d bytes, over the %d MiB a dry run decodes", st.Size(), MaxInspectBytes>>20)
	}
	data, err := os.ReadFile(s.base + ".bin")
	if err != nil {
		return "decode: " + err.Error()
	}
	text, err := s.d.inspect(data)
	if err != nil {
		return "decode: " + err.Error()
	}
	return text
}

var (
	_ StreamTransport = (*usbTransport)(nil)
	_ StreamTransport = (*tcpTransport)(nil)
	_ StreamTransport = (*dryRun)(nil)
)
