package redisadmin

import (
	"io"
	"net"
	"sync"
)

// endpoint is a host address an instance's clients connect to that stays
// the same while the servers behind it change (#1385): the primary
// endpoint (host and port) carries each connection to the current
// primary's server, and the read endpoint spreads connections over the
// replicas', one each in turn. A failover changes the servers behind it
// and drops the connections it carries, as Memorystore's does: a client
// reconnects to the same address and reaches the new primary.
type endpoint struct {
	ln net.Listener

	mu    sync.Mutex
	ups   []string
	next  int
	conns map[net.Conn]struct{}
	done  bool
}

// listen opens an endpoint on host, on a port the OS assigns.
func listen(host string) (*endpoint, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	e := &endpoint{ln: ln, conns: map[net.Conn]struct{}{}}
	go e.serve()
	return e, nil
}

// Addr is the endpoint's host:port.
func (e *endpoint) Addr() string { return e.ln.Addr().String() }

// set points the endpoint at ups, and with drop closes the connections it
// carries to the servers it pointed at before.
func (e *endpoint) set(ups []string, drop bool) {
	e.mu.Lock()
	e.ups = append([]string(nil), ups...)
	var old []net.Conn
	if drop {
		for c := range e.conns {
			old = append(old, c)
		}
	}
	e.mu.Unlock()
	for _, c := range old {
		_ = c.Close()
	}
}

// pick is the next server a connection goes to, "" for none.
func (e *endpoint) pick() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ups) == 0 {
		return ""
	}
	up := e.ups[e.next%len(e.ups)]
	e.next++
	return up
}

func (e *endpoint) track(c net.Conn, on bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if on {
		if e.done {
			return false
		}
		e.conns[c] = struct{}{}
	} else {
		delete(e.conns, c)
	}
	return true
}

func (e *endpoint) serve() {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		go e.carry(c)
	}
}

func (e *endpoint) carry(c net.Conn) {
	defer c.Close()
	up := e.pick()
	if up == "" {
		return
	}
	u, err := net.Dial("tcp", up)
	if err != nil {
		return
	}
	defer u.Close()
	if !e.track(c, true) {
		return
	}
	defer e.track(c, false)
	if !e.track(u, true) {
		return
	}
	defer e.track(u, false)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
	<-done
}

// close stops the endpoint and every connection it carries.
func (e *endpoint) close() {
	if e == nil {
		return
	}
	_ = e.ln.Close()
	e.mu.Lock()
	e.done = true
	var all []net.Conn
	for c := range e.conns {
		all = append(all, c)
	}
	e.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}
