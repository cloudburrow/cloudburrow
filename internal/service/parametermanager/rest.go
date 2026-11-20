package parametermanager

import (
	"context"
	"net/http"
	"regexp"

	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves the API and its locations on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	pmpb.RegisterParameterManagerServer(g, s)
	locations.Register(g, &locationsServer{})
}

// Locations are the locations Parameter Manager answers: global, and the
// regions.
func Locations() []string { return append([]string{"global"}, locations.Regions...) }

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

var projectRE = regexp.MustCompile(`^projects/[^/]+$`)

func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	mk := func(id string) *locationpb.Location {
		return &locationpb.Location{Name: req.GetName() + "/locations/" + id, LocationId: id, DisplayName: id}
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
	return &locationpb.Location{Name: req.GetName(), LocationId: m[2], DisplayName: m[2]}, nil
}

// Mixins are the bindings parametermanager_v1.yaml adds: Locations.
func Mixins() []*annotations.HttpRule {
	return []*annotations.HttpRule{
		{Selector: "google.cloud.location.Locations.GetLocation",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
		{Selector: "google.cloud.location.Locations.ListLocations",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
	}
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.parametermanager.v1"}, Mixins: Mixins(), SnakeCaseMasks: true}
}

// NewRESTHandler is the JSON API over s.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return t
}
