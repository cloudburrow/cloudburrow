package compute

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Health checking (#2153), as https://docs.cloud.google.com/load-balancing/docs/health-check-concepts
// describes it (read 2026-11-22): each endpoint is probed every
// checkIntervalSec, a probe failing after timeoutSec; healthyThreshold
// successes in a row make it HEALTHY and unhealthyThreshold failures
// UNHEALTHY, and it starts UNHEALTHY. Probes come from Google's probe
// ranges (35.191.0.0/16 and 130.211.0.0/22; for target pools' legacy
// health checks 35.191.0.0/16, 209.85.152.0/22 and 209.85.204.0/22), so a
// VM whose network's firewall rules do not allow both ranges on the probed
// port fails every probe, as on Google ("Firewall rules for health
// checks"). An HTTP probe needs a 200 (and the expected response, when
// set); a TCP or SSL probe a connection or handshake (and the response);
// a gRPC probe grpc.health.v1's SERVING. A stopped instance is UNHEALTHY.

var (
	hcProbeSources     = []string{"35.191.0.1", "130.211.0.1"}
	legacyProbeSources = []string{"35.191.0.1", "209.85.152.1", "209.85.204.1"}
)

// lbProbeTask is one endpoint to probe with one health check.
type lbProbeTask struct {
	key     string
	probe   hcProbe
	ep      lbEndpoint
	port    int
	allowed bool // the firewall lets the probes in
}

type hcState struct {
	healthy   bool
	ok, fail  int
	cancel    context.CancelFunc
	probeKey  string
	allowed   bool
	lastError string
	// decided is whether a threshold has been reached, and unreachable
	// whether the last failure was a connection's or a timeout's (not a
	// response's): autohealing's detailedHealthState (#2225).
	decided, unreachable bool
	// ep is the endpoint as the last sync saw it: an instance that was
	// still starting when its probe began is running by now.
	ep lbEndpoint
}

// lbHealthState is every probe's state.
type lbHealthState struct {
	mu     sync.Mutex
	states map[string]*hcState
}

func (h *lbHealthState) get(key string) (hcState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.states[key]
	if !ok {
		return hcState{}, false
	}
	return *st, true
}

// probeKeyOf is the key of a health check's probe of an endpoint.
func probeKeyOf(hc string, p hcProbe, ep lbEndpoint, port int) string {
	return hc + "|" + p.key() + "|" + ep.id + "|" + strconv.Itoa(port)
}

// probePort is the port a health check probes an endpoint on.
func (s *Server) probePortLocked(p hcProbe, ep lbEndpoint) int {
	switch {
	case p.port != 0:
		return p.port
	case p.portName != "":
		return namedPort(s.res[ep.group], p.portName)
	}
	return ep.port
}

// firewallAllowsLocked is whether v's network's firewall rules let TCP
// from src in to port, as https://docs.cloud.google.com/firewall/docs/firewalls
// evaluates them: the applicable ingress rule of the lowest priority
// number decides, a deny before an allow of the same priority, and the
// implied rule denies the rest.
func (s *Server) firewallAllowsLocked(v vmNet, src string, port int) bool {
	ip := net.ParseIP(src)
	var rules []fwRule
	for _, o := range s.firewallsOfLocked(v.network) {
		if str(o, "direction") == "EGRESS" || !appliesTo(o, v) {
			continue
		}
		p, _ := o["priority"].(float64)
		rules = append(rules, fwRule{o: o, prio: int(p), deny: len(sliceOf(o["denied"])) > 0})
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].prio != rules[j].prio {
			return rules[i].prio < rules[j].prio
		}
		return rules[i].deny && !rules[j].deny
	})
	inRanges := func(l []string, ip net.IP) bool {
		for _, r := range l {
			if _, n, err := net.ParseCIDR(r); err == nil && n.Contains(ip) {
				return true
			}
			if r == ip.String() {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		if !inRanges(strsAny(r.o["sourceRanges"]), ip) {
			continue
		}
		if dst := strsAny(r.o["destinationRanges"]); len(dst) > 0 && !inRanges(dst, net.ParseIP(v.ip)) {
			continue
		}
		entries := sliceOf(r.o["allowed"])
		if r.deny {
			entries = sliceOf(r.o["denied"])
		}
		for _, e := range entries {
			if protoPortMatch(objOf(e), port) {
				return !r.deny
			}
		}
	}
	return false
}

// protoPortMatch is whether a firewall entry covers TCP port.
func protoPortMatch(e obj, port int) bool {
	switch strings.ToLower(str(e, "IPProtocol")) {
	case "all":
		return true
	case "tcp", "6":
	default:
		return false
	}
	ports := strsAny(e["ports"])
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		lo, hi := p, p
		if i := strings.Index(p, "-"); i >= 0 {
			lo, hi = p[:i], p[i+1:]
		}
		a, _ := strconv.Atoi(lo)
		z, _ := strconv.Atoi(hi)
		if port >= a && port <= z {
			return true
		}
	}
	return false
}

// probesAllowedLocked is whether every probe source reaches ep's port.
func (s *Server) probesAllowedLocked(ep lbEndpoint, port int, sources []string) bool {
	if !ep.hasVM {
		return true
	}
	for _, src := range sources {
		if !s.firewallAllowsLocked(ep.vmNet, src, port) {
			return false
		}
	}
	return true
}

// wantedProbesLocked are the probes every backend service and target pool
// with a health check needs.
func (s *Server) wantedProbesLocked() []lbProbeTask {
	var out []lbProbeTask
	for k, o := range s.res {
		switch str(o, "kind") {
		case "compute#backendService":
			set, _ := s.backendSetLocked(k)
			if set.hc == "" {
				continue
			}
			for _, g := range set.groups {
				for _, ep := range g.endpoints {
					if ep.kind != "vm" && ep.kind != "ip" {
						continue
					}
					port := s.probePortLocked(set.probe, ep)
					out = append(out, lbProbeTask{key: probeKeyOf(set.hc, set.probe, ep, port), probe: set.probe, ep: ep, port: port,
						allowed: s.probesAllowedLocked(ep, port, hcProbeSources)})
				}
			}
		case "compute#targetPool":
			hcs := strs(o, "healthChecks")
			if len(hcs) == 0 {
				continue
			}
			p := probeOf(s.res[hcs[0]])
			for _, inst := range strs(o, "instances") {
				ep, ok := s.instanceEndpointLocked(inst, p.port, "")
				if !ok {
					continue
				}
				out = append(out, lbProbeTask{key: probeKeyOf(hcs[0], p, ep, p.port), probe: p, ep: ep, port: p.port,
					allowed: s.probesAllowedLocked(ep, p.port, legacyProbeSources)})
			}
		}
	}
	// Managed instance groups' autohealing health checks (#2225).
	return append(out, s.autohealProbesLocked()...)
}

// sync starts the probes newly wanted and stops the others.
func (h *lbHealthState) sync(ctx context.Context, s *Server, tasks []lbProbeTask) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.states == nil {
		h.states = map[string]*hcState{}
	}
	want := map[string]lbProbeTask{}
	for _, t := range tasks {
		want[t.key] = t
	}
	for k, st := range h.states {
		if _, ok := want[k]; !ok {
			st.cancel()
			delete(h.states, k)
		}
	}
	for k, t := range want {
		if st, ok := h.states[k]; ok {
			st.allowed, st.ep = t.allowed, t.ep
			continue
		}
		pctx, cancel := context.WithCancel(ctx)
		h.states[k] = &hcState{cancel: cancel, allowed: t.allowed, ep: t.ep}
		go h.run(pctx, s, t)
	}
}

func (h *lbHealthState) stopAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, st := range h.states {
		st.cancel()
		delete(h.states, k)
	}
}

// run probes one endpoint until its context ends.
func (h *lbHealthState) run(ctx context.Context, s *Server, t lbProbeTask) {
	interval := time.Duration(max(t.probe.interval, 1)) * time.Second
	for {
		h.mu.Lock()
		st := h.states[t.key]
		allowed := st != nil && st.allowed
		if st != nil {
			t.ep = st.ep
		}
		h.mu.Unlock()
		if st == nil {
			return
		}
		var err error
		if !allowed {
			err = fmt.Errorf("the firewall does not allow the health check's probe ranges on port %d", t.port)
		} else {
			err = s.probeOnce(ctx, t)
		}
		h.mu.Lock()
		if st := h.states[t.key]; st != nil {
			if err == nil {
				st.ok, st.fail = st.ok+1, 0
				if st.ok >= t.probe.healthy {
					st.healthy, st.decided = true, true
				}
				st.lastError = ""
			} else {
				st.fail, st.ok = st.fail+1, 0
				if st.fail >= t.probe.unhealthy {
					st.healthy, st.decided = false, true
				}
				st.lastError = err.Error()
				_, bad := err.(hcBadResponse)
				st.unreachable = !bad
			}
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// hcBadResponse is a probe failure of an endpoint that answered, but not
// as the health check requires (UNHEALTHY rather than TIMEOUT).
type hcBadResponse struct{ error }

// dialEndpoint is a host address that reaches an instance or hybrid
// endpoint's port.
func (s *Server) dialAddr(ctx context.Context, ep lbEndpoint, port int) (string, error) {
	if ep.kind == "vm" {
		if !ep.running {
			return "", fmt.Errorf("instance %s is not running", ep.instance)
		}
		if s.opts.LB != nil {
			return s.opts.LB.InstanceAddr(ctx, ep.vm, port)
		}
	}
	if ep.ip == "" {
		return "", fmt.Errorf("the endpoint has no address")
	}
	return net.JoinHostPort(ep.ip, strconv.Itoa(port)), nil
}

// proxyV1Header is a PROXY protocol version 1 header.
func proxyV1Header(src, dst net.Addr) string {
	sh, sp, _ := net.SplitHostPort(src.String())
	dh, dp, _ := net.SplitHostPort(dst.String())
	if net.ParseIP(sh).To4() == nil || net.ParseIP(dh).To4() == nil {
		return "PROXY UNKNOWN\r\n"
	}
	return "PROXY TCP4 " + sh + " " + dh + " " + sp + " " + dp + "\r\n"
}

// probeOnce runs one probe.
func (s *Server) probeOnce(ctx context.Context, t lbProbeTask) error {
	timeout := time.Duration(max(t.probe.timeout, 1)) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr, err := s.dialAddr(ctx, t.ep, t.port)
	if err != nil {
		return err
	}
	p := t.probe
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err == nil && p.proxyV1 {
			_, err = io.WriteString(c, "PROXY TCP4 35.191.0.1 "+t.ep.ip+" 40000 "+strconv.Itoa(t.port)+"\r\n")
		}
		return c, err
	}
	host := p.host
	if host == "" {
		host = t.ep.ip
	}
	switch p.typ {
	case "HTTP", "HTTPS", "HTTP2":
		scheme := "http"
		tr := &http.Transport{DialContext: dial, DisableKeepAlives: true}
		if p.typ != "HTTP" {
			scheme = "https"
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, ServerName: p.host} //nolint:gosec // Google's probes do not verify backends' certificates
			tr.ForceAttemptHTTP2 = p.typ == "HTTP2"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+p.path, nil)
		if err != nil {
			return err
		}
		req.Host = host
		req.Header.Set("User-Agent", "GoogleHC/1.0")
		resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode != http.StatusOK {
			return hcBadResponse{fmt.Errorf("HTTP %d", resp.StatusCode)}
		}
		if p.response != "" && !strings.HasPrefix(string(body), p.response) {
			return hcBadResponse{fmt.Errorf("the response does not start with %q", p.response)}
		}
		return nil
	case "TCP", "SSL":
		c, err := dial(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(timeout))
		if p.typ == "SSL" {
			tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // as Google's probes
			if err := tc.HandshakeContext(ctx); err != nil {
				return err
			}
			c = tc
		}
		if p.request != "" {
			if _, err := io.WriteString(c, p.request); err != nil {
				return err
			}
		}
		if p.response != "" {
			buf := make([]byte, len(p.response))
			if _, err := io.ReadFull(bufio.NewReader(c), buf); err != nil || string(buf) != p.response {
				return hcBadResponse{fmt.Errorf("the response is not %q", p.response)}
			}
		}
		return nil
	case "GRPC", "GRPC_WITH_TLS":
		creds := insecure.NewCredentials()
		if p.typ == "GRPC_WITH_TLS" {
			creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // as Google's probes
		}
		conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(creds))
		if err != nil {
			return err
		}
		defer conn.Close()
		r, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: p.grpcName})
		if err != nil {
			return err
		}
		if r.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			return hcBadResponse{fmt.Errorf("gRPC status %s", r.GetStatus())}
		}
		return nil
	}
	return fmt.Errorf("health check type %s", p.typ)
}

// endpointHealthyLocked is whether ep may take traffic from set.
func (s *Server) endpointHealthyLocked(set lbBackendSet, ep lbEndpoint) bool {
	if !ep.running {
		return false
	}
	if set.hc == "" || (ep.kind != "vm" && ep.kind != "ip") {
		return true
	}
	st, ok := s.lb.health.get(probeKeyOf(set.hc, set.probe, ep, s.probePortLocked(set.probe, ep)))
	return ok && st.healthy
}

// lbHealthLocked are getHealth's statuses of one backend group.
func (s *Server) lbHealthLocked(bsRel, group string) []obj {
	set, ok := s.backendSetLocked(bsRel)
	if !ok {
		return nil
	}
	var out []obj
	for _, g := range set.groups {
		if str(g.backend, "group") != group {
			continue
		}
		for _, ep := range g.endpoints {
			if ep.kind != "vm" && ep.kind != "ip" {
				continue
			}
			state := "UNHEALTHY"
			if s.endpointHealthyLocked(set, ep) {
				state = "HEALTHY"
			}
			h := obj{"healthState": state, "ipAddress": ep.ip, "port": float64(ep.port)}
			if ep.instance != "" {
				h["instance"] = ep.instance
			}
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return str(out[i], "instance")+str(out[i], "ipAddress") < str(out[j], "instance")+str(out[j], "ipAddress")
	})
	return out
}

// poolHealthLocked is a target pool instance's health: HEALTHY with no
// health check while it runs, else as its legacy health check found it.
func (s *Server) poolHealthLocked(poolRel string, pool obj, inst string) obj {
	hcs := strs(pool, "healthChecks")
	port := 0
	var p hcProbe
	if len(hcs) > 0 {
		p = probeOf(s.res[hcs[0]])
		port = p.port
	}
	ep, _ := s.instanceEndpointLocked(inst, port, "")
	state := "UNHEALTHY"
	if ep.running {
		state = "HEALTHY"
		if len(hcs) > 0 {
			st, ok := s.lb.health.get(probeKeyOf(hcs[0], p, ep, port))
			if !ok || !st.healthy {
				state = "UNHEALTHY"
			}
		}
	}
	return obj{"healthState": state, "instance": inst, "ipAddress": ep.ip}
}

// poolInstanceHealthyLocked is whether a pool's instance takes traffic.
func (s *Server) poolInstanceHealthyLocked(poolRel string, pool obj, inst string) bool {
	return str(s.poolHealthLocked(poolRel, pool, inst), "healthState") == "HEALTHY"
}

// endpointHealthsLocked are a network endpoint's health under each backend
// service that uses its group, for listNetworkEndpoints.
func (s *Server) endpointHealthsLocked(neg string, e obj) []any {
	var out []any
	for _, u := range s.lbUsersLocked(neg) {
		if str(s.res[u], "kind") != "compute#backendService" {
			continue
		}
		set, _ := s.backendSetLocked(u)
		for _, g := range set.groups {
			for _, ep := range g.endpoints {
				if str(g.backend, "group") != neg || endpointKey(e) != endpointKey(obj{"instance": lastSeg(ep.instance), "ipAddress": ep.ip, "fqdn": ep.fqdn, "port": float64(ep.port)}) {
					continue
				}
				state := "UNHEALTHY"
				if s.endpointHealthyLocked(set, ep) {
					state = "HEALTHY"
				}
				h := obj{"healthState": state, "backendService": obj{"backendService": u}}
				if set.hc != "" {
					h["healthCheck"] = obj{"healthCheck": set.hc}
				}
				out = append(out, h)
			}
		}
	}
	return out
}
