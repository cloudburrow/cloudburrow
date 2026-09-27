// Package hostrelay forwards TCP connections from one address to another.
//
// It is how a pod reaches a service the CLI serves on loopback when the
// container runtime gives pods no path to the host's loopback (#575): on
// Docker Engine on Linux the host is reachable from the kind network only
// at that network's gateway address, so the CLI listens there as well and
// relays each connection to the service's loopback listener. The service
// itself stays bound to loopback, and nothing listens on any other
// interface.
package hostrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Relay accepts on Listen and copies each connection to and from Target.
type Relay struct {
	Listen string
	Target string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// Start binds Listen and relays until Close. It does not block.
func (r *Relay) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", r.Listen)
	if err != nil {
		return fmt.Errorf("relay %s -> %s: %w", r.Listen, r.Target, err)
	}
	r.mu.Lock()
	r.ln, r.conns = ln, map[net.Conn]struct{}{}
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.relay(c)
			}()
		}
	}()
	return nil
}

func (r *Relay) track(c net.Conn, on bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		return false
	}
	if on {
		r.conns[c] = struct{}{}
	} else {
		delete(r.conns, c)
	}
	return true
}

func (r *Relay) relay(in net.Conn) {
	if !r.track(in, true) {
		_ = in.Close()
		return
	}
	defer func() { r.track(in, false); _ = in.Close() }()
	out, err := net.Dial("tcp", r.Target)
	if err != nil {
		return
	}
	if !r.track(out, true) {
		_ = out.Close()
		return
	}
	defer func() { r.track(out, false); _ = out.Close() }()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Half-close so a request/response protocol sees the end of the
		// request, then let the other direction finish.
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(out, in)
	go pipe(in, out)
	<-done
	<-done
}

// Addr is the bound listen address, or "" before Start.
func (r *Relay) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return ""
	}
	return r.ln.Addr().String()
}

// Close stops accepting, ends every relayed connection and waits for them.
func (r *Relay) Close() error {
	r.mu.Lock()
	ln, conns := r.ln, r.conns
	r.ln, r.conns = nil, nil
	r.mu.Unlock()
	if ln == nil {
		return nil
	}
	err := ln.Close()
	for c := range conns {
		_ = c.Close()
	}
	r.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
