package compute

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/vmrelay"
)

// The load balancers' data plane (#2153): each load-balancing forwarding
// rule's ports are listened on at 127.0.0.1 (lbListenLocked), and every
// connection or request is routed as Google routes it, from the records
// as they are when it arrives:
//
//   - a target HTTP or HTTPS proxy: the URL map (lburlmaps.go's lbMatch)
//     chooses a backend service or bucket, or a redirect (lbhttp.go);
//   - a target TCP or SSL proxy: its backend service (lbstream.go);
//   - a target pool, a target instance or a passthrough backend service:
//     an instance, on the rule's own port, as a passthrough Network Load
//     Balancer keeps the destination port (lbstream.go).
//
// A backend service's endpoints (lbbalance.go) are its instance groups'
// running instances on its named port, its network endpoint groups'
// endpoints, a Cloud Run service or function through the cluster's
// ingress, an internet endpoint, or a Private Service Connect producer;
// health checks (lbhealth.go) probe them, the balancing modes weigh them,
// and session affinity keeps a client on one.

// LBBackends reaches what load balancers send traffic to; nil leaves only
// the backends this package reaches itself (hybrid and internet
// endpoints, and Private Service Connect producers on this machine).
type LBBackends interface {
	// InstanceAddr is a host address (host:port) that reaches port of the
	// running instance vm.
	InstanceAddr(ctx context.Context, vm VMSpec, port int) (string, error)
	// Ingress is the host address of the Cloud Run ingress, "" when none.
	Ingress() string
	// LocalAPI is the base URL (http://host:port) of a local API by its
	// gcloud property name (run, storage, cloudfunctions), "" when it is not
	// served.
	LocalAPI(api string) string
}

// lbBind is the host address the listeners are on.
const lbBind = "127.0.0.1"

// lbPlane is the data plane's state.
type lbPlane struct {
	mu        sync.Mutex
	listeners map[string]*lbRuleListeners // by forwarding rule
	nudge     chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	health    lbHealthState
	bal       lbBalanceState
	cache     lbCache
	rates     lbRates
	cdn       lbCDN
	iap       lbIAPKey
}

// lbRuleListeners are one rule's listeners, and the kind they serve.
type lbRuleListeners struct {
	kind string // http, https, stream, ssl, l4
	ls   []*lbListener
}

type lbListener struct {
	rulePort int
	addr     string
	ln       net.Listener
	// inj is ln, which also takes the connections VMs relay to the rule
	// (vmrelay.go).
	inj *vmrelay.Listener
	pc  net.PacketConn
	srv *http.Server
}

func newLBPlane() *lbPlane {
	return &lbPlane{listeners: map[string]*lbRuleListeners{}, nudge: make(chan struct{}, 1)}
}

// lbChangedLocked tells the data plane a record changed.
func (s *Server) lbChangedLocked() {
	select {
	case s.lb.nudge <- struct{}{}:
	default:
	}
}

// ruleKindLocked is what a rule's listeners serve, "" when nothing.
func (s *Server) ruleKindLocked(o obj) string {
	t := str(o, "target")
	if str(o, "IPProtocol") == "UDP" {
		if str(o, "backendService") != "" || strings.Contains(t, "/targetPools/") || strings.Contains(t, "/targetInstances/") {
			return "udp"
		}
		return ""
	}
	if str(o, "IPProtocol") != "" && str(o, "IPProtocol") != "TCP" {
		return "" // ESP, AH, SCTP, ICMP, L3_DEFAULT: not carried (docs/gaps.md)
	}
	if str(o, "backendService") != "" {
		return "l4"
	}
	switch {
	case strings.Contains(t, "/targetHttpProxies/"):
		return "http"
	case strings.Contains(t, "/targetHttpsProxies/"):
		return "https"
	case strings.Contains(t, "/targetTcpProxies/"):
		return "stream"
	case strings.Contains(t, "/targetSslProxies/"):
		return "ssl"
	case strings.Contains(t, "/targetPools/"), strings.Contains(t, "/targetInstances/"):
		return "l4"
	}
	return ""
}

// lbListenLocked opens (or reopens, keeping its ports) a rule's
// listeners, and records their addresses on it.
func (s *Server) lbListenLocked(rel string) {
	o, ok := s.res[rel]
	if !ok {
		return
	}
	kind := s.ruleKindLocked(o)
	p := s.lb
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.listeners[rel]
	if cur != nil && cur.kind == kind {
		return
	}
	prev := map[int]string{}
	for _, e := range strs(o, "cloudburrowAddresses") {
		if i := strings.Index(e, "="); i > 0 {
			n, _ := strconv.Atoi(e[:i])
			prev[n] = e[i+1:]
		}
	}
	if cur != nil {
		for _, l := range cur.ls {
			prev[l.rulePort] = l.addr
			l.close()
		}
		delete(p.listeners, rel)
	}
	delete(o, "cloudburrowAddress")
	delete(o, "cloudburrowAddresses")
	ports := rulePorts(o)
	if kind == "" || len(ports) == 0 {
		return
	}
	if len(ports) > maxRulePorts {
		ports = ports[:maxRulePorts]
	}
	rl := &lbRuleListeners{kind: kind}
	var addrs []any
	for _, port := range ports {
		var l *lbListener
		if kind == "udp" {
			var pc net.PacketConn
			var err error
			if a := prev[port]; a != "" {
				pc, err = net.ListenPacket("udp", a)
			}
			if pc == nil || err != nil {
				pc, err = net.ListenPacket("udp", net.JoinHostPort(lbBind, "0"))
			}
			if err != nil {
				continue
			}
			l = &lbListener{rulePort: port, addr: pc.LocalAddr().String(), pc: pc}
		} else {
			var ln net.Listener
			var err error
			if a := prev[port]; a != "" {
				ln, err = net.Listen("tcp", a)
			}
			if ln == nil || err != nil {
				ln, err = net.Listen("tcp", net.JoinHostPort(lbBind, "0"))
			}
			if err != nil {
				continue
			}
			inj := vmrelay.NewListener(ln)
			l = &lbListener{rulePort: port, addr: ln.Addr().String(), ln: inj, inj: inj}
		}
		s.serveListener(rel, kind, l)
		rl.ls = append(rl.ls, l)
		addrs = append(addrs, strconv.Itoa(port)+"="+l.addr)
	}
	if len(rl.ls) == 0 {
		return
	}
	p.listeners[rel] = rl
	o["cloudburrowAddress"] = rl.ls[0].addr
	o["cloudburrowAddresses"] = addrs
}

func (l *lbListener) close() {
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = l.srv.Shutdown(ctx)
		cancel()
	}
	if l.pc != nil {
		_ = l.pc.Close()
		return
	}
	_ = l.ln.Close()
}

// serveListener serves one listener of a rule.
func (s *Server) serveListener(rule, kind string, l *lbListener) {
	switch kind {
	case "udp":
		go s.serveUDP(rule, l.rulePort, l.pc)
	case "http", "https":
		h := &lbHTTP{s: s, rule: rule, port: l.rulePort, tls: kind == "https"}
		l.srv = &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 610 * time.Second}
		if kind == "https" {
			l.srv.TLSConfig = &tls.Config{GetConfigForClient: s.lbTLSConfig(rule), NextProtos: []string{"h2", "http/1.1"}}
			go func() { _ = l.srv.ServeTLS(l.ln, "", "") }()
			return
		}
		go func() { _ = l.srv.Serve(l.ln) }()
	default:
		go func() {
			for {
				c, err := l.ln.Accept()
				if err != nil {
					if errors.Is(err, net.ErrClosed) {
						return
					}
					time.Sleep(50 * time.Millisecond)
					continue
				}
				go s.serveStream(rule, kind, l.rulePort, c)
			}
		}()
	}
}

// lbTLSConfig is a TLS listener's configuration, read at each handshake:
// the proxy's certificates (the one whose names match the client's SNI,
// else the first) and its SSL policy.
func (s *Server) lbTLSConfig(rule string) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		s.mu.Lock()
		proxy := deepCopy(s.res[str(s.res[rule], "target")]).(obj)
		var policy obj
		if sp := str(proxy, "sslPolicy"); sp != "" {
			policy = deepCopy(s.res[sp]).(obj)
		}
		s.mu.Unlock()
		// Certificate Manager is asked outside the lock: it issues
		// certificates through this server.
		certs := s.proxyCertificates(proxy, hello.ServerName)
		if len(certs) == 0 {
			return nil, errors.New("the target proxy has no active SSL certificate")
		}
		minV, suites := tlsConfigOf(policy)
		cfg := &tls.Config{Certificates: certs, MinVersion: minV, CipherSuites: suites, NextProtos: []string{"h2", "http/1.1"}}
		// A target HTTPS proxy's frontend mutual TLS (#2160, lbmtls.go).
		s.mtlsHandshake(proxy, cfg)
		if hello.ServerName != "" {
			for _, c := range certs {
				if c.Leaf != nil && c.Leaf.VerifyHostname(hello.ServerName) == nil {
					cfg.Certificates = []tls.Certificate{c}
					break
				}
			}
		}
		return cfg, nil
	}
}

// lbStart opens the restored rules' listeners and starts the data plane's
// loop: listeners kept in step with the rules, Google-managed certificates
// minted once a rule serves them, and the health checks.
func (s *Server) lbStart() {
	s.mu.Lock()
	for k, o := range s.res {
		if o["_lb"] == true {
			s.lbListenLocked(k)
		}
	}
	s.mu.Unlock()
	if s.lb.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.lb.cancel = cancel
	s.lb.wg.Add(1)
	go func() {
		defer s.lb.wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			s.lbReconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-s.lb.nudge:
			case <-t.C:
			}
		}
	}()
}

// lbStop closes every listener and stops the probes.
func (s *Server) lbStop() {
	if s.lb.cancel != nil {
		s.lb.cancel()
		s.lb.wg.Wait()
		s.lb.cancel = nil
	}
	s.lb.health.stopAll()
	p := s.lb
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, rl := range p.listeners {
		for _, l := range rl.ls {
			l.close()
		}
		delete(p.listeners, k)
	}
}

// lbReconcile keeps the listeners, managed certificates and health probes
// in step with the records.
func (s *Server) lbReconcile(ctx context.Context) {
	s.mu.Lock()
	changed := false
	live := map[string]bool{}
	for k, o := range s.res {
		if o["_lb"] != true {
			continue
		}
		live[k] = true
		before := str(o, "cloudburrowAddress")
		s.lbListenLocked(k)
		if str(o, "cloudburrowAddress") != before {
			changed = true
		}
		// A Google-managed certificate is provisioned once a rule's proxy
		// serves it.
		proxy := s.res[str(o, "target")]
		for _, c := range strs(proxy, "sslCertificates") {
			if co, ok := s.res[c]; ok && str(co, "type") == "MANAGED" && str(co, "_certPEM") == "" {
				if err := s.mintManagedLocked(co); err == nil {
					changed = true
				}
			}
		}
	}
	s.lb.mu.Lock()
	for k, rl := range s.lb.listeners {
		if !live[k] {
			for _, l := range rl.ls {
				l.close()
			}
			delete(s.lb.listeners, k)
		}
	}
	s.lb.mu.Unlock()
	probes := s.wantedProbesLocked()
	if changed {
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	s.lb.health.sync(ctx, s, probes)
}

// RuleAddresses are the host addresses a load-balancing forwarding rule's
// ports are listened on, by port, sorted by port.
func (s *Server) RuleAddresses(rel string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := strs(s.res[rel], "cloudburrowAddresses")
	sort.Strings(out)
	return out
}
