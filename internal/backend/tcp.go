package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

var statusRequest = []byte{0x1b, 0x69, 0x53}

// abandonJob is Brother's way to stop data sent midway: enough NULs to complete
// any partly received command (invalidate), then ESC @ (initialize).
var abandonJob = append(make([]byte, 200), 0x1b, 0x40)

type tcp struct {
	prof *ql.Profile
	addr string

	dialTimeout  time.Duration
	writeTimeout time.Duration
	// firstReply is how long Print waits for any status after the job bytes. A printer that sends
	// nothing in that window is taken not to report status, and the job counts as sent.
	firstReply    time.Duration
	statusTimeout time.Duration
	timeouts      ql.Timeouts

	// Network QLs serve one 9100 connection at a time.
	mu     sync.Mutex
	closed atomic.Bool
}

// NewTCP writes the encoded job to a network QL on raw port 9100. addr is host, host:port or
// tcp://host:port. Status is best-effort: ESC i S over the same port, unknown when unanswered.
func NewTCP(prof *ql.Profile, addr string) Backend {
	return &tcp{
		prof:          prof,
		addr:          tcpAddr(addr),
		dialTimeout:   5 * time.Second,
		writeTimeout:  2 * time.Minute,
		firstReply:    3 * time.Second,
		statusTimeout: 2 * time.Second,
		timeouts:      ql.DefaultTimeouts(),
	}
}

func tcpAddr(addr string) string {
	addr = strings.TrimPrefix(addr, "tcp://")
	addr = strings.TrimSuffix(addr, "/")
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), "9100")
}

func (t *tcp) dial(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", t.addr)
}

func (t *tcp) Print(ctx context.Context, job ql.Job) error {
	if t.closed.Load() {
		return ErrClosed
	}
	bin, err := ql.NewEncoder().Encode(t.prof, job)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := t.dial(ctx, t.dialTimeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return retryable(fmt.Errorf("dial %s: %w (%v)", t.addr, ql.ErrNoPrinter, err))
	}
	defer conn.Close()
	if err := ctx.Err(); err != nil {
		return err
	}

	// ctx is not consulted again until the label is sent: stopping mid-write would
	// leave the printer inside a raster command with no print or cut. USB jobs
	// likewise finish the label being written.
	_ = conn.SetWriteDeadline(time.Now().Add(t.writeTimeout))
	n, err := conn.Write(bin)
	if err != nil {
		if n == 0 {
			return retryable(fmt.Errorf("write %s: %w (%v)", t.addr, ql.ErrNoPrinter, err))
		}
		conn.Close()
		t.abandon()
		return fmt.Errorf("write %s after %d bytes: %w (%v)", t.addr, n, ql.ErrPartial, err)
	}
	return t.waitDone(ctx, conn, job)
}

// abandon tells the printer to drop a partly sent job, best effort, on a new
// connection because network QLs serve one at a time.
func (t *tcp) abandon() {
	conn, err := net.DialTimeout("tcp", t.addr, t.dialTimeout)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(t.dialTimeout))
	_, _ = conn.Write(abandonJob)
}

func (t *tcp) waitDone(ctx context.Context, conn net.Conn, job ql.Job) error {
	height := 0
	for _, p := range job.Pages {
		if p != nil {
			height += p.H
		}
	}
	dpi := 300
	if t.prof != nil && t.prof.DPI > 0 {
		dpi = t.prof.DPI
	}
	tm := t.timeouts
	limit := tm.CompletedBase + time.Duration(ql.LengthMM(dpi, height, job.Options.HighRes))*tm.CompletedPerMM
	limit = max(limit, tm.PhasePrinting)
	deadline := time.Now().Add(limit)

	// A cancel returns only once the printer has gone from printing back to idle,
	// as ql's USB wait does, so the connection is never closed (or reset over
	// unread status bytes) while the printer may still be taking in the label.
	r := statusReader{conn: conn, prof: t.prof}
	pages := len(job.Pages)
	completed := 0
	first := true
	firstUntil := time.Now().Add(t.firstReply)
	printing, idle := false, false
	for {
		final := deadline
		if first {
			final = firstUntil
		}
		until := final
		if ctx.Done() != nil {
			until = earlier(final, time.Now().Add(cancelPoll))
		}
		st, err := r.next(until)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				cerr := ctx.Err()
				if cerr != nil && idle {
					return fmt.Errorf("%w: %w", ql.ErrPartial, cerr)
				}
				if until.Before(final) {
					continue
				}
				if first && !r.sawData {
					return nil
				}
				if cerr != nil {
					return fmt.Errorf("%w: %w", ql.ErrPartial, cerr)
				}
				return fmt.Errorf("print %s: %w waiting for completion", t.addr, ql.ErrTimeout)
			}
			return fmt.Errorf("print %s: %w (%v)", t.addr, ql.ErrPartial, err)
		}
		first = false
		if st.Printing {
			printing = true
		}
		idle = printing && !st.Printing && (st.Type == ql.StatusPhase || st.Type == ql.StatusCompleted)
		if st.Type == ql.StatusError || len(st.Errors) > 0 {
			return fmt.Errorf("print %s: %w", t.addr, statusErr(st))
		}
		if st.Type == ql.StatusNotify && st.Notification == ql.NotifyCoolingStart {
			deadline = later(deadline, time.Now().Add(tm.Cooling))
			continue
		}
		if st.Type == ql.StatusNotify && st.Notification == ql.NotifyCoolingFinish {
			deadline = later(deadline, time.Now().Add(tm.CompletedBase))
			continue
		}
		if st.Type == ql.StatusCompleted {
			completed++
			if completed >= pages {
				return nil
			}
		}
		if cerr := ctx.Err(); cerr != nil && idle {
			return fmt.Errorf("%w: %w", ql.ErrPartial, cerr)
		}
	}
}

const cancelPoll = 200 * time.Millisecond

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (t *tcp) State(ctx context.Context) (State, error) {
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
	conn, err := t.dial(ctx, t.statusTimeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return State{}, cerr
		}
		return stateFromErr(fmt.Errorf("dial %s: %w (%v)", t.addr, ql.ErrNoPrinter, err)), nil
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	deadline := time.Now().Add(t.statusTimeout)
	_ = conn.SetWriteDeadline(deadline)
	if _, err := conn.Write(statusRequest); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return State{}, cerr
		}
		return State{Condition: Unknown, Message: "status request: " + err.Error()}, nil
	}
	r := statusReader{conn: conn, prof: t.prof}
	st, err := r.next(deadline)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return State{}, cerr
		}
		return State{Condition: Unknown, Message: "no status reply"}, nil
	}
	return StateFromStatus(st), nil
}

func (t *tcp) Close() error {
	t.closed.Store(true)
	return nil
}

type statusReader struct {
	conn    net.Conn
	prof    *ql.Profile
	buf     []byte
	sawData bool
}

func (r *statusReader) next(deadline time.Time) (ql.Status, error) {
	tmp := make([]byte, 256)
	for {
		for len(r.buf) >= 3 {
			i := indexHeader(r.buf)
			if i < 0 {
				r.buf = r.buf[len(r.buf)-2:]
				break
			}
			r.buf = r.buf[i:]
			if len(r.buf) < 32 {
				break
			}
			st, err := ql.ParseStatus(r.prof, r.buf[:32])
			if err != nil {
				r.buf = r.buf[1:]
				continue
			}
			r.buf = r.buf[32:]
			return st, nil
		}
		_ = r.conn.SetReadDeadline(deadline)
		n, err := r.conn.Read(tmp)
		if n > 0 {
			r.sawData = true
			r.buf = append(r.buf, tmp[:n]...)
		}
		if err != nil {
			return ql.Status{}, err
		}
	}
}

func indexHeader(b []byte) int {
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0x80 && b[i+1] == 0x20 && b[i+2] == 0x42 {
			return i
		}
	}
	return -1
}
