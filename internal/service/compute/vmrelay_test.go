package compute

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/vmrelay"
)

type fakeGateways struct {
	network string
	at      netip.AddrPort
	got     chan net.Conn
}

func (g fakeGateways) RelayAddresses(network string) []netip.AddrPort {
	if network == g.network {
		return []netip.AddrPort{g.at}
	}
	return nil
}

func (g fakeGateways) RelayInject(network string, dst netip.AddrPort, c net.Conn) bool {
	if network != g.network || dst != g.at {
		return false
	}
	g.got <- c
	return true
}

// TestVMRelayCarriesTheInstanceIdentity builds a guest's relay
// configuration (an internal load balancer of its network, one of another
// network, a gateway) and relays connections through the relay's real
// listener: one with the instance's token and IP reaches the rule's
// listener as from the instance, and a wrong token, a wrong source or an
// unreachable rule is refused.
func TestVMRelayCarriesTheInstanceIdentity(t *testing.T) {
	now := time.Unix(1e9, 0)
	nw, other := "projects/p/global/networks/n", "projects/p/global/networks/other"
	gw := fakeGateways{network: nw, at: netip.MustParseAddrPort("10.0.0.40:443"), got: make(chan net.Conn, 1)}
	s := &Server{opts: Options{Now: func() time.Time { return now }, VMs: nil, Gateways: gw}, res: map[string]obj{}, lb: newLBPlane()}
	s.res[nw] = obj{"id": "123"}
	s.res[other] = obj{"id": "456"}
	s.res["projects/p/regions/r/subnetworks/s"] = obj{"network": nw, "ipCidrRange": "10.0.0.0/24"}
	rel := "projects/p/zones/us-central1-a/instances/a"
	inst := obj{"name": "a", "selfLink": rel, "zone": "projects/p/zones/us-central1-a", "status": "RUNNING", "_guestToken": "tok-a",
		"networkInterfaces": []any{map[string]any{"network": nw, "networkIP": "10.0.0.2"}},
		"serviceAccounts":   []any{map[string]any{"email": "web@p.iam.gserviceaccount.com"}}}
	s.res[rel] = inst
	ilb := "projects/p/regions/r/forwardingRules/ilb"
	s.res[ilb] = obj{"_lb": true, "IPAddress": "10.0.0.30", "portRange": "80-80", "loadBalancingScheme": "INTERNAL_MANAGED",
		"network": nw, "target": "projects/p/regions/r/targetHttpProxies/x"}
	s.res["projects/p/regions/r/forwardingRules/far"] = obj{"_lb": true, "IPAddress": "10.9.0.30", "portRange": "80-80",
		"loadBalancingScheme": "INTERNAL_MANAGED", "network": other, "target": "projects/p/regions/r/targetHttpProxies/x"}
	// The rule's listener, as lbListenLocked opens it.
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inj := vmrelay.NewListener(base)
	defer inj.Close()
	s.lb.listeners[ilb] = &lbRuleListeners{kind: "http", ls: []*lbListener{{rulePort: 80, addr: base.Addr().String(), ln: inj, inj: inj}}}
	r, err := vmrelay.Listen("127.0.0.1:0", s.relayResolve)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s.relay = r

	out, _ := s.vpcConfigLocked(inst, "10.244.0.7")
	for _, want := range []string{"relayport " + strconv.Itoa(r.Port()) + "\n", "relay 47000 10.0.0.30 80 ", "relay 47001 10.0.0.40 443 ",
		"chain relay {", "ip daddr 10.0.0.30 tcp dport 80 redirect to :47000", "ip daddr 10.0.0.40 tcp dport 443 redirect to :47001"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing:\n%s", want, out)
		}
	}
	if strings.Contains(out, "10.9.0.30") {
		t.Errorf("another network's internal load balancer is relayed:\n%s", out)
	}
	// The header the guest sends is the one its configuration gives.
	var hdr []byte
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 5 && f[0] == "relay" && f[2] == "10.0.0.30" {
			hdr, _ = base64.StdEncoding.DecodeString(f[4])
		}
	}
	h, err := vmrelay.Read(bufio.NewReader(strings.NewReader(string(hdr))))
	if err != nil || h.Token != "tok-a" || h.Src.Addr().String() != "10.0.0.2" || h.Dst.String() != "10.0.0.30:80" {
		t.Fatalf("the configured header: %+v %v", h, err)
	}

	go func() {
		for {
			c, err := inj.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, c.RemoteAddr().String()+" "+c.LocalAddr().String())
			}()
		}
	}()
	send := func(h vmrelay.Header) string {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(r.Port()))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(vmrelay.Encode(h))
		_, _ = io.WriteString(c, "hi\n")
		b, _ := io.ReadAll(c)
		return string(b)
	}
	src := netip.MustParseAddrPort("10.0.0.2:0")
	if got := send(vmrelay.Header{Src: src, Dst: netip.MustParseAddrPort("10.0.0.30:80"), Token: "tok-a"}); !strings.HasPrefix(got, "10.0.0.2:") ||
		!strings.HasSuffix(got, " 10.0.0.30:80") {
		t.Errorf("relayed: %q", got)
	}
	for name, h := range map[string]vmrelay.Header{
		"a wrong token":       {Src: src, Dst: netip.MustParseAddrPort("10.0.0.30:80"), Token: "tok-b"},
		"a wrong source":      {Src: netip.MustParseAddrPort("10.0.0.3:0"), Dst: netip.MustParseAddrPort("10.0.0.30:80"), Token: "tok-a"},
		"another network's":   {Src: src, Dst: netip.MustParseAddrPort("10.9.0.30:80"), Token: "tok-a"},
		"a port not listened": {Src: src, Dst: netip.MustParseAddrPort("10.0.0.30:81"), Token: "tok-a"},
	} {
		if got := send(h); got != "" {
			t.Errorf("%s was relayed: %q", name, got)
		}
	}
	// The gateway's address goes to Secure Web Proxy.
	go send(vmrelay.Header{Src: src, Dst: gw.at, Token: "tok-a"})
	select {
	case c := <-gw.got:
		if c.RemoteAddr().(*net.TCPAddr).IP.String() != "10.0.0.2" {
			t.Errorf("the gateway's connection is from %v", c.RemoteAddr())
		}
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Error("the gateway got no connection")
	}
	// A stopped instance's token no longer relays.
	inst["status"] = "TERMINATED"
	if got := send(vmrelay.Header{Src: src, Dst: netip.MustParseAddrPort("10.0.0.30:80"), Token: "tok-a"}); got != "" {
		t.Errorf("a stopped instance's connection was relayed: %q", got)
	}
}
