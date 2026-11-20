package redisadmin

import (
	"testing"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
)

// TestInstanceNetwork (#1906): the network fields' defaults and checks.
func TestInstanceNetwork(t *testing.T) {
	const name = "projects/p/locations/us-central1/instances/i"
	nw, mode, rg, err := instanceNetwork(&redispb.Instance{}, name, "p")
	if err != nil || nw != "projects/p/global/networks/default" || mode != "DIRECT_PEERING" || checkRange(rg, 29) != nil {
		t.Errorf("defaults = %q %q %q %v", nw, mode, rg, err)
	}
	if _, _, rg, _ := instanceNetwork(&redispb.Instance{ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED}, name, "p"); checkRange(rg, 28) != nil || rg[len(rg)-3:] != "/28" {
		t.Errorf("read replicas' default range %q", rg)
	}
	if nw, _, _, _ := instanceNetwork(&redispb.Instance{AuthorizedNetwork: "vpc-1"}, name, "p"); nw != "projects/p/global/networks/vpc-1" {
		t.Errorf("bare network name: %q", nw)
	}
	if _, mode, rg, err := instanceNetwork(&redispb.Instance{ConnectMode: redispb.Instance_PRIVATE_SERVICE_ACCESS, ReservedIpRange: "google-managed-services-default"}, name, "p"); err != nil || mode != "PRIVATE_SERVICE_ACCESS" || rg != "google-managed-services-default" {
		t.Errorf("PSA with an allocated range: %q %q %v", mode, rg, err)
	}
	for _, in := range []*redispb.Instance{
		{AuthorizedNetwork: "projects/p/networks/x"},
		{ReservedIpRange: "10.0.0.0/30"},
		{ReservedIpRange: "10.0.0.8/28"},
		{ReservedIpRange: "8.8.8.0/29"},
		{ReservedIpRange: "fd00::/64"},
		{ReservedIpRange: "my-range"},
		{ReservedIpRange: "10.0.0.0/29", ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED},
		{ConnectMode: redispb.Instance_ConnectMode(9)},
	} {
		if _, _, _, err := instanceNetwork(in, name, "p"); code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v, want INVALID_ARGUMENT", in, err)
		}
	}
	if _, _, _, err := instanceNetwork(&redispb.Instance{ReservedIpRange: "192.168.0.0/24"}, name, "p"); err != nil {
		t.Errorf("a /24: %v", err)
	}
}

// TestRestrictedAndOverlappingRanges (#1919): the VPC's restricted ranges,
// a range reaching out of its private block, and another instance's range
// on the same network are refused; a chosen range skips the used ones.
func TestRestrictedAndOverlappingRanges(t *testing.T) {
	for _, s := range []string{"127.0.0.0/29", "169.254.10.0/29", "199.36.153.0/29", "0.0.0.0/29", "224.0.0.0/29", "10.0.0.0/7", "172.16.0.0/11"} {
		if err := checkRange(s, 29); code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", s, err)
		}
	}
	for _, s := range []string{"192.88.99.0/29", "100.64.0.0/29", "240.0.0.0/29", "172.17.0.0/29"} {
		if err := checkRange(s, 29); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	c := &instances{byID: map[string]*record{}}
	net1 := "projects/p/global/networks/default"
	a := &record{Name: "projects/p/locations/us-central1/instances/a", AuthorizedNetwork: net1, ReservedIPRange: "10.1.0.0/28"}
	c.byID[a.Name] = a
	b := &record{Name: "projects/p/locations/us-central1/instances/b", AuthorizedNetwork: net1, ReservedIPRange: "10.1.0.8/29"}
	if err := c.claimRange(b, true); code(err) != codes.InvalidArgument {
		t.Errorf("overlapping range: %v, want INVALID_ARGUMENT", err)
	}
	other := &record{Name: b.Name, AuthorizedNetwork: "projects/p/global/networks/vpc-2", ReservedIPRange: "10.1.0.8/29"}
	if err := c.claimRange(other, true); err != nil {
		t.Errorf("the same range on another network: %v", err)
	}
	// A chosen range that would collide moves to the next unused block.
	d := &record{Name: "projects/p/locations/us-central1/instances/d", AuthorizedNetwork: net1}
	d.ReservedIPRange = defaultRange(d.Name, 29)
	c.byID["projects/p/locations/us-central1/instances/x"] = &record{Name: "projects/p/locations/us-central1/instances/x", AuthorizedNetwork: net1, ReservedIPRange: d.ReservedIPRange}
	if err := c.claimRange(d, false); err != nil || d.ReservedIPRange == defaultRange(d.Name, 29) || checkRange(d.ReservedIPRange, 29) != nil {
		t.Errorf("chosen range %q, %v", d.ReservedIPRange, err)
	}
	if err := c.claimRange(&record{Name: "n", AuthorizedNetwork: net1, ReservedIPRange: "google-managed-services-default"}, true); err != nil {
		t.Errorf("an allocated range's name: %v", err)
	}
}
