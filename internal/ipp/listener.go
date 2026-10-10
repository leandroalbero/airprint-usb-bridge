package ipp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"
)

const peekTimeout = 10 * time.Second

// muxListener serves plain HTTP and TLS on one port, choosing by the first byte
// (0x16 is a TLS handshake record). Peeking happens per connection so a client
// that connects and sends nothing cannot stall Accept.
type muxListener struct {
	inner  net.Listener
	tlsCfg *tls.Config
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	err    error
}

func newMuxListener(inner net.Listener, tlsCfg *tls.Config) *muxListener {
	l := &muxListener{inner: inner, tlsCfg: tlsCfg, conns: make(chan net.Conn), done: make(chan struct{})}
	go l.acceptLoop()
	return l
}

func (l *muxListener) acceptLoop() {
	var delay time.Duration
	for {
		c, err := l.inner.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else if delay *= 2; delay > time.Second {
					delay = time.Second
				}
				select {
				case <-time.After(delay):
					continue
				case <-l.done:
					return
				}
			}
			l.errMu.Lock()
			l.err = err
			l.errMu.Unlock()
			l.Close()
			return
		}
		delay = 0
		go l.classify(c)
	}
}

func (l *muxListener) classify(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(peekTimeout))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var out net.Conn = &peekedConn{Conn: c, r: br}
	if first[0] == 0x16 {
		out = tls.Server(out, l.tlsCfg)
	}
	select {
	case l.conns <- out:
	case <-l.done:
		c.Close()
	}
}

func (l *muxListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		l.errMu.Lock()
		defer l.errMu.Unlock()
		if l.err != nil {
			return nil, l.err
		}
		return nil, net.ErrClosed
	}
}

func (l *muxListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.inner.Close()
	})
	return err
}

func (l *muxListener) Addr() net.Addr { return l.inner.Addr() }

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
