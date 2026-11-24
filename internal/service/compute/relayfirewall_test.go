package compute

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/vmrelay"
)

// relayFixture is a started Server with one running VM (tags web,
// service account web@), a UDP and a TCP passthrough load balancer whose
// backends are local echo servers, and the firewall log it writes.
type relayFixture struct {
	s          *Server
	inst       obj
	src        netip.AddrPort
	udpVIP     netip.AddrPort
	mu         sync.Mutex
	logs       []FirewallLogEntry
	configText string
}

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("echo:"), buf[:n]...), a)
		}
	}()
	f := &relayFixture{}
	lb := &udpLB{fakeLB: fakeLB{vms: map[string]string{}}, echo: pc.LocalAddr().String()}
	s, err := New(Options{VMs: fakeVMs{}, LB: lb, FirewallLog: func(e FirewallLogEntry) {
		f.mu.Lock()
		f.logs = append(f.logs, e)
		f.mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	f.s = s
	z := "zones/us-central1-a/"
	vm := func(name string, tags []any, sa string) {
		lbCall(t, s, "POST", z+"instances", obj{"name": name, "machineType": z + "machineTypes/e2-small",
			"tags":              obj{"items": tags},
			"serviceAccounts":   []any{obj{"email": sa, "scopes": []any{"https://www.googleapis.com/auth/cloud-platform"}}},
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"network": "global/networks/default", "accessConfigs": []any{obj{"name": "External NAT"}}}}})
	}
	vm("client", []any{"web"}, "web@p.iam.gserviceaccount.com")
	vm("backend", nil, "batch@p.iam.gserviceaccount.com")
	lbCall(t, s, "POST", "regions/us-central1/targetPools", obj{"name": "pool", "instances": []any{z + "instances/backend"}})
	lbCall(t, s, "POST", "regions/us-central1/forwardingRules", obj{"name": "dns", "IPProtocol": "UDP", "portRange": "53",
		"target": "regions/us-central1/targetPools/pool"})
	s.mu.Lock()
	for k, o := range s.res {
		if strings.HasSuffix(k, "/instances/client") {
			f.inst = o
		}
	}
	rule := s.res["projects/p/regions/us-central1/forwardingRules/dns"]
	if rule != nil {
		// The unit server gives an external rule this machine's loopback
		// address; a VM dials a VPC address.
		rule["IPAddress"] = "10.128.0.50"
	}
	s.mu.Unlock()
	if f.inst == nil || rule == nil {
		t.Fatalf("the instance %v or the rule %v", f.inst, rule)
	}
	vip, err := netip.ParseAddr(str(rule, "IPAddress"))
	if err != nil {
		t.Fatalf("the rule's address: %v", rule)
	}
	f.udpVIP = netip.AddrPortFrom(vip, 53)
	self, _ := vmNetOf(str(f.inst, "selfLink"), f.inst)
	f.src = netip.AddrPortFrom(netip.MustParseAddr(self.ip), 0)
	s.mu.Lock()
	f.configText, _ = s.vpcConfigLocked(f.inst, "10.244.0.9")
	s.mu.Unlock()
	return f
}

func (f *relayFixture) token() string {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return str(f.inst, "_guestToken")
}

// udpEcho sends ping over a relayed UDP flow and returns the reply, ""
// when the relay closes the flow.
func (f *relayFixture) udpEcho(t *testing.T, ping string) string {
	t.Helper()
	c, err := net.Dial("tcp", f.s.RelayAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write(vmrelay.Encode(vmrelay.Header{Src: f.src, Dst: f.udpVIP, Token: f.token(), UDP: true}))
	if err := vmrelay.WriteFrame(c, []byte(ping)); err != nil {
		return ""
	}
	b, err := vmrelay.ReadFrame(c)
	if err != nil {
		return ""
	}
	return string(b)
}

func (f *relayFixture) logged() []FirewallLogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FirewallLogEntry(nil), f.logs...)
}

// TestRelayAppliesEgressFirewallRules (#2180): a VM's relayed UDP flow to
// a passthrough load balancer carries its datagrams; an egress deny rule
// targeting the VM's service account refuses it at the relay and writes
// a DENIED firewall log record; a higher-priority allow targeting its
// network tag wins; a rule on another tag or another protocol does not
// apply; and a denied 5-tuple is logged once per 5 seconds.
func TestRelayAppliesEgressFirewallRules(t *testing.T) {
	f := newRelayFixture(t)
	s := f.s
	for _, want := range []string{"relayudp 47000 " + f.udpVIP.Addr().String() + " 53 ",
		"ip daddr " + f.udpVIP.Addr().String() + " udp dport 53 redirect to :47000"} {
		if !strings.Contains(f.configText, want) {
			t.Fatalf("%q missing:\n%s", want, f.configText)
		}
	}
	if got := f.udpEcho(t, "a"); got != "echo:a" {
		t.Fatalf("the relayed flow with no rule: %q", got)
	}
	lbCall(t, s, "POST", "global/firewalls", obj{"name": "deny-dns", "direction": "EGRESS", "priority": 1000,
		"targetServiceAccounts": []any{"web@p.iam.gserviceaccount.com"}, "destinationRanges": []any{f.udpVIP.Addr().String() + "/32"},
		"denied": []any{obj{"IPProtocol": "udp", "ports": []any{"53"}}}, "logConfig": obj{"enable": true}})
	fw := lbCall(t, s, "GET", "global/firewalls/deny-dns", nil)
	if lc := objOf(fw["logConfig"]); lc["metadata"] != "INCLUDE_ALL_METADATA" {
		t.Errorf("logConfig: %v", fw["logConfig"])
	}
	if got := f.udpEcho(t, "b"); got != "" {
		t.Fatalf("a flow the egress deny rule names was carried: %q", got)
	}
	logs := f.logged()
	if len(logs) != 1 {
		t.Fatalf("%d firewall log records, want 1: %v", len(logs), logs)
	}
	p := logs[0].Payload
	conn, rd := p["connection"].(map[string]any), p["rule_details"].(map[string]any)
	if p["disposition"] != "DENIED" || conn["dest_ip"] != f.udpVIP.Addr().String() || conn["dest_port"] != 53 || conn["protocol"] != 17 ||
		conn["src_ip"] != f.src.Addr().String() || rd["reference"] != "network:default/firewall:deny-dns" || rd["action"] != "DENY" ||
		rd["direction"] != "EGRESS" || rd["priority"] != 1000 {
		t.Errorf("the record: %v", p)
	}
	if inst, _ := p["instance"].(map[string]any); inst["vm_name"] != "client" || inst["zone"] != "us-central1-a" || inst["region"] != "us-central1" {
		t.Errorf("the record's instance: %v", p["instance"])
	}
	if logs[0].Project != "p" || logs[0].Labels["subnetwork_name"] != "default" || logs[0].Labels["location"] != "us-central1" {
		t.Errorf("the record's project %q and resource labels %v", logs[0].Project, logs[0].Labels)
	}
	// A rule that applies to another tag, or to TCP, decides nothing here.
	lbCall(t, s, "POST", "global/firewalls", obj{"name": "allow-other", "direction": "EGRESS", "priority": 10,
		"targetTags": []any{"other"}, "allowed": []any{obj{"IPProtocol": "all"}}})
	lbCall(t, s, "POST", "global/firewalls", obj{"name": "allow-tcp", "direction": "EGRESS", "priority": 10,
		"targetTags": []any{"web"}, "allowed": []any{obj{"IPProtocol": "tcp"}}})
	if got := f.udpEcho(t, "c"); got != "" {
		t.Fatalf("a rule for another tag or protocol let the flow through: %q", got)
	}
	// The higher-priority allow on the VM's tag wins.
	lbCall(t, s, "POST", "global/firewalls", obj{"name": "allow-dns", "direction": "EGRESS", "priority": 900,
		"targetTags": []any{"web"}, "allowed": []any{obj{"IPProtocol": "udp", "ports": []any{"50-60"}}},
		"logConfig": obj{"enable": true, "metadata": "EXCLUDE_ALL_METADATA"}})
	if got := f.udpEcho(t, "d"); got != "echo:d" {
		t.Fatalf("the higher-priority allow: %q", got)
	}
	logs = f.logged()
	last := logs[len(logs)-1].Payload
	if last["disposition"] != "ALLOWED" || last["rule_details"].(map[string]any)["reference"] != "network:default/firewall:allow-dns" {
		t.Errorf("the allowed record: %v", last)
	}
	if _, ok := last["instance"]; ok {
		t.Errorf("EXCLUDE_ALL_METADATA kept the instance: %v", last)
	}
	// A deny at the same priority wins over the allow.
	lbCall(t, s, "POST", "global/firewalls", obj{"name": "deny-dns-900", "direction": "EGRESS", "priority": 900,
		"denied": []any{obj{"IPProtocol": "17"}}})
	if got := f.udpEcho(t, "e"); got != "" {
		t.Fatalf("a deny of the same priority as the allow lost: %q", got)
	}
	// A disabled rule does not apply.
	lbCall(t, s, "PATCH", "global/firewalls/deny-dns-900", obj{"disabled": true})
	if got := f.udpEcho(t, "f"); got != "echo:f" {
		t.Fatalf("a disabled deny applied: %q", got)
	}
	// metadata is checked.
	if _, err := vmCall(t, s, "POST", "global/firewalls", jsonBody(obj{"name": "bad", "direction": "EGRESS",
		"allowed": []any{obj{"IPProtocol": "tcp"}}, "logConfig": obj{"enable": true, "metadata": "SOME"}})); err == nil {
		t.Error("an unknown logConfig.metadata was accepted")
	}
}

// TestFirewallDeniedLogsOncePerFiveSeconds checks the denied record's
// rate: one per 5-tuple every 5 seconds.
func TestFirewallDeniedLogsOncePerFiveSeconds(t *testing.T) {
	now := time.Unix(2e9, 0)
	s := &Server{opts: Options{Now: func() time.Time { return now }, FirewallLog: func(FirewallLogEntry) {}}, res: map[string]obj{}}
	nw := "projects/p/global/networks/n"
	s.res["projects/p/global/firewalls/d"] = obj{"name": "d", "network": nw, "direction": "EGRESS", "priority": 5.0,
		"destinationRanges": []any{"0.0.0.0/0"}, "denied": []any{map[string]any{"IPProtocol": "all"}}, "logConfig": map[string]any{"enable": true}}
	inst := obj{"name": "a", "networkInterfaces": []any{map[string]any{"network": nw, "networkIP": "10.0.0.2"}}}
	self := vmNet{name: "a", zone: "us-central1-a", project: "p", network: nw, ip: "10.0.0.2"}
	src, dst := netip.MustParseAddrPort("10.0.0.2:4000"), netip.MustParseAddrPort("10.0.0.9:80")
	count := func() int {
		if d := s.egressFirewallLocked(inst, self, src, dst, "tcp"); d.allow {
			t.Fatal("allowed")
		} else if d.log != nil {
			return 1
		}
		return 0
	}
	n := count() + count()
	now = now.Add(4 * time.Second)
	n += count()
	if n != 1 {
		t.Fatalf("%d records in 4 seconds, want 1", n)
	}
	now = now.Add(2 * time.Second)
	if count() != 1 {
		t.Fatal("no record after 5 seconds")
	}
	if d := s.egressFirewallLocked(inst, self, netip.MustParseAddrPort("10.0.0.2:4001"), dst, "tcp"); d.log == nil {
		t.Fatal("another 5-tuple was not logged")
	}
}

// TestGuestUDPRelayScript runs the guest's udprelay (perl) against the
// relay's framing: a client's datagrams reach the relay after the header,
// framed, and the framed replies come back to the client as datagrams.
func TestGuestUDPRelayScript(t *testing.T) {
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("no perl on this machine")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hdr := vmrelay.Encode(vmrelay.Header{Src: netip.MustParseAddrPort("10.0.0.2:0"), Dst: netip.MustParseAddrPort("10.0.0.30:53"), Token: "tok", UDP: true})
	dir := t.TempDir()
	hf := filepath.Join(dir, "hdr")
	if err := os.WriteFile(hf, hdr, 0o600); err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lp := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cmd := exec.Command(perl, "guest/root/usr/lib/cloudburrow/udprelay", strconv.Itoa(lp), "127.0.0.1", port, hf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	got := make(chan vmrelay.Header, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		h, err := vmrelay.Read(br)
		if err != nil {
			return
		}
		got <- h
		for {
			b, err := vmrelay.ReadFrame(br)
			if err != nil {
				return
			}
			_ = vmrelay.WriteFrame(c, append([]byte("re:"), b...))
		}
	}()
	c, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(lp))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 100)
	waitFor(t, "a reply through udprelay", func() bool {
		_, _ = c.Write([]byte("x"))
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := c.Read(buf)
		return err == nil && string(buf[:n]) == "re:x"
	})
	if h := <-got; !h.UDP || h.Token != "tok" || h.Dst.String() != "10.0.0.30:53" {
		t.Errorf("the header: %+v", h)
	}
	// A second datagram uses the same flow.
	_, _ = c.Write([]byte("yy"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "re:yy" {
		t.Fatalf("the second datagram: %q %v", buf[:n], err)
	}
}

// TestRelayCarriesPrivateServiceConnect (#2180): a VM's connection to a
// Private Service Connect endpoint of its network reaches the load
// balancer its service attachment publishes, as from the endpoint's
// address in the attachment's NAT subnet; an egress deny to the
// endpoint's address refuses it; an endpoint not ACCEPTED is not relayed.
func TestRelayCarriesPrivateServiceConnect(t *testing.T) {
	now := time.Unix(1e9, 0)
	nw := "projects/c/global/networks/n"
	s := &Server{opts: Options{Now: func() time.Time { return now }}, res: map[string]obj{}, lb: newLBPlane()}
	s.res[nw] = obj{"id": "1"}
	rel := "projects/c/zones/us-central1-a/instances/a"
	inst := obj{"name": "a", "selfLink": rel, "zone": "projects/c/zones/us-central1-a", "status": "RUNNING", "_guestToken": "tok",
		"networkInterfaces": []any{map[string]any{"network": nw, "networkIP": "10.0.0.2"}}}
	s.res[rel] = inst
	producer := "projects/prod/regions/us-central1/forwardingRules/ilb"
	s.res[producer] = obj{"_lb": true, "selfLink": producer, "IPAddress": "10.50.0.5", "portRange": "80-80", "loadBalancingScheme": "INTERNAL_MANAGED",
		"network": "projects/prod/global/networks/p", "target": "projects/prod/regions/us-central1/targetHttpProxies/x"}
	nat := "projects/prod/regions/us-central1/subnetworks/nat"
	s.res[nat] = obj{"ipCidrRange": "192.168.9.0/24", "purpose": "PRIVATE_SERVICE_CONNECT"}
	att := "projects/prod/regions/us-central1/serviceAttachments/svc"
	s.res[att] = obj{"targetService": producer, "natSubnets": []any{nat}}
	ep := "projects/c/regions/us-central1/forwardingRules/ep"
	s.res[ep] = obj{"selfLink": ep, "IPAddress": "10.0.0.77", "network": nw, "target": att, "pscConnectionStatus": "ACCEPTED", "pscConnectionId": "42"}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inj := vmrelay.NewListener(base)
	defer inj.Close()
	s.lb.listeners[producer] = &lbRuleListeners{kind: "http", ls: []*lbListener{{rulePort: 80, addr: base.Addr().String(), ln: inj, inj: inj}}}
	r, err := vmrelay.Listen("127.0.0.1:0", s.relayResolve)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s.relay = r
	out, _ := s.vpcConfigLocked(inst, "10.244.0.7")
	if !strings.Contains(out, "relay 47000 10.0.0.77 80 ") || strings.Contains(out, "10.50.0.5") {
		t.Fatalf("the endpoint's relay:\n%s", out)
	}
	go func() {
		for {
			c, err := inj.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, c.RemoteAddr().String())
			_ = c.Close()
		}
	}()
	send := func() string {
		c, err := net.Dial("tcp", r.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = c.Write(vmrelay.Encode(vmrelay.Header{Src: netip.MustParseAddrPort("10.0.0.2:0"), Dst: netip.MustParseAddrPort("10.0.0.77:80"), Token: "tok"}))
		b, _ := io.ReadAll(c)
		return string(b)
	}
	got := send()
	host, _, _ := net.SplitHostPort(got)
	if a, err := netip.ParseAddr(host); err != nil || !netip.MustParsePrefix("192.168.9.0/24").Contains(a) {
		t.Fatalf("the producer saw %q, want an address of the NAT subnet", got)
	}
	// The relay reads s.res under s.mu (relayResolve), so the test writes
	// under it too.
	s.mu.Lock()
	s.res["projects/c/global/firewalls/no-psc"] = obj{"name": "no-psc", "network": nw, "direction": "EGRESS", "priority": 1000.0,
		"destinationRanges": []any{"10.0.0.77"}, "denied": []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"80"}}}}
	s.mu.Unlock()
	if got := send(); got != "" {
		t.Fatalf("an egress deny to the endpoint was relayed: %q", got)
	}
	s.mu.Lock()
	delete(s.res, "projects/c/global/firewalls/no-psc")
	s.res[ep]["pscConnectionStatus"] = "PENDING"
	s.mu.Unlock()
	if got := send(); got != "" {
		t.Fatalf("a PENDING endpoint was relayed: %q", got)
	}
}
