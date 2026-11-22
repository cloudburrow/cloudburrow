package compute

import (
	"strings"
	"testing"
)

// TestVPCServicesReachInternalLBsAndEndpoints (#2140): a VM's ruleset
// carries its connections to an internal load balancer's address and to a
// Private Service Connect endpoint for a published service to the
// listener of the load balancer behind them, as its pod reaches it; a VM
// of another region reaches neither until global access is on, and the
// egress rules match the original destination.
func TestVPCServicesReachInternalLBsAndEndpoints(t *testing.T) {
	s, _ := newTestServer(t, Options{VMs: fakeVMs{}, GuestReach: func(a string) string {
		_, port, _ := strings.Cut(a, ":")
		return "192.168.65.254:" + port
	}})
	must := func(project, method, path string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, project, path, nil, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return out
	}
	const prod, cons, region = "svc-prod", "svc-cons", "us-central1"
	producerILB(t, s, prod, region)
	ilb := must(prod, "GET", "regions/"+region+"/forwardingRules/ilb", nil)
	listener := strings.TrimPrefix(strs(ilb, "cloudburrowAddresses")[0], "80=127.0.0.1:")
	must(prod, "POST", "regions/"+region+"/subnetworks", obj{"name": "psc-nat", "network": "global/networks/default",
		"ipCidrRange": "10.0.2.0/24", "purpose": "PRIVATE_SERVICE_CONNECT"})
	must(prod, "POST", "regions/"+region+"/serviceAttachments", obj{"name": "svc", "connectionPreference": "ACCEPT_AUTOMATIC",
		"targetService": "projects/" + prod + "/regions/" + region + "/forwardingRules/ilb",
		"natSubnets":    []any{"regions/" + region + "/subnetworks/psc-nat"}})
	must(cons, "POST", "regions/"+region+"/forwardingRules", obj{"name": "ep", "network": "global/networks/default",
		"subnetwork": "regions/" + region + "/subnetworks/default",
		"target":     "projects/" + prod + "/regions/" + region + "/serviceAttachments/svc"})
	ep := must(cons, "GET", "regions/"+region+"/forwardingRules/ep", nil)
	vm := func(project, name, zone string) obj {
		must(project, "POST", "zones/"+zone+"/instances", obj{"name": name, "machineType": "zones/" + zone + "/machineTypes/e2-small",
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
		return s.res["projects/"+project+"/zones/"+zone+"/instances/"+name]
	}
	conf := func(inst obj) string {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, _ := s.vpcConfigLocked(inst, "10.244.0.9")
		return c
	}
	pvm, cvm, far := vm(prod, "client", "us-central1-a"), vm(cons, "client", "us-central1-a"), vm(cons, "far", "europe-west1-b")
	toILB := "ip daddr " + str(ilb, "IPAddress") + " tcp dport 80 dnat ip to 192.168.65.254:" + listener + ` comment "ilb"`
	toEP := "ip daddr " + str(ep, "IPAddress") + " tcp dport 80 dnat ip to 192.168.65.254:" + listener + ` comment "ep"`
	if c := conf(pvm); !strings.Contains(c, "chain services {\n    type nat hook output priority -100;") || !strings.Contains(c, toILB) {
		t.Fatalf("the producer's VM does not reach its internal load balancer:\n%s", c)
	}
	if c := conf(cvm); !strings.Contains(c, toEP) || strings.Contains(c, toILB) {
		t.Fatalf("the consumer's VM: the endpoint, not the producer's address:\n%s", c)
	}
	if c := conf(far); strings.Contains(c, toEP) {
		t.Fatalf("another region's VM reaches the endpoint without global access:\n%s", c)
	}
	must(cons, "PATCH", "regions/"+region+"/forwardingRules/ep", obj{"allowPscGlobalAccess": true, "fingerprint": ep["fingerprint"]})
	if c := conf(far); !strings.Contains(c, toEP) {
		t.Fatalf("global access on, and still not reached:\n%s", c)
	}
	// The egress rules match where the connection was going.
	must(cons, "POST", "global/firewalls", obj{"name": "no-ep", "network": "global/networks/default", "direction": "EGRESS",
		"destinationRanges": []any{str(ep, "IPAddress") + "/32"}, "denied": []any{obj{"IPProtocol": "tcp"}}, "priority": 100})
	if c := conf(cvm); !strings.Contains(c, "ct original ip daddr { "+str(ep, "IPAddress")+"/32 } meta l4proto tcp drop") {
		t.Fatalf("egress deny of the endpoint:\n%s", c)
	}
	// A published rule cannot be deleted while an attachment publishes it.
	if _, err := s.Call("DELETE", prod, "regions/"+region+"/forwardingRules/ilb", nil, nil); ErrorStatus(err) != 400 {
		t.Fatalf("deleting a published rule: %v", err)
	}
	// targetService must exist and be an internal load balancer's.
	if _, err := s.Call("POST", prod, "regions/"+region+"/serviceAttachments", nil, obj{"name": "nope", "connectionPreference": "ACCEPT_AUTOMATIC",
		"targetService": "projects/" + prod + "/regions/" + region + "/forwardingRules/none",
		"natSubnets":    []any{"regions/" + region + "/subnetworks/psc-nat"}}); ErrorStatus(err) != 404 {
		t.Fatalf("a missing target service: %v", err)
	}
}
