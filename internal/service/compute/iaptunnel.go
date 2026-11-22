package compute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IAP TCP forwarding's destinations (#2160, internal/service/iap's
// Tunnel): an instance's interface, or a host in a network, reached as
// the load balancers reach an instance's port (LBPlane.InstanceAddr), with
// the connection coming from IAP's range, 35.235.240.0/20, which the
// network's firewall rules must allow on the port
// (https://docs.cloud.google.com/iap/docs/using-tcp-forwarding, read
// 2026-11-22).

// IAPTunnelSource is an address of IAP TCP forwarding's range, which the
// firewall rules are checked against.
const IAPTunnelSource = "35.235.240.1"

// ErrTunnelNoInstance and ErrTunnelNotRunning are a tunnel's instance that
// does not exist and one that is not RUNNING.
var (
	ErrTunnelNoInstance = errors.New("no such instance")
	ErrTunnelNotRunning = errors.New("the instance is not running")
)

// TunnelVM is a tunnel's destination instance.
type TunnelVM struct {
	ip string
	ep lbEndpoint
}

// IP is the destination's internal address.
func (v TunnelVM) IP() string { return v.ip }

// TunnelInstance resolves an instance's interface (nic0, nic1, ...).
func (s *Server) TunnelInstance(project, zone, instance, iface string) (TunnelVM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := "projects/" + project + "/zones/" + zone + "/instances/" + instance
	o, ok := s.res[rel]
	if !ok {
		return TunnelVM{}, ErrTunnelNoInstance
	}
	idx := 0
	if n, ok := strings.CutPrefix(iface, "nic"); ok {
		i, err := strconv.Atoi(n)
		if err != nil {
			return TunnelVM{}, fmt.Errorf("no interface %s", iface)
		}
		idx = i
	}
	nics, _ := o["networkInterfaces"].([]any)
	if idx >= len(nics) {
		return TunnelVM{}, fmt.Errorf("no interface %s", iface)
	}
	ep, _ := s.instanceEndpointLocked(rel, 0, "")
	v := TunnelVM{ip: str(objOf(nics[idx]), "networkIP"), ep: ep}
	if str(o, "status") != stRunning {
		return v, ErrTunnelNotRunning
	}
	return v, nil
}

// TunnelHost resolves a host in a network of a region: an instance's
// internal address, or its internal DNS name (NAME.ZONE.c.PROJECT.internal
// or NAME).
func (s *Server) TunnelHost(project, region, network, host string) (TunnelVM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "projects/" + project + "/zones/"
	var rels []string
	for rel := range s.res {
		if strings.HasPrefix(rel, prefix) && strings.Contains(rel, "/instances/") && strings.Count(rel, "/") == 5 {
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, rel := range rels {
		o := s.res[rel]
		v, ok := vmNetOf(rel, o)
		if !ok || lastSeg(v.network) != lastSeg(network) || regionOfZone(v.zone) != region {
			continue
		}
		if v.ip != h && v.fqdn() != h && v.name != h {
			continue
		}
		ep, _ := s.instanceEndpointLocked(rel, 0, "")
		t := TunnelVM{ip: v.ip, ep: ep}
		if !v.running {
			return t, ErrTunnelNotRunning
		}
		return t, nil
	}
	return TunnelVM{}, ErrTunnelNoInstance
}

// TunnelDial connects to a destination's port, once the network's
// firewall rules let IAP's range in to it.
func (s *Server) TunnelDial(ctx context.Context, v TunnelVM, port int) (net.Conn, error) {
	if !v.ep.hasVM {
		return nil, ErrTunnelNoInstance
	}
	s.mu.Lock()
	allowed := s.firewallAllowsLocked(v.ep.vmNet, IAPTunnelSource, port)
	s.mu.Unlock()
	if !allowed {
		return nil, fmt.Errorf("the firewall rules of %s do not allow %s on port %d", lastSeg(v.ep.vmNet.network), IAPTunnelSource, port)
	}
	if s.opts.LB == nil {
		return nil, errors.New("instances' ports are not reachable here")
	}
	for {
		addr, err := s.opts.LB.InstanceAddr(ctx, v.ep.vm, port)
		if err == nil {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(500 * time.Millisecond):
		}
	}
}
