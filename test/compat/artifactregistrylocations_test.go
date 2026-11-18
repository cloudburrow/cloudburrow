//go:build compat

package compat

import (
	"strings"
	"testing"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// arLocationsClient is the official Artifact Registry client over REST at
// the prefix the Run port serves Artifact Registry under (#1578), where its
// Locations mixin is: GET /v1/projects/{p}/locations on the port itself is
// Cloud Run v1's.
func arLocationsClient(t *testing.T, h *Harness) *artifactregistry.Client {
	t.Helper()
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ep := h.Endpoint(EnvRun)
	if ep == "" {
		t.Fatal("no Run endpoint")
	}
	c, err := artifactregistry.NewRESTClient(h.Context(), option.WithEndpoint("http://"+ep+"/artifactregistry"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: google.cloud.location.Locations/ListLocations, google.cloud.location.Locations/GetLocation
//
// TestArtifactRegistryLocations (#1578): the official client lists the
// repository locations Google's "Repository locations" page names, the
// multi-regions asia, europe and us among them, pages them, gets one, and
// a location not listed is NOT_FOUND.
func TestArtifactRegistryLocations(t *testing.T) {
	h := New(t)
	c := arLocationsClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project()
	var ids []string
	it := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: parent, PageSize: 10})
	for {
		l, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLocations: %v", err)
		}
		if l.GetName() != parent+"/locations/"+l.GetLocationId() {
			t.Errorf("location %v: name and id disagree", l)
		}
		ids = append(ids, l.GetLocationId())
	}
	if len(ids) < 40 {
		t.Errorf("ListLocations gave %d locations: %v", len(ids), ids)
	}
	joined := "," + strings.Join(ids, ",") + ","
	for _, want := range []string{"us", "europe", "asia", "us-central1", "europe-west1", "asia-southeast3", "northamerica-south1"} {
		if !strings.Contains(joined, ","+want+",") {
			t.Errorf("ListLocations has no %q: %v", want, ids)
		}
	}
	l, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent + "/locations/europe"})
	if err != nil || l.GetLocationId() != "europe" || l.GetName() != parent+"/locations/europe" {
		t.Errorf("GetLocation(europe) = %v, %v", l, err)
	}
	if _, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent + "/locations/mars-north1"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetLocation of an unknown location: %v; want NotFound", err)
	}
}
