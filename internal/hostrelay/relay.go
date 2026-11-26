// Package hostrelay forwards TCP connections from one address to another.
//
// It is how a pod reaches a service the CLI serves on loopback when the
// container runtime gives pods no path to the host's loopback (#575): on
// Docker Engine on Linux the host is reachable from the kind network only
// at that network's gateway address, so the CLI listens there as well and
// relays each connection to the service's loopback listener. The service
// itself stays bound to loopback, and nothing listens on any other
// interface.
//
// With the instance's compute on a remote host (#2209), the address a
// relay listens on is the host's, not this machine's: SetRemote hands the
// listen to the SSH connection, which binds it on the host as a reverse
// forward to the same target here.
package hostrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Tunnel binds a listen address on the compute host and carries each
// connection to a target on this machine: remotecompute.Session.Reverse.
type Tunnel interface {
	Reverse(ctx context.Context, listen, target string) (bound string, cancel func() error, err error)
}

var (
	remoteMu sync.Mutex
	remote   Tunnel
)

// SetRemote makes every Relay started from now on a reverse forward over
// t; nil returns to listening on this machine.
func SetRemote(t Tunnel) {
	remoteMu.Lock()
	remote = t
	remoteMu.Unlock()
}

// Remote is the tunnel set by SetRemote, or nil.
func Remote() Tunnel {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	return remote
}

// Relay accepts on Listen and copies each connection to and from Target.
type Relay struct {
	Listen string
	Target string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
	// bound and cancel are the reverse forward's, with a Tunnel.
	bound  string
	cancel func() error
}

// Start binds Listen and relays until Close. It does not block.
func (r *Relay) Start(ctx context.Context) error {
	if t := Remote(); t != nil {
		bound, cancel, err := t.Reverse(ctx, r.Listen, r.Target)
		if err != nil {
			return fmt.Errorf("relay %s -> %s on the compute host: %w", r.Listen, r.Target, err)
		}
		r.mu.Lock()
		r.bound, r.cancel = bound, cancel
		r.mu.Unlock()
		return nil
	}
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
	if r.bound != "" {
		return r.bound
	}
	if r.ln == nil {
		return ""
	}
	return r.ln.Addr().String()
}

// Close stops accepting, ends every relayed connection and waits for them.
func (r *Relay) Close() error {
	r.mu.Lock()
	ln, conns, cancel := r.ln, r.conns, r.cancel
	r.ln, r.conns, r.cancel, r.bound = nil, nil, nil, ""
	r.mu.Unlock()
	if cancel != nil {
		return cancel()
	}
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
