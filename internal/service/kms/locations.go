package kms

import (
	"context"
	"sort"
	"strings"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Locations are the locations ListLocations answers for every project
// (#1291): global, the multi-regions and dual-regions, and the regions
// Google's Cloud KMS locations page lists. Any well-formed location is
// accepted in a key ring's name, as before, so a location that holds a key
// ring is listed too, and GetLocation finds it. Which fields Google sets on
// each Location (here name, location_id, display_name and a LocationMetadata)
// is UNVERIFIED: no response was observed.
var Locations = []string{
	"global",
	"asia", "europe", "us",
	"eur4", "eur5", "eur6", "nam4", "nam6", "nam7", "nam8", "nam9", "nam10", "nam11", "nam12",
	"nam13", "nam14", "nam15", "nam16", "nam-eur-asia1", "asia1",
	"africa-south1",
	"asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3",
	"asia-south1", "asia-south2", "asia-southeast1", "asia-southeast2",
	"australia-southeast1", "australia-southeast2",
	"europe-central2", "europe-north1", "europe-southwest1",
	"europe-west1", "europe-west2", "europe-west3", "europe-west4", "europe-west6",
	"europe-west8", "europe-west9", "europe-west10", "europe-west12",
	"me-central1", "me-central2", "me-west1",
	"northamerica-northeast1", "northamerica-northeast2",
	"southamerica-east1", "southamerica-west1",
	"us-central1", "us-east1", "us-east4", "us-east5", "us-south1",
	"us-west1", "us-west2", "us-west3", "us-west4",
}

// locationsServer serves google.cloud.location.Locations, the mixin
// cloudkms_v1.yaml binds, over the key rings s stores.
type locationsServer struct {
	locationpb.UnimplementedLocationsServer
	s *Server
}

// locations is every location of project: the listed ones, and each
// location holding one of its key rings, sorted.
func (l locationsServer) locations(project string) ([]string, error) {
	seen := map[string]bool{}
	for _, id := range Locations {
		seen[id] = true
	}
	rings, err := l.s.list(ringPrefix, project+"/locations")
	if err != nil {
		return nil, err
	}
	for _, r := range rings {
		if parts := strings.Split(r, "/"); len(parts) >= 4 {
			seen[parts[3]] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// kmsLocation is one Location. Its metadata says no HSM and no EKM, since
// neither protection level is served locally.
func kmsLocation(project, id string) (*locationpb.Location, error) {
	md, err := anypb.New(&kmspb.LocationMetadata{})
	if err != nil {
		return nil, apierror.Internal(err, "location metadata")
	}
	return &locationpb.Location{Name: project + "/locations/" + id, LocationId: id, DisplayName: id, Metadata: md}, nil
}

// ListLocations lists the project's locations, paged. A filter is refused,
// since none is applied.
func (l locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetName() + "/locations/global"); err != nil || strings.Count(req.GetName(), "/") != 1 {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}", req.GetName()))
	}
	if req.GetFilter() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("ListLocations filter is not supported"))
	}
	ids, err := l.locations(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := paging.Page("kms-locations:"+req.GetName(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		loc, err := kmsLocation(req.GetName(), id)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		resp.Locations = append(resp.Locations, loc)
	}
	return resp, nil
}

// GetLocation answers a listed location; any other is NOT_FOUND.
func (l locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	project, id, err := resource.ParseLocation(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName()))
	}
	ids, err := l.locations("projects/" + project)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if i := sort.SearchStrings(ids, id); i < len(ids) && ids[i] == id {
		loc, err := kmsLocation("projects/"+project, id)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		return loc, nil
	}
	return nil, apierror.Wrap(apierror.NotFound("location %q not found", req.GetName()))
}
