package datatransfer

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The JSON API shares the gRPC port, as bigquerydatatransfer.googleapis.com
// serves both. Its routes are the google.api.http bindings of
// google.cloud.bigquery.datatransfer.v1, read from the generated
// descriptors, plus the Locations bindings bigquerydatatransfer_v1.yaml
// adds; the transcoder calls the same server gRPC does.
var mixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.GetLocation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
}

// Transcoder returns the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.bigquery.datatransfer.v1"}, Mixins: mixins}
}

// Routes returns every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler returns the JSON API for what register adds.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	register(t)
	return t
}

// Locations are BigQuery's multi-regions and Google Cloud's regions.
var Locations = append([]string{"eu", "us"}, locations.Regions...)

// LocationsServer serves google.cloud.location.Locations.
type LocationsServer struct {
	locationpb.UnimplementedLocationsServer
}

// Register adds the mixin to a gRPC server, or to the JSON transcoder.
func (l *LocationsServer) Register(s grpc.ServiceRegistrar) { locations.Register(s, l) }

func sortedLocations() []string {
	ids := append([]string(nil), Locations...)
	sort.Strings(ids)
	return ids
}

func location(project, id string) *locationpb.Location {
	return &locationpb.Location{Name: project + "/locations/" + id, LocationId: id, DisplayName: id}
}

// ListLocations lists the locations, paged. A filter is refused.
func (l *LocationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetName())
	if m == nil || m[2] != "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}", req.GetName()))
	}
	if req.GetFilter() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("ListLocations filter is not supported"))
	}
	page, next, err := paging.Page("dts-locations:"+req.GetName(), sortedLocations(), req.GetPageToken(), int(req.GetPageSize()))
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
	m := parentRE.FindStringSubmatch(req.GetName())
	if m == nil || m[2] == "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName()))
	}
	for _, id := range Locations {
		if id == m[2] {
			return location(strings.TrimSuffix(req.GetName(), "/locations/"+id), id), nil
		}
	}
	return nil, apierror.Wrap(apierror.NotFound("location %q not found", req.GetName()))
}
