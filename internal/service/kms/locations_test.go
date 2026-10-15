package kms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestLocations (#1291): the listed locations plus one holding a key ring,
// paged; GetLocation of a listed one, NOT_FOUND for another, and
// INVALID_ARGUMENT for a malformed name or project; a filter is refused.
func TestLocations(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory())
	if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/demo-project/locations/unit-lab", KeyRingId: "r"}); err != nil {
		t.Fatal(err)
	}
	l := s.Locations()
	seen := map[string]bool{}
	token := ""
	for pages := 0; ; pages++ {
		resp, err := l.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/demo-project", PageSize: 10, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range resp.GetLocations() {
			if loc.GetName() != "projects/demo-project/locations/"+loc.GetLocationId() || seen[loc.GetLocationId()] || loc.GetMetadata() == nil {
				t.Errorf("listed %v", loc)
			}
			seen[loc.GetLocationId()] = true
		}
		if token = resp.GetNextPageToken(); token == "" {
			if pages == 0 {
				t.Error("one page of 10 held every location")
			}
			break
		}
	}
	if len(seen) != len(Locations)+1 || !seen["global"] || !seen["us-central1"] || !seen["unit-lab"] {
		t.Errorf("listed %d locations; want the %d listed plus unit-lab", len(seen), len(Locations))
	}
	if loc, err := l.GetLocation(ctx, &locationpb.GetLocationRequest{Name: "projects/demo-project/locations/global"}); err != nil || loc.GetLocationId() != "global" {
		t.Errorf("GetLocation(global) = %v, %v", loc, err)
	}
	var md kmspb.LocationMetadata
	if loc, _ := l.GetLocation(ctx, &locationpb.GetLocationRequest{Name: "projects/demo-project/locations/global"}); loc.GetMetadata().UnmarshalTo(&md) != nil || md.GetHsmAvailable() || md.GetEkmAvailable() {
		t.Errorf("metadata = %v; want a LocationMetadata with no HSM and no EKM", loc.GetMetadata())
	}
	for name, want := range map[string]codes.Code{
		"projects/demo-project/locations/unit-lab":  codes.OK,
		"projects/other-project/locations/unit-lab": codes.NotFound,
		"projects/demo-project/locations/nowhere":   codes.NotFound,
		"projects/demo-project/locations/Bad_Name":  codes.InvalidArgument,
		"projects/demo-project":                     codes.InvalidArgument,
	} {
		if _, err := l.GetLocation(ctx, &locationpb.GetLocationRequest{Name: name}); status.Code(err) != want {
			t.Errorf("GetLocation(%s) = %v; want %v", name, err, want)
		}
	}
	for _, req := range []*locationpb.ListLocationsRequest{{Name: "projects/x"}, {Name: "projects/demo-project/locations"}, {Name: "projects/demo-project", Filter: "a=b"}} {
		want := codes.InvalidArgument
		if req.Filter != "" {
			want = codes.Unimplemented
		}
		if _, err := l.ListLocations(ctx, req); status.Code(err) != want {
			t.Errorf("ListLocations(%v) = %v; want %v", req, err, want)
		}
	}
}

// TestLocationsOverREST (#1291): cloudkms_v1.yaml's two Locations paths are
// transcoded to the mixin.
func TestLocationsOverREST(t *testing.T) {
	srv := httptest.NewServer(NewRESTHandler(NewServer(store.NewMemory())))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/projects/demo-project/locations?pageSize=3")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Locations []struct {
			LocationID string `json:"locationId"`
		} `json:"locations"`
		NextPageToken string `json:"nextPageToken"`
	}
	err = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if resp.StatusCode != 200 || err != nil || len(list.Locations) != 3 || list.NextPageToken == "" {
		t.Fatalf("GET locations = %d %+v (%v)", resp.StatusCode, list, err)
	}
	resp, err = http.Get(srv.URL + "/v1/projects/demo-project/locations/us-east1")
	if err != nil {
		t.Fatal(err)
	}
	var loc struct {
		LocationID string `json:"locationId"`
		Metadata   struct {
			Type string `json:"@type"`
		} `json:"metadata"`
	}
	err = json.NewDecoder(resp.Body).Decode(&loc)
	resp.Body.Close()
	if resp.StatusCode != 200 || err != nil || loc.LocationID != "us-east1" || loc.Metadata.Type != "type.googleapis.com/google.cloud.kms.v1.LocationMetadata" {
		t.Fatalf("GET location = %d %+v (%v)", resp.StatusCode, loc, err)
	}
}
