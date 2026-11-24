package compute

import (
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoints and balancing (#2153), as
// https://docs.cloud.google.com/load-balancing/docs/backend-service
// ("Traffic distribution", "Balancing mode", "Capacity scaler", "Session
// affinity") describes them (read 2026-11-22): a backend's capacity is its
// balancing mode's (UTILIZATION's maxUtilization per healthy instance,
// RATE's requests per second, CONNECTION's connections) times its
// capacity scaler, 0 draining it; requests go to the backends with
// capacity left, in proportion to it, and spill over to the others once
// every backend is full; within a backend the locality policy (round
// robin by default, LEAST_REQUEST, RANDOM, RING_HASH, MAGLEV) picks the
// endpoint; session affinity (client IP, generated or HTTP cookie,
// header field) keeps a client on the endpoint its key hashes to, among
// the healthy ones.

// lbEndpoint is one place a backend sends traffic.
type lbEndpoint struct {
	id    string // stable key
	group string // the backend group
	// kind is vm, ip (hybrid), internet, serverless, psc.
	kind     string
	instance string // an instance's relative name
	vm       VMSpec
	running  bool
	ip       string
	port     int
	fqdn     string
	neg      obj // a serverless or PSC group
	vmNet    vmNet
	hasVM    bool
}

// namedPort is a group's port of that name, 0 when it has none.
func namedPort(g obj, name string) int {
	for _, e := range listOf(g["namedPorts"]) {
		np := objOf(e)
		if str(np, "name") == name {
			return int(numOr(np, "port", 0))
		}
	}
	return 0
}

// instanceEndpointLocked is an instance as an endpoint on port.
func (s *Server) instanceEndpointLocked(rel string, port int, group string) (lbEndpoint, bool) {
	o, ok := s.res[rel]
	if !ok {
		return lbEndpoint{}, false
	}
	v, _ := vmNetOf(rel, o)
	ep := lbEndpoint{id: rel + ":" + strconv.Itoa(port), group: group, kind: "vm", instance: rel, running: str(o, "status") == stRunning,
		ip: v.ip, port: port, vmNet: v, hasVM: true}
	if s.opts.VMs != nil {
		ep.vm = s.vmSpecLocked(o)
	} else {
		ep.vm = VMSpec{Name: str(o, "name"), InternalIP: v.ip, Zone: lastSeg(str(o, "zone"))}
	}
	return ep, true
}

// groupEndpointsLocked are a backend group's endpoints, for a backend
// service of serving port name portName (and deprecated port).
func (s *Server) groupEndpointsLocked(group, portName string, defPort int) []lbEndpoint {
	g, ok := s.res[group]
	if !ok {
		return nil
	}
	var out []lbEndpoint
	switch groupKind(group, g) {
	case "ig":
		port := namedPort(g, portName)
		if port == 0 {
			port = defPort
		}
		for _, inst := range s.groupInstancesLocked(g) {
			if ep, ok := s.instanceEndpointLocked(inst, port, group); ok {
				out = append(out, ep)
			}
		}
	case "zonalNEG", "hybrid", "internet":
		project := strings.Split(group, "/")[1]
		zone := lastSeg(str(g, "zone"))
		for _, e := range listOf(g["_endpoints"]) {
			ep := objOf(e)
			port := int(numOr(ep, "port", numOr(g, "defaultPort", 0)))
			switch {
			case str(ep, "instance") != "":
				if le, ok := s.instanceEndpointLocked(zoneRel(project, zone, "instances", str(ep, "instance")), port, group); ok {
					le.ip = str(ep, "ipAddress")
					out = append(out, le)
				}
			case str(ep, "fqdn") != "":
				out = append(out, lbEndpoint{id: group + "|" + str(ep, "fqdn") + ":" + strconv.Itoa(port), group: group, kind: "internet",
					fqdn: str(ep, "fqdn"), port: port, running: true})
			default:
				kind := "ip"
				if strings.HasPrefix(str(g, "networkEndpointType"), "INTERNET") {
					kind = "internet"
				}
				out = append(out, lbEndpoint{id: group + "|" + str(ep, "ipAddress") + ":" + strconv.Itoa(port), group: group, kind: kind,
					ip: str(ep, "ipAddress"), port: port, running: true})
			}
		}
	case "serverless":
		out = append(out, lbEndpoint{id: group, group: group, kind: "serverless", neg: g, running: true})
	case "psc":
		out = append(out, lbEndpoint{id: group, group: group, kind: "psc", neg: g, running: true})
	}
	return out
}

// lbBackendSet is a backend service's backends as one request sees them.
type lbBackendSet struct {
	bs       obj
	rel      string
	groups   []lbGroup
	hc       string // the health check, "" for none
	probe    hcProbe
	scheme   string
	protocol string
}

type lbGroup struct {
	backend   obj
	endpoints []lbEndpoint
	healthy   []lbEndpoint
}

// backendSetLocked snapshots a backend service's backends and their
// health.
func (s *Server) backendSetLocked(rel string) (lbBackendSet, bool) {
	bs, ok := s.res[rel]
	if !ok {
		return lbBackendSet{}, false
	}
	set := lbBackendSet{bs: deepCopy(bs).(obj), rel: rel, scheme: str(bs, "loadBalancingScheme"), protocol: str(bs, "protocol")}
	if hcs := strs(bs, "healthChecks"); len(hcs) > 0 {
		set.hc = hcs[0]
		set.probe = probeOf(s.res[set.hc])
	}
	for _, e := range listOf(bs["backends"]) {
		be := objOf(e)
		g := lbGroup{backend: be, endpoints: s.groupEndpointsLocked(str(be, "group"), str(bs, "portName"), int(numOr(bs, "port", 80)))}
		for _, ep := range g.endpoints {
			if s.endpointHealthyLocked(set, ep) {
				g.healthy = append(g.healthy, ep)
			}
		}
		set.groups = append(set.groups, g)
	}
	return set, true
}

// lbBalanceState is what balancing remembers: request rates, connections
// in flight and the round robin's position.
type lbBalanceState struct {
	mu       sync.Mutex
	rr       map[string]uint64
	inflight map[string]int
	rate     map[string]*rateWindow
}

// rateWindow counts requests in the last second.
type rateWindow struct {
	at []time.Time
}

func (r *rateWindow) add(now time.Time) {
	r.trim(now)
	r.at = append(r.at, now)
}

func (r *rateWindow) trim(now time.Time) {
	i := 0
	for i < len(r.at) && now.Sub(r.at[i]) > time.Second {
		i++
	}
	r.at = r.at[i:]
}

func (r *rateWindow) count(now time.Time) int {
	r.trim(now)
	return len(r.at)
}

// capacityOf is a backend group's capacity and how much of it is used.
func (b *lbBalanceState) capacityOf(g lbGroup, now time.Time) (capacity, used float64) {
	be := g.backend
	scaler := numOr(be, "capacityScaler", 1)
	n := float64(len(g.healthy))
	switch str(be, "balancingMode") {
	case "RATE":
		max := numOr(be, "maxRate", 0)
		if v, ok := num(be, "maxRatePerInstance"); ok {
			max = v * n
		}
		if v, ok := num(be, "maxRatePerEndpoint"); ok {
			max = v * n
		}
		capacity = max * scaler
		for _, ep := range g.healthy {
			if r := b.rate[ep.id]; r != nil {
				used += float64(r.count(now))
			}
		}
	case "CONNECTION":
		max := numOr(be, "maxConnections", 0)
		if v, ok := num(be, "maxConnectionsPerInstance"); ok {
			max = v * n
		}
		if v, ok := num(be, "maxConnectionsPerEndpoint"); ok {
			max = v * n
		}
		if max == 0 {
			max = 1e9 * n // a passthrough backend: no cap
		}
		capacity = max * scaler
		for _, ep := range g.healthy {
			used += float64(b.inflight[ep.id])
		}
	default: // UTILIZATION, and serverless, internet and PSC backends
		capacity = numOr(be, "maxUtilization", 1) * n * scaler * 100
		for _, ep := range g.healthy {
			used += float64(b.inflight[ep.id])
		}
		if _, ok := be["balancingMode"]; !ok {
			capacity = scaler * 1e9
		}
	}
	return capacity, used
}

// pick chooses an endpoint of set for a request whose affinity key is key
// ("" for none); exclude are endpoints already tried. ok is false when no
// endpoint is healthy.
func (b *lbBalanceState) pick(set lbBackendSet, key string, exclude map[string]bool, failOpen bool) (lbEndpoint, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rr == nil {
		b.rr, b.inflight, b.rate = map[string]uint64{}, map[string]int{}, map[string]*rateWindow{}
	}
	now := time.Now()
	type cand struct {
		g        lbGroup
		cap, use float64
	}
	var cands []cand
	for _, g := range set.groups {
		var eps []lbEndpoint
		for _, ep := range g.healthy {
			if !exclude[ep.id] {
				eps = append(eps, ep)
			}
		}
		if len(eps) == 0 || numOr(g.backend, "capacityScaler", 1) == 0 {
			continue
		}
		g.healthy = eps
		c, u := b.capacityOf(g, now)
		cands = append(cands, cand{g, c, u})
	}
	if len(cands) == 0 && failOpen {
		// A passthrough load balancer with no healthy backend sends to
		// every backend (load-balancing/docs/internal#traffic_distribution).
		for _, g := range set.groups {
			if len(g.endpoints) > 0 {
				g.healthy = g.endpoints
				cands = append(cands, cand{g, 1, 0})
			}
		}
	}
	if len(cands) == 0 {
		return lbEndpoint{}, false
	}
	var all []lbEndpoint
	for _, c := range cands {
		all = append(all, c.g.healthy...)
	}
	var ep lbEndpoint
	if key != "" {
		// Affinity: the endpoint the key hashes highest with (rendezvous
		// hashing), among every healthy one, so a client stays put while
		// its endpoint is healthy.
		best := uint64(0)
		for _, e := range all {
			h := fnv.New64a()
			_, _ = h.Write([]byte(key + "|" + e.id))
			if v := h.Sum64(); v >= best {
				best, ep = v, e
			}
		}
	} else {
		// Backends with capacity left, in proportion to what is left;
		// every backend, in proportion to capacity, once all are full.
		var free []cand
		for _, c := range cands {
			if c.cap > c.use {
				free = append(free, c)
			}
		}
		pool := free
		weight := func(c cand) float64 { return c.cap - c.use }
		if len(pool) == 0 {
			pool = cands
			weight = func(c cand) float64 { return c.cap + 1 }
		}
		total := 0.0
		for _, c := range pool {
			total += weight(c)
		}
		chosen := pool[0]
		if total > 0 {
			r := rand.Float64() * total
			for _, c := range pool {
				if r -= weight(c); r <= 0 {
					chosen = c
					break
				}
			}
		}
		eps := chosen.g.healthy
		sort.Slice(eps, func(i, j int) bool { return eps[i].id < eps[j].id })
		switch str(set.bs, "localityLbPolicy") {
		case "RANDOM":
			ep = eps[rand.IntN(len(eps))]
		case "LEAST_REQUEST":
			ep = eps[0]
			for _, e := range eps[1:] {
				if b.inflight[e.id] < b.inflight[ep.id] {
					ep = e
				}
			}
		default:
			n := b.rr[set.rel+"|"+chosen.g.backend["group"].(string)]
			b.rr[set.rel+"|"+chosen.g.backend["group"].(string)] = n + 1
			ep = eps[n%uint64(len(eps))]
		}
	}
	if b.rate[ep.id] == nil {
		b.rate[ep.id] = &rateWindow{}
	}
	b.rate[ep.id].add(now)
	b.inflight[ep.id]++
	return ep, true
}

// done ends a request or connection on ep.
func (b *lbBalanceState) done(ep lbEndpoint) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight[ep.id] > 0 {
		b.inflight[ep.id]--
	}
}

// hashKey is a string's affinity key.
func hashKey(parts ...string) string { return strings.Join(parts, "|") }
