package main

import (
	"context"
	"net"

	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
)

// Reach is the address at which a pod reaches the listener at loopback
// (127.0.0.1:PORT) on this machine, "" before Start: the internal load
// balancers' and Private Service Connect endpoints' listeners, which VMs
// are carried to (#2140, internal/service/compute/vpcservices.go). Docker
// Desktop forwards host.docker.internal to the loopback, so the address is
// that one's with the same port; on Docker Engine a relay is started on
// the kind network's gateway at the same port, once per listener, and
// closed with the others at Stop.
func (h *clusterHost) Reach(loopback string) string {
	if h == nil {
		return ""
	}
	_, port, err := net.SplitHostPort(loopback)
	if err != nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.address == "" {
		return ""
	}
	to := net.JoinHostPort(h.address, port)
	if h.mode == "host.docker.internal" {
		return to
	}
	if h.reached == nil {
		h.reached = map[string]bool{}
	}
	if !h.reached[loopback] {
		r := &hostrelay.Relay{Listen: to, Target: loopback}
		if err := r.Start(context.Background()); err != nil {
			return ""
		}
		h.relays = append(h.relays, r)
		h.reached[loopback] = true
	}
	return to
}
