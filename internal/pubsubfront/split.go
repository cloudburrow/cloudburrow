package pubsubfront

import (
	"bufio"
	"net"
	"sync"
	"time"
)

// Google's emulator serves gRPC and its REST API (JSON over HTTP/1.1, the
// API gcloud and Terraform's google provider use) on one port, so the front
// must too. A connection is told apart by its first bytes: every gRPC client
// opens with the HTTP/2 client preface, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
// and no HTTP/1.1 method is PRI. A REST client speaking HTTP/2 with prior
// knowledge would reach the gRPC server and be refused; none of the clients
// that use the emulator's REST API does, since plain-text HTTP is HTTP/1.1
// to all of them.
const (
	h2Preface = "PRI"
	// sniffTimeout bounds how long a connection may stay silent before it is
	// routed; a tcpSocket readiness probe connects and says nothing.
	sniffTimeout = 30 * time.Second
)

// Split routes l's connections: those that open with the HTTP/2 preface to
// the first listener, the rest to the second. Closing either closes l.
func Split(l net.Listener) (grpcL, httpL net.Listener) {
	s := &splitter{l: l, done: make(chan struct{})}
	g := &routed{s: s, conns: make(chan net.Conn)}
	h := &routed{s: s, conns: make(chan net.Conn)}
	go s.run(g, h)
	return g, h
}

type splitter struct {
	l    net.Listener
	once sync.Once
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (s *splitter) close(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
		_ = s.l.Close()
	})
}

func (s *splitter) run(g, h *routed) {
	for {
		c, err := s.l.Accept()
		if err != nil {
			s.close(err)
			return
		}
		go s.route(c, g, h)
	}
}

func (s *splitter) route(c net.Conn, g, h *routed) {
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(sniffTimeout))
	b, err := br.Peek(len(h2Preface))
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = c.Close()
		return
	}
	to := h
	if string(b) == h2Preface {
		to = g
	}
	select {
	case to.conns <- &peeked{Conn: c, r: br}:
	case <-s.done:
		_ = c.Close()
	}
}

// routed is one side of a Split.
type routed struct {
	s     *splitter
	conns chan net.Conn
}

func (r *routed) Accept() (net.Conn, error) {
	select {
	case c := <-r.conns:
		return c, nil
	case <-r.s.done:
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		if r.s.err != nil {
			return nil, r.s.err
		}
		return nil, net.ErrClosed
	}
}

func (r *routed) Close() error {
	r.s.close(nil)
	return nil
}

func (r *routed) Addr() net.Addr { return r.s.l.Addr() }

// peeked is a connection whose first bytes were read to route it, and are
// read again from r.
type peeked struct {
	net.Conn
	r *bufio.Reader
}

func (p *peeked) Read(b []byte) (int, error) { return p.r.Read(b) }
