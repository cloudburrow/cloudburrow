package scheduler

import (
	"context"
	"strings"
	"testing"

	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// The Locations mixin (#1237): every region, plus a location that holds a
// job; GetLocation of either; NOT_FOUND, INVALID_ARGUMENT and UNIMPLEMENTED
// for an unknown location, a malformed name and a filter.
func TestLocations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewStore(store.NewMemory())
	if _, err := st.Create(Job{Name: "projects/demo-project/locations/local-lab/jobs/j", Schedule: "* * * * *", TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	l := NewLocationsServer(st)
	resp, err := l.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/demo-project", PageSize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetLocations()) != len(Regions)+1 || resp.GetNextPageToken() != "" {
		t.Fatalf("ListLocations = %d locations, token %q; want %d", len(resp.GetLocations()), resp.GetNextPageToken(), len(Regions)+1)
	}
	found := false
	for _, loc := range resp.GetLocations() {
		found = found || loc.GetLocationId() == "local-lab"
	}
	if !found {
		t.Error("the location holding a job is not listed")
	}
	if other, _ := l.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/other-project", PageSize: 1000}); len(other.GetLocations()) != len(Regions) {
		t.Errorf("another project lists %d locations; want %d", len(other.GetLocations()), len(Regions))
	}
	for name, want := range map[string]codes.Code{
		"projects/demo-project/locations/us-central1": codes.OK,
		"projects/demo-project/locations/local-lab":   codes.OK,
		"projects/other-project/locations/local-lab":  codes.NotFound,
		"projects/demo-project/locations/Bad_Name":    codes.InvalidArgument,
		"projects/demo-project":                       codes.InvalidArgument,
	} {
		got, err := l.GetLocation(ctx, &locationpb.GetLocationRequest{Name: name})
		if status.Code(err) != want || (err == nil && got.GetName() != name) {
			t.Errorf("GetLocation(%s) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := l.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/demo-project", Filter: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad filter: %v; want INVALID_ARGUMENT", err)
	}
	// The mixin's AIP-160 filter (#1943).
	got, err := l.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/demo-project", Filter: `locationId = "us-*" AND NOT locationId = us-central1`})
	if err != nil || len(got.GetLocations()) == 0 {
		t.Fatalf("filtered ListLocations: %v, %v", got, err)
	}
	for _, loc := range got.GetLocations() {
		if !strings.HasPrefix(loc.GetLocationId(), "us-") || loc.GetLocationId() == "us-central1" {
			t.Errorf("filter let %s through", loc.GetName())
		}
	}
	if _, err := l.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "demo-project"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("malformed name: %v; want INVALID_ARGUMENT", err)
	}
}
