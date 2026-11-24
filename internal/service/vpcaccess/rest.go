package vpcaccess

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves the API, its operations and locations on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	vpcaccesspb.RegisterVpcAccessServiceServer(g, s)
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{})
}

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

var projectRE = regexp.MustCompile(`^projects/[^/]+$`)

// ListLocations answers the regions Compute Engine serves: a connector is
// in a region of its network.
func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	mk := func(id string) *locationpb.Location {
		return &locationpb.Location{Name: req.GetName() + "/locations/" + id, LocationId: id}
	}
	ids, err := aipfilter.LocationIDs(req.GetFilter(), locations.ComputeRegions(), func(id string) (*locationpb.Location, error) { return mk(id), nil })
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

// mixins are the bindings vpcaccess_v1.yaml adds: Locations.ListLocations
// and Operations.GetOperation and ListOperations.
var mixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}/operations"}},
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.vpcaccess.v1"}, Mixins: mixins, SnakeCaseMasks: true}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler is the JSON API over s, with connectors.patch, which the
// proto does not bind (update.go).
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.servePatch(w, r) {
			return
		}
		t.ServeHTTP(w, r)
	})
}

// Handles is whether path is one of this API's (/v1/projects/P/locations
// and below), which the Compute Engine port serves beside /compute/v1/.
func Handles(path string) bool {
	if !strings.HasPrefix(path, "/v1/projects/") {
		return false
	}
	p := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	return len(p) >= 3 && p[2] == "locations"
}
