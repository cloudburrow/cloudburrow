package redisadmin

import (
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
)

// TestZoneDistribution (#1908): the checks, and each node's zone.
func TestZoneDistribution(t *testing.T) {
	for _, z := range []zoneDist{
		{Mode: "SINGLE_ZONE"},
		{Mode: "SINGLE_ZONE", Zone: "europe-west1-b"},
		{Mode: "SINGLE_ZONE", Zone: "us-central1"},
		{Mode: "MULTI_ZONE", Zones: []string{"us-central1-a", "us-east1-b"}},
		{Mode: "MULTI_ZONE", Zones: []string{"us-central1-a", "us-central1-a"}},
		{Mode: "BOTH"},
	} {
		if code(z.check("us-central1")) != codes.InvalidArgument {
			t.Errorf("%+v: want INVALID_ARGUMENT", z)
		}
	}
	for _, z := range []zoneDist{{Mode: "ZONE_DISTRIBUTION_MODE_UNSPECIFIED"}, {Mode: "SINGLE_ZONE", Zone: "us-central1-f"}, {Mode: "MULTI_ZONE", Zone: "ignored"}} {
		if err := z.check("us-central1"); err != nil {
			t.Errorf("%+v: %v", z, err)
		}
	}
	if !(zoneDist{Mode: "ZONE_DISTRIBUTION_MODE_UNSPECIFIED"}).same(zoneDist{Mode: "MULTI_ZONE"}) || (zoneDist{Mode: "MULTI_ZONE"}).same(zoneDist{Mode: "SINGLE_ZONE", Zone: "us-central1-a"}) {
		t.Error("same")
	}
	r := &clusterRecord{Name: "projects/p/locations/us-central1/clusters/c"}
	for sh := 0; sh < 2; sh++ {
		for k := 0; k < 3; k++ {
			r.Nodes = append(r.Nodes, &clusterNode{Shard: sh, Primary: k == 0})
		}
	}
	var got []string
	for _, n := range r.Nodes {
		got = append(got, r.nodeZone(n))
	}
	want := []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-b", "us-central1-c", "us-central1-a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-zone node zones %v, want %v", got, want)
	}
	r.ZoneDistribution = `{"mode":2,"zone":"us-central1-f"}`
	if z := r.activeZones(); !reflect.DeepEqual(z, []string{"us-central1-f"}) {
		t.Errorf("single-zone active zones %v", z)
	}
	if sameClusterZones(r.ZoneDistribution, nil) {
		t.Error("a change from SINGLE_ZONE to the default is not refused")
	}
}
