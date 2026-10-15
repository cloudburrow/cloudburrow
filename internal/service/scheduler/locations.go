package scheduler

import (
	"context"
	"regexp"
	"sort"
	"strings"

	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// Regions are the locations ListLocations answers for every project (#1237):
// the regions Google's Cloud Scheduler locations page lists. Any well-formed
// location is accepted in a job's name, as before, so a location that holds
// a job is listed too, and GetLocation finds it. Which fields Google sets on
// each Location (here name, location_id and display_name) is UNVERIFIED: no
// response was observed.
var Regions = []string{
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

var (
	projectRE  = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	locationRE = regexp.MustCompile(`^(projects/[a-z][a-z0-9-]{4,28}[a-z0-9])/locations/([a-z0-9-]+)$`)
)

// LocationsServer serves google.cloud.location.Locations, the mixin
// cloudscheduler.googleapis.com binds (cloudscheduler_v1.yaml).
type LocationsServer struct {
	locationpb.UnimplementedLocationsServer
	store *Store
}

// NewLocationsServer returns the Locations mixin over the jobs in st.
func NewLocationsServer(st *Store) *LocationsServer { return &LocationsServer{store: st} }

// Register adds the mixin to a gRPC server, or to the JSON transcoder.
func (l *LocationsServer) Register(s grpc.ServiceRegistrar) { locations.Register(s, l) }

// locations is every location of project: the regions, and each location
// holding one of its jobs, sorted.
func (l *LocationsServer) locations(project string) ([]string, error) {
	seen := map[string]bool{}
	for _, r := range Regions {
		seen[r] = true
	}
	jobs, err := l.store.List(project + "/locations/")
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		parent, _, _ := strings.Cut(j.Name, "/jobs/")
		if m := locationRE.FindStringSubmatch(parent); m != nil {
			seen[m[2]] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func location(project, id string) *locationpb.Location {
	return &locationpb.Location{Name: project + "/locations/" + id, LocationId: id, DisplayName: id}
}

// ListLocations lists the project's locations, paged. A filter is refused,
// since none is applied.
func (l *LocationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}", req.GetName()))
	}
	if req.GetFilter() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("ListLocations filter is not supported"))
	}
	ids, err := l.locations(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := paging.Page("scheduler-locations:"+req.GetName(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		resp.Locations = append(resp.Locations, location(req.GetName(), id))
	}
	return resp, nil
}

// GetLocation answers a listed location; any other is NOT_FOUND.
func (l *LocationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := locationRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName()))
	}
	ids, err := l.locations(m[1])
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if i := sort.SearchStrings(ids, m[2]); i < len(ids) && ids[i] == m[2] {
		return location(m[1], m[2]), nil
	}
	return nil, apierror.Wrap(apierror.NotFound("location %q not found", req.GetName()))
}
