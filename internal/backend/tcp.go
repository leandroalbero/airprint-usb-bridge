package backend

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type tcpTransport struct {
	addr          string
	dialTimeout   time.Duration
	statusTimeout time.Duration
	stall         time.Duration
	// linger is how long Send waits for the printer to close the connection after the job.
	linger time.Duration

	mu     sync.Mutex
	closed atomic.Bool
}

// NewTCP sends each unit over its own raw connection to addr (host, host:port or tcp://host:port;
// port 9100 by default). State only checks that the port accepts a connection.
func NewTCP(addr string) Transport {
	return &tcpTransport{
		addr:          TCPAddr(addr),
		dialTimeout:   5 * time.Second,
		statusTimeout: 2 * time.Second,
		stall:         DefaultStall,
		linger:        2 * time.Second,
	}
}

func (t *tcpTransport) Send(ctx context.Context, data []byte) error {
	if t.closed.Load() {
		return ErrClosed
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	port, err := DialTCP(ctx, t.addr, t.dialTimeout, t.stall)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return Retryable(err)
	}
	p := port.(*tcpPort)
	defer p.Close()
	wrote := 0
	for wrote < len(data) {
		if err := ctx.Err(); err != nil {
			if wrote == 0 {
				return err
			}
			return fmt.Errorf("%w after %d bytes: %w", ErrPartial, wrote, err)
		}
		end := min(wrote+tcpChunk, len(data))
		n, err := p.Write(data[wrote:end])
		wrote += n
		if err != nil {
			if wrote == 0 {
				return Retryable(fmt.Errorf("%s: %w (%v)", t.addr, ErrNoPrinter, err))
			}
			return fmt.Errorf("%s: %w after %d bytes: %w", t.addr, ErrPartial, wrote, err)
		}
	}
	// A half-close tells the printer the job is complete; waiting for its close keeps the
	// final bytes from being reset away by an early close with unread replies pending.
	if tc, ok := p.conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	_ = p.conn.SetReadDeadline(time.Now().Add(t.linger))
	_, _ = io.Copy(io.Discard, p.conn)
	return nil
}

func (t *tcpTransport) State(ctx context.Context) (State, error) {
	if t.closed.Load() {
		return State{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if !t.mu.TryLock() {
		return State{Condition: Busy, Message: "printing"}, nil
	}
	defer t.mu.Unlock()
	port, err := DialTCP(ctx, t.addr, t.statusTimeout, t.statusTimeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return State{}, cerr
		}
		return StateFromError(err), nil
	}
	port.Close()
	return State{Condition: Idle}, nil
}

func (t *tcpTransport) Close() error {
	t.closed.Store(true)
	return nil
}
