package compute

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestNetworkPeeringThroughTheGoClient (#2140): a peering is INACTIVE until
// the peer peers back, then both are ACTIVE; overlapping subnets, a
// duplicate name and a network peered with itself are refused;
// updatePeering changes the custom route exchange; removing one side makes
// the other INACTIVE.
func TestNetworkPeeringThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	_, opts := newTestServer(t, Options{})
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	const project = "peer-p"
	for i, n := range []string{"a", "b", "c"} {
		op, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{
			Name: proto.String(n), AutoCreateSubnetworks: proto.Bool(false)}})
		if err != nil {
			t.Fatal(err)
		}
		_ = op.Wait(ctx)
		cidr := "10." + strconv.Itoa(i) + ".0.0/24"
		if n == "c" {
			cidr = "10.0.0.0/25" // overlaps a's
		}
		if _, err := subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: project, Region: "us-central1", SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(n + "-sub"), Network: proto.String("global/networks/" + n), IpCidrRange: proto.String(cidr)}}); err != nil {
			t.Fatal(err)
		}
	}
	peer := func(from, to, name string) error {
		_, err := nets.AddPeering(ctx, &computepb.AddPeeringNetworkRequest{Project: project, Network: from,
			NetworksAddPeeringRequestResource: &computepb.NetworksAddPeeringRequest{NetworkPeering: &computepb.NetworkPeering{
				Name: proto.String(name), Network: proto.String("projects/" + project + "/global/networks/" + to),
				ExchangeSubnetRoutes: proto.Bool(true), ImportCustomRoutes: proto.Bool(true)}}})
		return err
	}
	state := func(n string) string {
		t.Helper()
		got, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: project, Network: n})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.GetPeerings()) == 0 {
			return "none"
		}
		return got.GetPeerings()[0].GetState()
	}
	if err := peer("a", "b", "a-to-b"); err != nil {
		t.Fatal(err)
	}
	if got := state("a"); got != "INACTIVE" {
		t.Errorf("a's peering before b peers back = %s", got)
	}
	if err := peer("b", "a", "b-to-a"); err != nil {
		t.Fatal(err)
	}
	if a, b := state("a"), state("b"); a != "ACTIVE" || b != "ACTIVE" {
		t.Errorf("after both peer: a %s, b %s", a, b)
	}
	got, _ := nets.Get(ctx, &computepb.GetNetworkRequest{Project: project, Network: "a"})
	pp := got.GetPeerings()[0]
	if !strings.HasSuffix(pp.GetNetwork(), "/projects/peer-p/global/networks/b") || !pp.GetImportCustomRoutes() || pp.GetExportCustomRoutes() ||
		!pp.GetExchangeSubnetRoutes() || pp.GetPeerMtu() != 1460 || !strings.Contains(pp.GetStateDetails(), "Connected") {
		t.Errorf("a's peering = %+v", pp)
	}
	for name, err := range map[string]error{
		"a duplicate name":           peer("a", "b", "a-to-b"),
		"overlapping subnets":        peer("a", "c", "a-to-c"),
		"a network peered to itself": peer("a", "a", "self"),
		"a missing peer network":     peer("a", "nowhere", "gone"),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := nets.UpdatePeering(ctx, &computepb.UpdatePeeringNetworkRequest{Project: project, Network: "a",
		NetworksUpdatePeeringRequestResource: &computepb.NetworksUpdatePeeringRequest{NetworkPeering: &computepb.NetworkPeering{
			Name: proto.String("a-to-b"), ExportCustomRoutes: proto.Bool(true), ImportCustomRoutes: proto.Bool(false)}}}); err != nil {
		t.Fatal(err)
	}
	got, _ = nets.Get(ctx, &computepb.GetNetworkRequest{Project: project, Network: "a"})
	if pp := got.GetPeerings()[0]; !pp.GetExportCustomRoutes() || pp.GetImportCustomRoutes() {
		t.Errorf("after updatePeering: %+v", pp)
	}
	if _, err := nets.RemovePeering(ctx, &computepb.RemovePeeringNetworkRequest{Project: project, Network: "b",
		NetworksRemovePeeringRequestResource: &computepb.NetworksRemovePeeringRequest{Name: proto.String("b-to-a")}}); err != nil {
		t.Fatal(err)
	}
	if a, b := state("a"), state("b"); a != "INACTIVE" || b != "none" {
		t.Errorf("after b removes its side: a %s, b %s", a, b)
	}
}

// TestPeeredNetworksShareASegment (#2140): with an ACTIVE peering, a VM's
// configuration reaches the directly peered network's VMs and subnet
// routes on one VXLAN segment, while its DNS names and its firewall's
// source tags stay its own network's; a network peered only with the
// peer stays unreachable (peering is not transitive).
func TestPeeredNetworksShareASegment(t *testing.T) {
	now := time.Unix(1e9, 0)
	s := &Server{opts: Options{Now: func() time.Time { return now }}, res: map[string]obj{}}
	na, nb, nc := "projects/p/global/networks/a", "projects/p/global/networks/b", "projects/p/global/networks/c"
	peering := func(to string) map[string]any {
		return map[string]any{"name": "to-" + lastSeg(to), "network": to, "state": "ACTIVE"}
	}
	s.res[na] = obj{"id": "111", "peerings": []any{peering(nb)}}
	s.res[nb] = obj{"id": "222", "peerings": []any{peering(na), peering(nc)}}
	s.res[nc] = obj{"id": "333", "peerings": []any{peering(nb)}}
	s.res["projects/p/regions/r/subnetworks/a"] = obj{"network": na, "ipCidrRange": "10.0.0.0/24"}
	s.res["projects/p/regions/r/subnetworks/b"] = obj{"network": nb, "ipCidrRange": "10.1.0.0/24"}
	s.res["projects/p/regions/r/subnetworks/c"] = obj{"network": nc, "ipCidrRange": "10.2.0.0/24"}
	inst := func(name, network, ip string) obj {
		rel := "projects/p/zones/us-central1-a/instances/" + name
		o := obj{"name": name, "selfLink": rel, "zone": "projects/p/zones/us-central1-a", "status": "RUNNING",
			"networkInterfaces": []any{map[string]any{"network": network, "networkIP": ip}}}
		s.res[rel] = o
		return o
	}
	a, b, c := inst("va", na, "10.0.0.2"), inst("vb", nb, "10.1.0.2"), inst("vc", nc, "10.2.0.2")
	s.vpcConfigLocked(b, "10.244.0.8")
	s.vpcConfigLocked(c, "10.244.0.9")
	out, _ := s.vpcConfigLocked(a, "10.244.0.7")
	vni := "vni " + strconv.FormatUint(uint64(vniOf("111")), 10) + "\n"
	for _, want := range []string{vni, "route 10.0.0.0/24", "route 10.1.0.0/24", "peer 10.1.0.2 42:01:0a:01:00:02 10.244.0.8"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from a's configuration:\n%s", want, out)
		}
	}
	for _, not := range []string{"10.2.0.", "vb.us-central1-a"} {
		if strings.Contains(out, not) {
			t.Errorf("a's configuration has %q:\n%s", not, out)
		}
	}
	// (Its ruleset names 10.0.0.0/8 among the private ranges a VM without
	// an external IP or Cloud NAT keeps, #2140: a's route and address are
	// what must be missing.)
	if out, _ := s.vpcConfigLocked(c, "10.244.0.9"); !strings.Contains(out, vni) || strings.Contains(out, "route 10.0.0.") || strings.Contains(out, "10.0.0.2") {
		t.Errorf("c's configuration (same segment, a unreachable):\n%s", out)
	}
	// Unpeered, a is a segment of its own again.
	s.res[na]["peerings"] = []any{}
	if out, _ := s.vpcConfigLocked(a, "10.244.0.7"); strings.Contains(out, "10.1.0.") || !strings.Contains(out, vni) {
		t.Errorf("a without its peering:\n%s", out)
	}
}

// TestPromoteEphemeralInternalAddress (#2140): reserving an instance's
// ephemeral internal IP makes a static address IN_USE by the instance; an
// instance given a reserved static address takes it; deleting the
// instance returns it to RESERVED.
func TestPromoteEphemeralInternalAddress(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{free: 1 << 40}})
	if err != nil {
		t.Fatal(err)
	}
	const project = "promote-p"
	call := func(method, path string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, project, path, nil, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return out
	}
	vm := func(name, ip string) {
		nic := map[string]any{"network": "global/networks/default"}
		if ip != "" {
			nic["networkIP"] = ip
		}
		call(http.MethodPost, "zones/us-central1-a/instances", obj{"name": name, "machineType": "zones/us-central1-a/machineTypes/e2-small",
			"disks":             []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{nic}})
		s.waitOp(context.Background(), "")
	}
	vm("one", "")
	inst := call(http.MethodGet, "zones/us-central1-a/instances/one", nil)
	ip := vmMapsTest(inst)
	call(http.MethodPost, "regions/us-central1/addresses", obj{"name": "kept", "addressType": "INTERNAL", "subnetwork": "regions/us-central1/subnetworks/default", "address": ip})
	a := call(http.MethodGet, "regions/us-central1/addresses/kept", nil)
	if a["status"] != "IN_USE" || len(strs(a, "users")) != 1 || !strings.HasSuffix(strs(a, "users")[0], "/instances/one") {
		t.Fatalf("promoted address = %v", a)
	}
	call(http.MethodPost, "regions/us-central1/addresses", obj{"name": "static", "addressType": "INTERNAL", "subnetwork": "regions/us-central1/subnetworks/default", "address": "10.128.0.50"})
	vm("two", "10.128.0.50")
	if a := call(http.MethodGet, "regions/us-central1/addresses/static", nil); a["status"] != "IN_USE" {
		t.Fatalf("an instance given a static address left it %v", a["status"])
	}
	if _, err := s.Call(http.MethodPost, project, "zones/us-central1-a/instances", nil, obj{"name": "three", "machineType": "zones/us-central1-a/machineTypes/e2-small",
		"disks":             []any{map[string]any{"boot": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/default", "networkIP": "10.128.0.50"}}}); err == nil {
		t.Error("a second instance took an address in use")
	}
	call(http.MethodDelete, "zones/us-central1-a/instances/two", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		a := call(http.MethodGet, "regions/us-central1/addresses/static", nil)
		if a["status"] == "RESERVED" && a["users"] == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after its instance was deleted the address is %v", a)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func vmMapsTest(inst obj) string {
	nics, _ := inst["networkInterfaces"].([]any)
	nic, _ := nics[0].(map[string]any)
	return str(nic, "networkIP")
}
