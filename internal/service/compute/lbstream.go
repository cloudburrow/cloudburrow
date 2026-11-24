package compute

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// The proxy and passthrough Network Load Balancers' connections (#2153):
//
//   - a target TCP proxy sends each connection to an endpoint of its
//     backend service, a target SSL proxy first ends TLS with its
//     certificates and SSL policy; either writes a PROXY protocol version 1
//     header first when its proxyHeader is PROXY_V1
//     (load-balancing/docs/tcp, load-balancing/docs/ssl);
//   - a target pool sends to one of its healthy instances, on the rule's
//     port, with its session affinity, to its backup pool once fewer than
//     failoverRatio of its instances are healthy, and to every instance
//     when none is (load-balancing/docs/target-pools);
//   - a target instance sends to its instance (load-balancing/docs/protocol-forwarding);
//   - a passthrough backend service sends to one of its backends'
//     instances on the rule's port, every instance when none is healthy
//     (load-balancing/docs/network/networklb-backend-service).
//
// Each copies bytes both ways until either side closes.

func (s *Server) serveStream(rule, kind string, port int, c net.Conn) {
	defer c.Close()
	s.mu.Lock()
	ro := deepCopy(s.res[rule]).(obj)
	target := str(ro, "target")
	to := deepCopy(s.res[target]).(obj)
	s.mu.Unlock()
	if ro == nil {
		return
	}
	if kind == "l4" && !s.l4Allowed(rule, "TCP", c.RemoteAddr(), port) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var (
		up       net.Conn
		ep       lbEndpoint
		set      lbBackendSet
		proxyHdr bool
		err      error
	)
	switch {
	case kind == "stream" || kind == "ssl":
		if kind == "ssl" {
			cfg := s.lbTLSConfig(rule)
			tc := tls.Server(c, &tls.Config{GetConfigForClient: cfg})
			if err := tc.HandshakeContext(ctx); err != nil {
				return
			}
			c = tc
		}
		proxyHdr = str(to, "proxyHeader") == "PROXY_V1"
		s.mu.Lock()
		set, _ = s.backendSetLocked(str(to, "service"))
		s.mu.Unlock()
		up, ep, err = s.dialSet(ctx, set, streamKey(set.bs, c), 0, false)
	case str(ro, "backendService") != "":
		s.mu.Lock()
		set, _ = s.backendSetLocked(str(ro, "backendService"))
		s.mu.Unlock()
		up, ep, err = s.dialSet(ctx, set, streamKey(set.bs, c), port, true)
	case strings.Contains(target, "/targetInstances/"):
		s.mu.Lock()
		e, ok := s.instanceEndpointLocked(str(to, "instance"), port, "")
		s.mu.Unlock()
		if !ok {
			return
		}
		var addr string
		if addr, err = s.dialAddr(ctx, e, port); err == nil {
			up, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	case strings.Contains(target, "/targetPools/"):
		up, err = s.dialPool(ctx, target, c, port)
	default:
		return
	}
	if err != nil || up == nil {
		return
	}
	if ep.id != "" {
		defer s.lb.bal.done(ep)
	}
	defer up.Close()
	if proxyHdr {
		if _, err := io.WriteString(up, proxyV1Header(c.RemoteAddr(), c.LocalAddr())); err != nil {
			return
		}
	}
	pipe(c, up)
}

// streamKey is a connection's affinity key under bs's session affinity.
func streamKey(bs obj, c net.Conn) string {
	ip, port, _ := net.SplitHostPort(c.RemoteAddr().String())
	switch str(bs, "sessionAffinity") {
	case "CLIENT_IP", "CLIENT_IP_PROTO", "CLIENT_IP_NO_DESTINATION":
		return hashKey(ip)
	case "CLIENT_IP_PORT_PROTO":
		return hashKey(ip, port)
	}
	return ""
}

// dialSet connects to an endpoint of set, on port (or the endpoint's own
// port when 0), trying the others when one refuses.
func (s *Server) dialSet(ctx context.Context, set lbBackendSet, key string, port int, failOpen bool) (net.Conn, lbEndpoint, error) {
	tried := map[string]bool{}
	for range 8 {
		ep, ok := s.lb.bal.pick(set, key, tried, failOpen)
		if !ok {
			return nil, lbEndpoint{}, errNoHealthy
		}
		tried[ep.id] = true
		p := ep.port
		if port != 0 {
			p = port
		}
		addr, err := s.dialAddr(ctx, ep, p)
		if err == nil {
			var c net.Conn
			if c, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr); err == nil {
				return c, ep, nil
			}
		}
		s.lb.bal.done(ep)
	}
	return nil, lbEndpoint{}, errNoHealthy
}

// dialPool connects to a target pool's instance.
func (s *Server) dialPool(ctx context.Context, poolRel string, c net.Conn, port int) (net.Conn, error) {
	ip, cport, _ := net.SplitHostPort(c.RemoteAddr().String())
	for depth := 0; depth < 2; depth++ {
		s.mu.Lock()
		pool := deepCopy(s.res[poolRel]).(obj)
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
		backup := str(pool, "backupPool")
		s.mu.Unlock()
		if pool == nil {
			return nil, errors.New("the target pool is gone")
		}
		ratio := numOr(pool, "failoverRatio", 0)
		if backup != "" && depth == 0 && len(all) > 0 && float64(len(healthy))/float64(len(all)) < ratio {
			poolRel = backup
			continue
		}
		cands := healthy
		if len(cands) == 0 {
			cands = all // no healthy instance: every instance
		}
		if len(cands) == 0 {
			return nil, errNoHealthy
		}
		key := ""
		switch str(pool, "sessionAffinity") {
		case "CLIENT_IP":
			key = hashKey(ip)
		case "CLIENT_IP_PROTO":
			key = hashKey(ip, "tcp")
		default:
			key = hashKey(ip, cport)
		}
		set := lbBackendSet{rel: poolRel, bs: obj{}, groups: []lbGroup{{backend: obj{"group": poolRel}, endpoints: cands, healthy: cands}}}
		up, ep, err := s.dialSet(ctx, set, key, port, false)
		if err == nil {
			s.lb.bal.done(ep)
		}
		return up, err
	}
	return nil, errNoHealthy
}

// pipe copies both ways until either side is done.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	<-done
}
