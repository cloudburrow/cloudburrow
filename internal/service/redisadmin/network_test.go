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
