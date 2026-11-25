package main

import "testing"

// TestDataflowNetwork (#2215): a job's workers run on its network option
// (a name or a complete URL), on Dataflow's default network named
// default when it gives none, and a subnetwork's network is not guessed
// without Compute Engine.
func TestDataflowNetwork(t *testing.T) {
	for _, c := range []struct{ network, subnetwork, want string }{
		{"", "", "projects/p/global/networks/default"},
		{"vpc", "", "projects/p/global/networks/vpc"},
		{"https://www.googleapis.com/compute/v1/projects/host/global/networks/shared", "", "projects/host/global/networks/shared"},
		{"global/networks/vpc", "", "projects/p/global/networks/vpc"},
		{"", "regions/us-central1/subnetworks/s", ""},
	} {
		if got := dataflowNetwork("p", c.network, c.subnetwork, "us-central1", nil); got != c.want {
			t.Errorf("dataflowNetwork(%q, %q) = %q; want %q", c.network, c.subnetwork, got, c.want)
		}
	}
}
