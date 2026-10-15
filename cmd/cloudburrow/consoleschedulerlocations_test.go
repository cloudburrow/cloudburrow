package main

import (
	"slices"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
)

// TestSchedulerCreateFormOffersListLocations (#1237): the Create job form's
// Region is a select of what Locations.ListLocations lists, defaulting to
// us-central1, so the console reaches the Locations mixin.
func TestSchedulerCreateFormOffersListLocations(t *testing.T) {
	t.Parallel()
	_, fields := schedulerProvider{}.CreateForm()
	for _, f := range fields {
		if f.Name != "location" {
			continue
		}
		want := slices.Clone(scheduler.Regions)
		slices.Sort(want)
		if f.Type != "select" || f.Default != "us-central1" || !slices.Equal(f.Options, want) {
			t.Fatalf("Region field = %+v; want a select of the %d listed regions", f, len(want))
		}
		return
	}
	t.Fatal("the Create job form has no Region field")
}
