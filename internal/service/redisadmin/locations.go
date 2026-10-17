package redisadmin

import (
	"context"
	"regexp"
	"sort"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// LocationsServer serves google.cloud.location.Locations, the mixin
// redis_v1.yaml binds (#1386): the regions every project has (the list
// Cloud Scheduler answers, locations.Regions), and any region holding one
// of the project's instances, since any is accepted. Each carries
// google.cloud.redis.v1.LocationMetadata with zones a, b and c; which
// zones Google lists per region is UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/redis/v1/cloud_redis.proto
type LocationsServer struct {
	locationpb.UnimplementedLocationsServer
	api *Server
}

// NewLocationsServer returns the Locations mixin of api.
func NewLocationsServer(api *Server) *LocationsServer { return &LocationsServer{api: api} }

// Register adds the mixin to a gRPC server, or to the JSON transcoder.
func (l *LocationsServer) Register(g grpc.ServiceRegistrar) { locations.Register(g, l) }

var (
	projectNameRE  = regexp.MustCompile(`^projects/([^/]+)$`)
	locationNameRE = regexp.MustCompile(`^projects/([^/]+)/locations/([a-z0-9-]+)$`)
)

func (l *LocationsServer) regions(ctx context.Context, project string) []string {
	seen := map[string]bool{}
	for _, r := range locations.Regions {
		seen[r] = true
	}
	seen[Region] = true
	if l.api.created != nil {
		for _, inst := range l.api.created.list(ctx, project, "-") {
			seen[instanceRE.FindStringSubmatch(inst.GetName())[2]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func location(project, region string) *locationpb.Location {
	md := &redispb.LocationMetadata{AvailableZones: map[string]*redispb.ZoneMetadata{}}
	for _, z := range []string{"a", "b", "c"} {
		md.AvailableZones[region+"-"+z] = &redispb.ZoneMetadata{}
	}
	loc := &locationpb.Location{Name: "projects/" + project + "/locations/" + region, LocationId: region}
	if a, err := anypb.New(md); err == nil {
		loc.Metadata = a
	}
	return loc
}

// ListLocations lists a project's locations, in name order and paged.
func (l *LocationsServer) ListLocations(ctx context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	m := projectNameRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name must be projects/{project}, not %q", req.GetName())
	}
	if req.GetFilter() != "" {
		return nil, apierror.Unimplemented("filtering locations is not supported")
	}
	if req.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	page, next, err := paging.Page("redis-locations:"+req.GetName(), l.regions(ctx, m[1]), req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, r := range page {
		resp.Locations = append(resp.Locations, location(m[1], r))
	}
	return resp, nil
}

// GetLocation returns one of a project's locations, NOT_FOUND for another.
func (l *LocationsServer) GetLocation(ctx context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := locationNameRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}, not %q", req.GetName())
	}
	for _, r := range l.regions(ctx, m[1]) {
		if r == m[2] {
			return location(m[1], r), nil
		}
	}
	return nil, apierror.NotFound("Location %s not found", req.GetName())
}
