package networkconnectivity

import (
	"net/http/httptest"
	"testing"
)

// TestHandles: the Compute Engine port routes only this API's collections
// and operations here; Serverless VPC Access's connectors, the locations
// and other APIs' operations are not this API's (main run 45371505851).
func TestHandles(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.ops["projects/p/locations/us-central1/operations/operation-mine"] = nil
	for path, want := range map[string]bool{
		"/v1/projects/p/locations/us-central1/serviceConnectionPolicies":        true,
		"/v1/projects/p/locations/us-central1/serviceConnectionPolicies/a":      true,
		"/v1/projects/p/locations/global/hubs/h:getIamPolicy":                   true,
		"/v1/projects/p/locations/us-central1/operations/operation-mine":        true,
		"/v1/projects/p/locations/us-central1/operations/operation-mine:cancel": true,
		"/v1/projects/p/locations/us-central1/connectors":                       false,
		"/v1/projects/p/locations/us-central1/operations/other":                 false,
		"/v1/projects/p/locations":                                              false,
		"/v1/projects/p/locations/global/certificateMaps":                       false,
		"/compute/v1/projects/p/global/networks":                                false,
	} {
		if got := s.Handles(httptest.NewRequest("GET", path, nil)); got != want {
			t.Errorf("Handles(%s) = %v, want %v", path, got, want)
		}
	}
}
