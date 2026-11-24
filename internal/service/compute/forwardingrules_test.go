package compute

import (
	"context"
	"net/http"
	"strings"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestPSCEndpointsThroughTheGoClient is #2141: a Private Service Connect
// endpoint is a forwarding rule targeting a service attachment, at a
// reserved internal address (IN_USE while it is), ACCEPTED while its
// project is on the producer's accept list and PENDING otherwise, read
// again each time; a VM of its network reaches the producer at its
// address; load balancing rules and unknown attachments are refused.
func TestPSCEndpointsThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	const attach = "projects/123456789012/regions/us-central1/serviceAttachments/a-psc"
	allowed := map[string]bool{"proj": true}
	s, opts := newTestServer(t, Options{ServiceAttachment: func(link, consumer string) (PrivateService, bool, bool) {
		if link != attach {
			return PrivateService{}, false, false
		}
		return PrivateService{Port: 5432, TargetIP: "10.96.7.7", TargetPort: 5432}, allowed[consumer], true
	}})
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	rules, err := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	const p, r = "proj", "us-central1"
	op, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: r, AddressResource: &computepb.Address{
		Name: proto.String("sql-ep"), AddressType: proto.String("INTERNAL"), Subnetwork: proto.String("default"), Address: proto.String("10.128.0.50")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	if _, err := rules.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: r, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String("lb"), Target: proto.String("projects/proj/regions/us-central1/targetPools/x")}}); code(err) != http.StatusNotFound {
		// Load balancers' rules are served since #2153: an absent target
		// pool is not found.
		t.Errorf("a load balancing rule of an absent target pool: %v", err)
	}
	if _, err := rules.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: r, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String("nope"), Network: proto.String("default"), Target: proto.String("projects/1/regions/us-central1/serviceAttachments/none")}}); code(err) != http.StatusNotFound {
		t.Errorf("an unknown attachment: %v", err)
	}
	op, err = rules.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: r, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String("sql-endpoint"), Network: proto.String("default"), IPAddress: proto.String("sql-ep"),
		Target: proto.String("https://www.googleapis.com/compute/v1/" + attach)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	fr, err := rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: r, ForwardingRule: "sql-endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	if fr.GetIPAddress() != "10.128.0.50" || fr.GetPscConnectionStatus() != "ACCEPTED" || fr.GetPscConnectionId() == 0 ||
		!strings.HasSuffix(fr.GetTarget(), attach) {
		t.Errorf("endpoint: %v", fr)
	}
	// An endpoint given no address takes the subnet's first free one, which
	// is then not handed to anything else.
	op, err = rules.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: r, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String("auto-ep"), Network: proto.String("default"), Target: proto.String(attach)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	auto, _ := rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: r, ForwardingRule: "auto-ep"})
	op, err = addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: r, AddressResource: &computepb.Address{
		Name: proto.String("after"), AddressType: proto.String("INTERNAL"), Subnetwork: proto.String("default")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	if after, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: r, Address: "after"}); auto.GetIPAddress() == "" || after.GetAddress() == auto.GetIPAddress() {
		t.Errorf("an endpoint's address %s was handed out again: %s", auto.GetIPAddress(), after.GetAddress())
	}
	a, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: r, Address: "sql-ep"})
	if a.GetStatus() != "IN_USE" || len(a.GetUsers()) != 1 {
		t.Errorf("the endpoint's address: %v", a)
	}
	if _, err := addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: p, Region: r, Address: "sql-ep"}); err == nil {
		t.Error("an address in use was deleted")
	}

	// A VM of the network reaches the producer at the endpoint's address.
	s.mu.Lock()
	vm := obj{"name": "vm", "selfLink": "projects/proj/zones/us-central1-a/instances/vm", "zone": "projects/proj/zones/us-central1-a",
		"status": "RUNNING", "networkInterfaces": []any{map[string]any{"network": "projects/proj/global/networks/default", "networkIP": "10.128.0.2"}}}
	s.res["projects/proj/zones/us-central1-a/instances/vm"] = vm
	out, _ := s.vpcConfigLocked(vm, "10.244.0.5")
	other := obj{"name": "vm2", "selfLink": "projects/proj/zones/europe-west1-b/instances/vm2", "zone": "projects/proj/zones/europe-west1-b",
		"status": "RUNNING", "networkInterfaces": []any{map[string]any{"network": "projects/proj/global/networks/default", "networkIP": "10.132.0.2"}}}
	s.res["projects/proj/zones/europe-west1-b/instances/vm2"] = other
	far, _ := s.vpcConfigLocked(other, "10.244.0.6")
	s.mu.Unlock()
	if !strings.Contains(out, `ip daddr 10.128.0.50 tcp dport 5432 dnat ip to 10.96.7.7:5432 comment "psc sql-endpoint"`) {
		t.Errorf("the VM's ruleset lacks the endpoint:\n%s", out)
	}
	if strings.Contains(far, "10.128.0.50") {
		t.Errorf("a VM of another region reaches an endpoint without global access:\n%s", far)
	}

	// Off the accept list, the endpoint is PENDING, and unreachable.
	allowed["proj"] = false
	fr, _ = rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: r, ForwardingRule: "sql-endpoint"})
	if fr.GetPscConnectionStatus() != "PENDING" {
		t.Errorf("off the accept list: %s", fr.GetPscConnectionStatus())
	}
	s.mu.Lock()
	out, _ = s.vpcConfigLocked(vm, "10.244.0.5")
	s.mu.Unlock()
	if strings.Contains(out, "10.128.0.50") {
		t.Errorf("a PENDING endpoint is reachable:\n%s", out)
	}
	op, err = rules.Delete(ctx, &computepb.DeleteForwardingRuleRequest{Project: p, Region: r, ForwardingRule: "sql-endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	if a, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: r, Address: "sql-ep"}); a.GetStatus() != "RESERVED" {
		t.Errorf("after the endpoint's deletion its address is %s", a.GetStatus())
	}
}
