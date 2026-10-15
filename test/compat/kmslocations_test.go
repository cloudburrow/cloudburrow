//go:build compat

package compat

import (
	"errors"
	"testing"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.location.Locations/ListLocations, google.cloud.location.Locations/GetLocation
//
// TestKMSLocations (#1291), with cloud.google.com/go/kms/apiv1's Locations
// mixin over gRPC and REST: ListLocations pages through global, the
// multi-regions and the regions, plus a location that holds a key ring;
// GetLocation reads one back with a LocationMetadata saying no HSM and no
// EKM; an unknown location is NOT_FOUND and a malformed name
// INVALID_ARGUMENT.
func TestKMSLocations(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	project := "projects/" + h.Project()
	clients := kmsClients(t, h)
	parent := project + "/locations/compat-kms-lab"
	if _, err := clients["grpc"].CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: "locations"}); err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			seen := map[string]bool{}
			it := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: project, PageSize: 9})
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
			for _, id := range []string{"global", "us", "europe", "us-central1", "europe-west1", "compat-kms-lab"} {
				if !seen[id] {
					t.Errorf("ListLocations did not list %s (%d listed)", id, len(seen))
				}
			}
			if len(seen) <= 9 {
				t.Errorf("ListLocations listed %d locations; want more than one page of 9", len(seen))
			}

			loc, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: project + "/locations/global"})
			if err != nil || loc.GetLocationId() != "global" {
				t.Fatalf("GetLocation(global) = %v, %v", loc, err)
			}
			var md kmspb.LocationMetadata
			if err := loc.GetMetadata().UnmarshalTo(&md); err != nil || md.GetHsmAvailable() || md.GetEkmAvailable() {
				t.Errorf("GetLocation(global).metadata = %v (%v); want a LocationMetadata, no HSM, no EKM", loc.GetMetadata(), err)
			}
			if loc, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent}); err != nil || loc.GetName() != parent {
				t.Errorf("GetLocation(compat-kms-lab) = %v, %v", loc, err)
			}
			if _, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: project + "/locations/no-such-region"}); status.Code(err) != codes.NotFound {
				t.Errorf("GetLocation(no-such-region) = %v; want NOT_FOUND", err)
			}
			if _, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: project + "/locations/Bad_Name"}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("GetLocation(Bad_Name) = %v; want INVALID_ARGUMENT", err)
			}
		})
	}
}
