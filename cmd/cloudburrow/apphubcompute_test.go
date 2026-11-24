package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAppHubDiscoversComputeTypes (#2153): forwarding rules, backend
// services and service attachments are services, instance groups
// workloads, at supported-resources' URIs by project ID, in their region
// (a zonal group's region and zone) or global.
func TestAppHubDiscoversComputeTypes(t *testing.T) {
	sl := "https://compute.googleapis.com/compute/v1/projects/p/"
	pages := map[string]string{
		"/compute/v1/projects/p/aggregated/forwardingRules":    `{"items":{"regions/us-central1":{"forwardingRules":[{"selfLink":"` + sl + `regions/us-central1/forwardingRules/ilb"}]},"global":{"forwardingRules":[{"selfLink":"` + sl + `global/forwardingRules/xlb"}]}}}`,
		"/compute/v1/projects/p/global/forwardingRules":        `{"items":[{"selfLink":"` + sl + `global/forwardingRules/xlb"}]}`,
		"/compute/v1/projects/p/aggregated/backendServices":    `{"items":{"global":{"backendServices":[{"selfLink":"` + sl + `global/backendServices/web"}]}}}`,
		"/compute/v1/projects/p/global/backendServices":        `{"items":[]}`,
		"/compute/v1/projects/p/aggregated/serviceAttachments": `{"items":{"regions/us-east1":{"serviceAttachments":[{"selfLink":"` + sl + `regions/us-east1/serviceAttachments/sa"}]},"regions/us-west1":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}}}`,
		"/compute/v1/projects/p/aggregated/instanceGroups":     `{"items":{"zones/us-central1-a":{"instanceGroups":[{"selfLink":"` + sl + `zones/us-central1-a/instanceGroups/web"}]}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := pages[r.URL.Path]; ok {
			_, _ = w.Write([]byte(b))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	got := map[string]string{}
	for _, r := range appHubComputeResources(context.Background(), hubLister{client: srv.Client()}, srv.URL, "p") {
		kind := "service"
		if r.Workload {
			kind = "workload"
		}
		got[r.URI] = kind + " " + r.Location + " " + r.Zone
	}
	cp := "//compute.googleapis.com/projects/p/"
	want := map[string]string{
		cp + "regions/us-central1/forwardingRules/ilb": "service us-central1 ",
		cp + "global/forwardingRules/xlb":              "service global ",
		cp + "global/backendServices/web":              "service global ",
		cp + "regions/us-east1/serviceAttachments/sa":  "service us-east1 ",
		cp + "zones/us-central1-a/instanceGroups/web":  "workload us-central1 us-central1-a",
	}
	if len(got) != len(want) {
		t.Fatalf("discovered %v", got)
	}
	for k, v := range want {
		if strings.TrimSpace(got[k]) != strings.TrimSpace(v) {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
}
