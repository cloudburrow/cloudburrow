package compute

import (
	"fmt"
	"net"
	"testing"
)

// TestExternalSubnetV6SkipsTheGlobalPool: every EXTERNAL dual-stack subnet
// gets a /64 of 2600:1900::/28 outside the global pool, whatever its
// selfLink hashes to. Before the fix a seed hashing into 2600:1901::/32
// (1 in 16) spent every probe there and the subnet was refused "No free
// IPv6 /64 for the subnetwork." (main CI after Landing 168).
func TestExternalSubnetV6SkipsTheGlobalPool(t *testing.T) {
	s, _ := newTestServer(t, Options{VMs: fakeVMs{}})
	for i := 0; i < 400; i++ {
		seed := fmt.Sprintf("https://compute.googleapis.com/compute/v1/projects/p/regions/us-central1/subnetworks/s%d", i)
		r, err := s.subnetV6Locked(obj{"name": "n"}, "EXTERNAL", seed)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ip, n, err := net.ParseCIDR(r)
		if err != nil {
			t.Fatalf("seed %d: %q: %v", i, r, err)
		}
		if ones, _ := n.Mask.Size(); ones != 64 || !externalPool.Contains(ip) || globalV6Pool.Contains(ip) {
			t.Fatalf("seed %d: %s, want a /64 of %s outside %s", i, r, externalPool, globalV6Pool)
		}
	}
}
