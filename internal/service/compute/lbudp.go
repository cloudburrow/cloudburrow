package compute

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UDP passthrough Network Load Balancers (#2160), as
// https://docs.cloud.google.com/load-balancing/docs/network/networklb-backend-service,
// .../network/target-pools, .../protocol-forwarding and
// .../internal (UDP, read 2026-11-22) describe them: a forwarding rule of
// IPProtocol UDP whose backendService, target pool or target instance
// receives the datagrams, on the rule's own port (the destination port is
// kept, as a passthrough load balancer keeps it), each client flow
// (source address and port, or the session affinity's key) to one
// backend, and the backend's replies back to the client from the rule's
// address. A flow ends after the backend service's connection tracking
// idle timeout (connectionTrackingPolicy.idleTimeoutSec, 600 seconds by
// default) without a datagram.
//
// A network edge security policy (lbarmor.go) filters these load
// balancers' flows, and the TCP ones', by its network match.

// LBDatagrams is what an LBBackends offers, when it can, to reach a
// running instance's UDP port: one datagram channel per flow.
type LBDatagrams interface {
	InstanceUDP(ctx context.Context, vm VMSpec, port int) (DatagramConn, error)
}

// DatagramConn carries one flow's datagrams to a backend and back.
type DatagramConn interface {
	Send(b []byte) error
	// Recv blocks for the next reply.
	Recv() ([]byte, error)
	Close() error
}

// udpConn is a DatagramConn over a connected UDP socket.
type udpConn struct{ c net.Conn }

func (u udpConn) Send(b []byte) error { _, err := u.c.Write(b); return err }
func (u udpConn) Recv() ([]byte, error) {
	buf := make([]byte, 65535)
	n, err := u.c.Read(buf)
	return buf[:n], err
}
func (u udpConn) Close() error { return u.c.Close() }

// udpFlow is one client's flow.
type udpFlow struct {
	up   DatagramConn
	last time.Time
	ep   lbEndpoint
}

// serveUDP serves a UDP rule's listener until it closes.
func (s *Server) serveUDP(rule string, port int, pc net.PacketConn) {
	var mu sync.Mutex
	flows := map[string]*udpFlow{}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				mu.Lock()
				for k, f := range flows {
					_ = f.up.Close()
					delete(flows, k)
				}
				mu.Unlock()
				return
			case <-t.C:
				idle := s.udpIdle(rule)
				mu.Lock()
				for k, f := range flows {
					if time.Since(f.last) > idle {
						_ = f.up.Close()
						delete(flows, k)
					}
				}
				mu.Unlock()
			}
		}
	}()
	buf := make([]byte, 65535)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		data := append([]byte(nil), buf[:n]...)
		key := src.String()
		mu.Lock()
		f := flows[key]
		mu.Unlock()
		if f == nil {
			if !s.l4Allowed(rule, "UDP", src, port) {
				continue
			}
			up, ep, err := s.udpDial(rule, port, src)
			if err != nil {
				continue
			}
			f = &udpFlow{up: up, ep: ep}
			mu.Lock()
			flows[key] = f
			mu.Unlock()
			go func(f *udpFlow, src net.Addr, key string) {
				for {
					b, err := f.up.Recv()
					if err != nil {
						mu.Lock()
						if flows[key] == f {
							delete(flows, key)
						}
						mu.Unlock()
						if f.ep.id != "" {
							s.lb.bal.done(f.ep)
						}
						return
					}
					mu.Lock()
					f.last = time.Now()
					mu.Unlock()
					_, _ = pc.WriteTo(b, src)
				}
			}(f, src, key)
		}
		mu.Lock()
		f.last = time.Now()
		mu.Unlock()
		_ = f.up.Send(data)
	}
}

// udpIdle is a rule's flow idle timeout.
func (s *Server) udpIdle(rule string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	bs := s.res[str(s.res[rule], "backendService")]
	if v, ok := num(objOf(bs["connectionTrackingPolicy"]), "idleTimeoutSec"); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 600 * time.Second
}

// udpDial opens a flow to a backend of the rule.
func (s *Server) udpDial(rule string, port int, src net.Addr) (DatagramConn, lbEndpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.mu.Lock()
	ro := deepCopy(s.res[rule]).(obj)
	target := str(ro, "target")
	to := deepCopy(s.res[target]).(obj)
	var set lbBackendSet
	switch {
	case str(ro, "backendService") != "":
		set, _ = s.backendSetLocked(str(ro, "backendService"))
	case strings.Contains(target, "/targetInstances/"):
		if e, ok := s.instanceEndpointLocked(str(to, "instance"), port, ""); ok {
			set = lbBackendSet{rel: target, bs: obj{}, groups: []lbGroup{{backend: obj{"group": target}, endpoints: []lbEndpoint{e}, healthy: []lbEndpoint{e}}}}
		}
	case strings.Contains(target, "/targetPools/"):
		set = s.poolSetLocked(target, port)
	}
	s.mu.Unlock()
	ip, sport, _ := net.SplitHostPort(src.String())
	key := hashKey(ip, sport)
	switch str(set.bs, "sessionAffinity") {
	case "CLIENT_IP", "CLIENT_IP_PROTO", "CLIENT_IP_NO_DESTINATION":
		key = hashKey(ip)
	}
	tried := map[string]bool{}
	for range 8 {
		ep, ok := s.lb.bal.pick(set, key, tried, true)
		if !ok {
			return nil, lbEndpoint{}, errNoHealthy
		}
		tried[ep.id] = true
		c, err := s.udpTo(ctx, ep, port)
		if err == nil {
			return c, ep, nil
		}
		s.lb.bal.done(ep)
	}
	return nil, lbEndpoint{}, errNoHealthy
}

// udpTo opens a datagram channel to an endpoint's port.
func (s *Server) udpTo(ctx context.Context, ep lbEndpoint, port int) (DatagramConn, error) {
	if ep.kind == "vm" {
		if !ep.running {
			return nil, errors.New("the instance is not running")
		}
		d, ok := s.opts.LB.(LBDatagrams)
		if !ok {
			return nil, errors.New("no UDP path to instances on this machine")
		}
		return d.InstanceUDP(ctx, ep.vm, port)
	}
	if ep.ip == "" {
		return nil, errors.New("the endpoint has no address")
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(ep.ip, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return udpConn{c}, nil
}

// poolSetLocked is a target pool's instances as a backend set, with its
// backup pool's when fewer than failoverRatio are healthy.
func (s *Server) poolSetLocked(poolRel string, port int) lbBackendSet {
	for depth := 0; depth < 2; depth++ {
		pool := s.res[poolRel]
		var all, healthy []lbEndpoint
		for _, inst := range strs(pool, "instances") {
			ep, ok := s.instanceEndpointLocked(inst, port, poolRel)
			if !ok {
				continue
			}
			all = append(all, ep)
			if s.poolInstanceHealthyLocked(poolRel, pool, inst) {
				healthy = append(healthy, ep)
			}
		}
		if b := str(pool, "backupPool"); b != "" && depth == 0 && len(all) > 0 &&
			float64(len(healthy))/float64(len(all)) < numOr(pool, "failoverRatio", 0) {
			poolRel = b
			continue
		}
		if len(healthy) == 0 {
			healthy = all
		}
		return lbBackendSet{rel: poolRel, bs: obj{"sessionAffinity": str(pool, "sessionAffinity")},
			groups: []lbGroup{{backend: obj{"group": poolRel}, endpoints: all, healthy: healthy}}}
	}
	return lbBackendSet{}
}

// l4Allowed applies the network edge security policy of an external
// passthrough rule to a new flow: the target's securityPolicy (a target
// pool's or instance's), else the region's network edge security
// service's. The first rule whose network match holds decides; deny
// drops the flow.
func (s *Server) l4Allowed(rule, proto string, src net.Addr, port int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ro := s.res[rule]
	if !strings.HasPrefix(str(ro, "loadBalancingScheme"), "EXTERNAL") && str(ro, "loadBalancingScheme") != "" {
		return true
	}
	if str(ro, "backendService") == "" && !strings.Contains(str(ro, "target"), "/targetPools/") &&
		!strings.Contains(str(ro, "target"), "/targetInstances/") {
		return true
	}
	policy := str(s.res[str(ro, "target")], "securityPolicy")
	if policy == "" {
		parts := strings.Split(rule, "/")
		if len(parts) > 3 && parts[2] == "regions" {
			prefix := "projects/" + parts[1] + "/regions/" + parts[3] + "/networkEdgeSecurityServices/"
			for k, o := range s.res {
				if strings.HasPrefix(k, prefix) {
					policy = str(o, "securityPolicy")
				}
			}
		}
	}
	p := s.res[policy]
	if p == nil {
		return true
	}
	ip, sp, _ := net.SplitHostPort(src.String())
	sport, _ := strconv.Atoi(sp)
	for _, e := range listOf(p["rules"]) {
		r := objOf(e)
		if !networkMatch(objOf(r["networkMatch"]), proto, ip, sport, port, str(ro, "IPAddress")) {
			continue
		}
		if r["preview"] == true {
			continue
		}
		return str(r, "action") == "allow"
	}
	return true
}

// networkMatch is a network edge rule's match: each field set must hold.
func networkMatch(m obj, proto, ip string, sport, dport int, dst string) bool {
	if m == nil {
		return false
	}
	anyIn := func(l []string, f func(string) bool) bool {
		if len(l) == 0 {
			return true
		}
		for _, v := range l {
			if f(v) {
				return true
			}
		}
		return false
	}
	portIn := func(spec string, p int) bool {
		lo, hi, ok := strings.Cut(spec, "-")
		a, _ := strconv.Atoi(lo)
		b := a
		if ok {
			b, _ = strconv.Atoi(hi)
		}
		return p >= a && p <= b
	}
	return anyIn(strs(m, "srcIpRanges"), func(c string) bool { return inIPRange(ip, c) }) &&
		anyIn(strs(m, "destIpRanges"), func(c string) bool { return inIPRange(dst, c) }) &&
		anyIn(strs(m, "ipProtocols"), func(v string) bool { return strings.EqualFold(v, proto) }) &&
		anyIn(strs(m, "srcPorts"), func(v string) bool { return portIn(v, sport) }) &&
		anyIn(strs(m, "destPorts"), func(v string) bool { return portIn(v, dport) })
}
