//go:build compat

package compat

import (
	"context"
	"errors"
	"testing"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/iterator"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.location.Locations/ListLocations, google.cloud.location.Locations/GetLocation
//
// TestSchedulerLocations (#1237), with cloud.google.com/go/scheduler/apiv1's
// Locations mixin: ListLocations pages through the regions, plus a location
// that holds a job; GetLocation reads one back; an unknown location is
// NOT_FOUND and a malformed name INVALID_ARGUMENT.
func TestSchedulerLocations(t *testing.T) {
	h := New(t)
	c := schedulerClient(t, h)
	ctx := h.Context()
	project := "projects/" + h.Project()

	parent := project + "/locations/compat-lab"
	job, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/locations", Schedule: "0 0 1 1 *",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:1/"}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job.GetName()}) })

	seen := map[string]bool{}
	it := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: project, PageSize: 7})
	for {
		loc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListLocations: %v", err)
		}
		if loc.GetName() != project+"/locations/"+loc.GetLocationId() || seen[loc.GetLocationId()] {
			t.Errorf("listed location %v", loc)
		}
		seen[loc.GetLocationId()] = true
	}
	for _, id := range []string{"us-central1", "europe-west1", "asia-northeast1", "compat-lab"} {
		if !seen[id] {
			t.Errorf("ListLocations did not list %s (%d listed)", id, len(seen))
		}
	}
	if len(seen) <= 7 {
		t.Errorf("ListLocations listed %d locations; want more than one page of 7", len(seen))
	}

	if loc, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: project + "/locations/us-central1"}); err != nil || loc.GetLocationId() != "us-central1" {
		t.Errorf("GetLocation(us-central1) = %v, %v", loc, err)
	}
	if loc, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent}); err != nil || loc.GetName() != parent {
		t.Errorf("GetLocation(compat-lab) = %v, %v", loc, err)
	}
	if _, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: project + "/locations/no-such-region"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetLocation(no-such-region) = %v; want NOT_FOUND", err)
	}
	if _, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: project + "/locations/Bad_Name"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetLocation(Bad_Name) = %v; want INVALID_ARGUMENT", err)
	}
}
