package main

import "testing"

func TestAppHubDiscoverNames(t *testing.T) {
	for in, want := range map[string]string{"US": "us", "us-central1": "us-central1", "EU": "eu", "nam5": "global", "": "global", "US-EAST1": "us-east1"} {
		if got := hubLocation(in); got != want {
			t.Errorf("hubLocation(%q) = %q; want %q", in, got, want)
		}
	}
	if got := withProject("projects/my-proj/locations/global/buckets/_Default", "123"); got != "projects/123/locations/global/buckets/_Default" {
		t.Errorf("withProject = %q", got)
	}
	if got := nameSegment("projects/p/locations/us-east1-b", "locations"); got != "us-east1-b" || hubRegionOfZone(got) != "us-east1" {
		t.Errorf("nameSegment/hubRegionOfZone = %q, %q", got, hubRegionOfZone(got))
	}
}
