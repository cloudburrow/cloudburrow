package gke

import (
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestClusterNetworksAreCheckedAgainstComputeEngine (#1936).
func TestClusterNetworksAreCheckedAgainstComputeEngine(t *testing.T) {
	cs, err := compute.New(compute.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Call("POST", "p", "global/networks", nil, map[string]any{"name": "vpc", "autoCreateSubnetworks": false}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Call("POST", "p", "regions/us-central1/subnetworks", nil, map[string]any{"name": "nodes", "network": "global/networks/vpc", "ipCidrRange": "10.1.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	if err := s.checkClusterVPC(&containerpb.Cluster{Network: "nope"}, "p", "us-central1-a"); err != nil {
		t.Fatalf("without compute: %v", err)
	}
	s.opts.Networks = func() Networks { return cs }
	for _, tc := range []struct {
		c  *containerpb.Cluster
		ok bool
	}{
		{&containerpb.Cluster{}, true},
		{&containerpb.Cluster{Network: "vpc", Subnetwork: "nodes"}, true},
		{&containerpb.Cluster{NetworkConfig: &containerpb.NetworkConfig{Network: "projects/p/global/networks/vpc", Subnetwork: "projects/p/regions/us-central1/subnetworks/nodes"}}, true},
		{&containerpb.Cluster{Network: "nope"}, false},
		{&containerpb.Cluster{Network: "vpc"}, false},
		{&containerpb.Cluster{Network: "vpc", Subnetwork: "default"}, false},
	} {
		if err := s.checkClusterVPC(tc.c, "p", "us-central1-a"); (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.c, err)
		}
	}
}

func TestNetworkUsersNameTheClustersOnANetwork(t *testing.T) {
	s := &Server{clusters: map[string]*entry{
		"projects/p/locations/us-central1-a/clusters/c": {cluster: &containerpb.Cluster{NetworkConfig: &containerpb.NetworkConfig{
			Network: "projects/p/global/networks/vpc", Subnetwork: "projects/p/regions/us-central1/subnetworks/nodes"}}},
	}}
	if u := s.NetworkUsers("projects/p/global/networks/vpc"); len(u) != 1 {
		t.Errorf("network users: %v", u)
	}
	if u := s.NetworkUsers("projects/p/regions/us-central1/subnetworks/nodes"); len(u) != 1 {
		t.Errorf("subnet users: %v", u)
	}
	if u := s.NetworkUsers("projects/p/global/networks/other"); len(u) != 0 {
		t.Errorf("other: %v", u)
	}
}
