package compute

import (
	"strings"
	"testing"
	"time"
)

// TestRouterApplianceBGP (#2140): a router interface on a subnet with a
// router appliance peer is a BGP speaker at its address, configured to
// peer with the appliance and advertise the region's subnets with the
// peer's priority as the MED; once its session is reported up,
// getRouterStatus says so with the routes learned, and the network's VMs
// in the region route what the appliance advertised to it, save what is
// within a subnet range, and only while the appliance forwards IP.
func TestRouterApplianceBGP(t *testing.T) {
	s, _ := newTestServer(t, Options{VMs: fakeVMs{}})
	const project, region, zone = "bgp-p", "us-central1", "us-central1-a"
	must := func(method, path string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, project, path, nil, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return out
	}
	vm := func(name, zone string, forward bool) (obj, string) {
		must("POST", "zones/"+zone+"/instances", obj{"name": name, "machineType": "zones/" + zone + "/machineTypes/e2-small",
			"canIpForward":      forward,
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
		rel := "projects/" + project + "/zones/" + zone + "/instances/" + name
		// The VM starts after insert's operation: a route's next hop is
		// only a forwarder once its VM runs (forwarderLocked).
		waitFor(t, name+" RUNNING", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return str(s.res[rel], "status") == "RUNNING"
		})
		s.mu.Lock()
		o := s.res[rel]
		s.mu.Unlock()
		return o, str(sliceOf(o["networkInterfaces"])[0].(map[string]any), "networkIP")
	}
	appliance, aip := vm("appliance", zone, true)
	client, _ := vm("client", zone, false)
	far, _ := vm("far", "europe-west1-b", false)
	must("POST", "regions/"+region+"/routers", obj{"name": "r1", "network": "global/networks/default", "bgp": obj{"asn": 64514.0},
		"md5AuthenticationKeys": []any{obj{"name": "k", "key": "s3cret"}},
		"interfaces":            []any{obj{"name": "if0", "subnetwork": "regions/" + region + "/subnetworks/default", "privateIpAddress": "10.128.0.250"}},
		"bgpPeers": []any{obj{"name": "p0", "interfaceName": "if0", "peerAsn": 65001.0, "advertisedRoutePriority": 150.0, "md5AuthenticationKeyName": "k",
			"routerApplianceInstance": "zones/" + zone + "/instances/appliance"}}})

	sps := s.BGPSpeakers()
	if len(sps) != 1 {
		t.Fatalf("speakers: %+v", sps)
	}
	sp := sps[0]
	for _, want := range []string{"as = 64514", `router-id = "10.128.0.250"`, `neighbor-address = "` + aip + `"`, "peer-as = 65001",
		`local-address = "10.128.0.250"`, "keepalive-interval = 20", "hold-time = 60", `neighbor-info-list = ["` + aip + `/32"]`,
		`ip-prefix = "10.128.0.0/20"`, `masklength-range = "20..20"`, `set-med = "150"`, `auth-password = "s3cret"`, `default-export-policy = "reject-route"`} {
		if !strings.Contains(sp.Config, want) {
			t.Fatalf("config lacks %q:\n%s", want, sp.Config)
		}
	}
	if sp.IP != "10.128.0.250" || len(sp.Advertise) != 1 || sp.Advertise[0] != "10.128.0.0/20" || sp.Peers["p0"] != aip {
		t.Fatalf("speaker: %+v", sp)
	}
	// The interface's address is taken: no VM is given it.
	s.mu.Lock()
	used := s.usedInSubnetLocked("projects/" + project + "/regions/" + region + "/subnetworks/default")
	tok, ok := s.speakerByTokenLocked(sp.Token)
	inst := obj{"selfLink": bgpRel(sp.Key), "zone": region + "-a", "_speaker": sp.Key,
		"networkInterfaces": []any{obj{"network": sp.Network, "networkIP": sp.IP, "accessConfigs": []any{}}}}
	spConf, _ := s.vpcConfigLocked(inst, "10.244.0.77")
	s.mu.Unlock()
	if used["10.128.0.250"] == "" || !ok || tok.Key != sp.Key {
		t.Fatalf("interface address used %v, speaker by token %v", used, ok)
	}
	if !strings.Contains(spConf, "addr 10.128.0.250\n") || !strings.Contains(spConf, `tcp dport 179 accept comment "BGP"`) || strings.Contains(spConf, "routevia") {
		t.Fatalf("the speaker's configuration:\n%s", spConf)
	}
	conf := func(o obj) string {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, _ := s.vpcConfigLocked(o, "10.244.0.9")
		return c
	}
	// The speaker is a member the VMs reach, though no name of theirs.
	if c := conf(appliance); !strings.Contains(c, "peer 10.128.0.250 ") || strings.Contains(c, "cloud-router") {
		t.Fatalf("the appliance's configuration lacks the speaker:\n%s", c)
	}
	status := func() map[string]any {
		t.Helper()
		out := must("GET", "regions/"+region+"/routers/r1/getRouterStatus", nil)
		return out["result"].(obj)
	}
	ps := sliceOf(status()["bgpPeerStatus"])[0].(obj)
	if ps["status"] != "DOWN" || ps["state"] != "Idle" {
		t.Fatalf("before any session: %v", ps)
	}
	up := s.opts.Now().Add(-90 * time.Second)
	s.SetBGPState(sp.Key, BGPState{Neighbors: []BGPNeighbor{{Address: aip, State: 6, Up: up, Learned: []BGPRoute{
		{Prefix: "10.128.0.0/24", NextHop: aip, MED: 0, ASPath: []uint32{65001}},
		{Prefix: "192.168.77.0/24", NextHop: aip, MED: 50, ASPath: []uint32{65001}},
	}}}})
	res := status()
	ps = sliceOf(res["bgpPeerStatus"])[0].(obj)
	if ps["status"] != "UP" || ps["state"] != "Established" || ps["numLearnedRoutes"] != 2.0 || ps["uptimeSeconds"] != "90" ||
		ps["uptime"] != "1 minutes, 30 seconds" {
		t.Fatalf("established: %v", ps)
	}
	best := sliceOf(res["bestRoutes"])
	if len(best) != 1 || len(sliceOf(res["bestRoutesForRouter"])) != 1 {
		t.Fatalf("best routes: %v", res)
	}
	b := best[0].(obj)
	if b["destRange"] != "192.168.77.0/24" || b["nextHopIp"] != aip || b["priority"] != 50.0 || b["routeType"] != "BGP" {
		t.Fatalf("best route: %v", b)
	}
	via := "routevia 192.168.77.0/24 " + aip + "\n"
	if c := conf(client); !strings.Contains(c, via) || strings.Contains(c, "routevia 10.128.0.0/24") {
		t.Fatalf("the client's dynamic routes:\n%s", c)
	}
	if c := conf(appliance); strings.Contains(c, "routevia") {
		t.Fatalf("the appliance routes to itself:\n%s", c)
	}
	if c := conf(far); strings.Contains(c, "routevia") {
		t.Fatalf("another region's VM has a regional route:\n%s", c)
	}
	// GLOBAL dynamic routing: every region.
	nw := must("GET", "global/networks/default", nil)
	must("PATCH", "global/networks/default", obj{"routingConfig": obj{"routingMode": "GLOBAL"}, "fingerprint": nw["fingerprint"]})
	if c := conf(far); !strings.Contains(c, via) {
		t.Fatalf("GLOBAL routing, the far VM:\n%s", c)
	}
	// An appliance that does not forward IP is no next hop.
	s.mu.Lock()
	appliance["canIpForward"] = false
	s.mu.Unlock()
	if c := conf(client); strings.Contains(c, "routevia") {
		t.Fatalf("a next hop without canIpForward:\n%s", c)
	}
	// A session whose MD5 key the kernel would not take.
	s.SetBGPState(sp.Key, BGPState{Neighbors: []BGPNeighbor{{Address: aip, State: 3, MD5Failed: true}}})
	if ps := sliceOf(status()["bgpPeerStatus"])[0].(obj); ps["status"] != "DOWN" || ps["state"] != "Active" || ps["statusReason"] != "MD5_AUTH_INTERNAL_PROBLEM" {
		t.Fatalf("an MD5 failure: %v", ps)
	}
	// A session gone quiet is DOWN again.
	s.SetBGPState(sp.Key, BGPState{Seen: s.opts.Now().Add(-time.Hour), Neighbors: []BGPNeighbor{{Address: aip, State: 6, Up: up}}})
	if ps := sliceOf(status()["bgpPeerStatus"])[0].(obj); ps["status"] != "DOWN" {
		t.Fatalf("a stale report: %v", ps)
	}
}
