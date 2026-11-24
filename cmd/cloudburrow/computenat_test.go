package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestNATFlowsFromConntrack (#2140): a NAT gateway pod's conntrack
// listing gives the connections it translated (its VMs' to its NAT IPs
// only), its dropped set those it dropped; each becomes a nat_flows entry
// in Cloud NAT's format, kept or not by the logConfig filter; the nodes
// route each NAT IP to the pod.
func TestNATFlowsFromConntrack(t *testing.T) {
	listing := `ipv4     2 tcp      6 117 TIME_WAIT src=10.128.0.2 dst=198.51.100.2 sport=40000 dport=8080 src=198.51.100.2 dst=203.0.113.1 sport=8080 dport=1030 [ASSURED] mark=0 zone=0 use=2
ipv4     2 udp      17 25 src=10.128.0.2 dst=198.51.100.2 sport=5353 dport=53 src=198.51.100.2 dst=203.0.113.1 sport=53 dport=1031 mark=0 zone=0 use=2
ipv4     2 tcp      6 100 ESTABLISHED src=10.244.0.5 dst=10.96.0.1 sport=1 dport=443 src=10.96.0.1 dst=10.244.0.5 sport=443 dport=1 mark=0 zone=0 use=2
ipv4     2 icmp     1 20 src=10.128.0.2 dst=198.51.100.2 type=8 code=0 id=1 src=198.51.100.2 dst=203.0.113.1 type=0 code=0 id=1 mark=0 zone=0 use=2`
	conns := natConntrack(listing, map[string]bool{"10.128.0.2": true}, map[string]bool{"203.0.113.1": true})
	if len(conns) != 2 {
		t.Fatalf("translated connections: %+v", conns)
	}
	if c := conns[0]; c.proto != 6 || c.srcIP != "10.128.0.2" || c.srcPort != 40000 || c.natIP != "203.0.113.1" || c.natPort != 1030 || c.destIP != "198.51.100.2" || c.dstPrt != 8080 {
		t.Fatalf("tcp: %+v", c)
	}
	dropped := natDropped(`{"nftables": [{"metainfo": {"version": "1.0.6"}}, {"set": {"family": "inet", "name": "dropped", "table": "cloudburrow", "elem": [{"elem": {"val": {"concat": ["10.128.0.3", 40001, "198.51.100.2", 8080, 6]}, "expires": 119}}, {"concat": ["10.128.0.3", 5000, "198.51.100.2", 53, "udp"]}]}}]}`)
	if len(dropped) != 2 || !dropped[0].dropped || dropped[0].srcPort != 40001 || dropped[1].proto != 17 {
		t.Fatalf("dropped: %+v", dropped)
	}
	g := compute.NATGateway{Key: "projects/p/regions/us-central1/routers/r/nats/nat", Project: "p", Region: "us-central1", Router: "r", RouterID: "123",
		Name: "nat", Network: "projects/p/global/networks/default", NatIPs: []string{"203.0.113.1"}, Log: true, LogFilter: "ALL", Pod: "10.244.0.20"}
	for filter, want := range map[string][2]bool{"ALL": {true, true}, "TRANSLATIONS_ONLY": {true, false}, "ERRORS_ONLY": {false, true}} {
		g.LogFilter = filter
		if natLogs(g, conns[0]) != want[0] || natLogs(g, dropped[0]) != want[1] {
			t.Fatalf("filter %s", filter)
		}
	}
	g.Log = false
	if natLogs(g, conns[0]) {
		t.Fatal("logged with logging off")
	}
	vm := compute.NATVM{Name: "a", Zone: "us-central1-a", Subnetwork: "projects/p/regions/us-central1/subnetworks/default", IP: "10.128.0.2", NatIP: "203.0.113.1", PortLo: 1024, PortHi: 1087}
	e := natFlowEntry(g, vm, conns[0], time.Now())
	if e.GetLogName() != "projects/p/logs/compute.googleapis.com%2Fnat_flows" || e.GetResource().GetType() != "nat_gateway" ||
		e.GetResource().GetLabels()["router_id"] != "123" || e.GetResource().GetLabels()["gateway_name"] != "nat" {
		t.Fatalf("entry: %v", e)
	}
	p := e.GetJsonPayload().AsMap()
	conn := p["connection"].(map[string]any)
	if p["allocation_status"] != "OK" || conn["nat_ip"] != "203.0.113.1" || conn["nat_port"] != 1030.0 || conn["protocol"] != 6.0 ||
		p["endpoint"].(map[string]any)["vm_name"] != "a" || p["vpc"].(map[string]any)["subnetwork_name"] != "default" ||
		p["gateway_identifiers"].(map[string]any)["router_name"] != "r" {
		t.Fatalf("payload: %v", p)
	}
	if p := natFlowEntry(g, vm, dropped[0], time.Now()).GetJsonPayload().AsMap(); p["allocation_status"] != "DROPPED" {
		t.Fatalf("dropped payload: %v", p)
	}
	script := natRouteScript([]compute.NATGateway{g, {NatIPs: []string{"203.0.113.9"}}})
	if !strings.Contains(script, "ip route flush proto 177") || !strings.Contains(script, "ip route replace 203.0.113.1/32 via 10.244.0.20 dev \"$d\" onlink proto 177") ||
		strings.Contains(script, "203.0.113.9") {
		t.Fatalf("routes:\n%s", script)
	}
}
