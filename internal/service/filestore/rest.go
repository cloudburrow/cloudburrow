package filestore

import (
	"context"
	"net/http"
	"regexp"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"google.golang.org/genproto/googleapis/api/annotations"
	_ "google.golang.org/genproto/googleapis/cloud/common" // OperationMetadata, for the JSON API's Any
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves the API, its operations and its locations on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	filestorepb.RegisterCloudFilestoreManagerServer(g, s)
	NewOperationsServer(s).Register(g)
	locations.Register(g, &locationsServer{})
}

// Locations are the locations Filestore answers: each region and its
// zones.
func Locations() []string {
	var out []string
	for _, r := range locations.Regions {
		out = append(out, r)
		out = append(out, locations.Zones(r)...)
	}
	return out
}

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

var projectRE = regexp.MustCompile(`^projects/[^/]+$`)

func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	mk := func(id string) *locationpb.Location {
		return &locationpb.Location{Name: req.GetName() + "/locations/" + id, LocationId: id}
	}
	ids, err := aipfilter.LocationIDs(req.GetFilter(), Locations(), func(id string) (*locationpb.Location, error) { return mk(id), nil })
	if err != nil {
		return nil, err
	}
	page, next, err := paging.Page("locations:"+req.GetName(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		out.Locations = append(out.Locations, mk(id))
	}
	return out, nil
}

func (l *locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := parentRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	if !validLocation(m[2]) {
		return nil, apierror.NotFound("Location '%s' was not found", req.GetName())
	}
	return &locationpb.Location{Name: req.GetName(), LocationId: m[2]}, nil
}

// Mixins are the bindings file_v1.yaml adds: Locations and Operations.
func Mixins() []*annotations.HttpRule {
	return []*annotations.HttpRule{
		{Selector: "google.cloud.location.Locations.GetLocation",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
		{Selector: "google.cloud.location.Locations.ListLocations",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
		{Selector: "google.longrunning.Operations.CancelOperation",
			Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=projects/*/locations/*/operations/*}:cancel"}},
		{Selector: "google.longrunning.Operations.DeleteOperation",
			Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*/operations/*}"}},
		{Selector: "google.longrunning.Operations.GetOperation",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
		{Selector: "google.longrunning.Operations.ListOperations",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}/operations"}},
	}
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.filestore.v1"}, Mixins: Mixins(), SnakeCaseMasks: true, Overlay: filestoreOverlay}
}

// NewRESTHandler is the JSON API over s.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return t
}
