package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// Port is an open connection to one printer, for drivers that talk to it themselves (status
// queries, replies). Open one per job: usblp nodes are exclusive, and network printers usually
// serve one 9100 connection at a time.
type Port interface {
	// Write writes all of p. It fails when no byte is accepted for the stall timeout given at
	// open; any progress restarts that timeout.
	Write(p []byte) (int, error)
	// Read waits up to timeout for at least one byte; ErrTimeout when none came.
	Read(p []byte, timeout time.Duration) (int, error)
	// Close waits up to the stall timeout for written data to leave the host, then closes.
	Close() error
}

// DefaultStall is how long a generic write may make no progress before it fails. Lasers can
// spend a minute warming up or waiting for a tray before they take more data.
const DefaultStall = 3 * time.Minute

type tcpPort struct {
	conn  net.Conn
	stall time.Duration
}

// DialTCP opens a raw connection to addr (host, host:port or tcp://host:port; port 9100 by default).
// A failed dial wraps ErrNoPrinter.
func DialTCP(ctx context.Context, addr string, timeout, stall time.Duration) (Port, error) {
	addr = TCPAddr(addr)
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("dial %s: %w (%v)", addr, ErrNoPrinter, err)
	}
	if stall <= 0 {
		stall = DefaultStall
	}
	return &tcpPort{conn: conn, stall: stall}, nil
}

const tcpChunk = 64 << 10

func (p *tcpPort) Write(b []byte) (int, error) {
	wrote := 0
	for wrote < len(b) {
		_ = p.conn.SetWriteDeadline(time.Now().Add(p.stall))
		end := min(wrote+tcpChunk, len(b))
		n, err := p.conn.Write(b[wrote:end])
		wrote += n
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return wrote, fmt.Errorf("write: %w", ErrTimeout)
			}
			return wrote, fmt.Errorf("write: %w", err)
		}
	}
	return wrote, nil
}

func (p *tcpPort) Read(b []byte, timeout time.Duration) (int, error) {
	_ = p.conn.SetReadDeadline(time.Now().Add(timeout))
	n, err := p.conn.Read(b)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return n, ErrTimeout
	}
	return n, err
}

func (p *tcpPort) Close() error {
	return p.conn.Close()
}
