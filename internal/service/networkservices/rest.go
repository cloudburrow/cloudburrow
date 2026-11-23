package networkservices

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nsvpb "cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves the API, its operations and locations on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	nsvpb.RegisterNetworkServicesServer(g, s)
	nsvpb.RegisterDepServiceServer(g, s)
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{})
}

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

var (
	projectRE  = regexp.MustCompile(`^projects/[^/]+$`)
	locationRE = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)$`)
)

func mkLocation(project, id string) *locationpb.Location {
	return &locationpb.Location{Name: project + "/locations/" + id, LocationId: id}
}

// ListLocations answers global and Compute Engine's regions.
func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	ids, err := aipfilter.LocationIDs(req.GetFilter(), Locations(), func(id string) (*locationpb.Location, error) { return mkLocation(req.GetName(), id), nil })
	if err != nil {
		return nil, err
	}
	page, next, err := paging.Page("locations:"+req.GetName(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		out.Locations = append(out.Locations, mkLocation(req.GetName(), id))
	}
	return out, nil
}

func (l *locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := locationRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	if !validLocation(m[1]) {
		return nil, apierror.NotFound("Location '%s' not found.", req.GetName())
	}
	return mkLocation(req.GetName()[:strings.Index(req.GetName(), "/locations/")], m[1]), nil
}

type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op := o.s.ops[req.GetName()]
	if op == nil {
		return nil, apierror.NotFound("Operation '%s' not found.", req.GetName())
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

func (o *operations) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if o.s.ops[req.GetName()] == nil {
		return nil, apierror.NotFound("Operation '%s' not found.", req.GetName())
	}
	delete(o.s.ops, req.GetName())
	return &emptypb.Empty{}, o.s.saveLocked()
}

// CancelOperation of a done operation does nothing, as on Google.
func (o *operations) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if o.s.ops[req.GetName()] == nil {
		return nil, apierror.NotFound("Operation '%s' not found.", req.GetName())
	}
	return &emptypb.Empty{}, nil
}

func (o *operations) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	if !parentRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	o.s.mu.Lock()
	var names []string
	for k := range o.s.ops {
		if strings.HasPrefix(k, req.GetName()+"/operations/") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	page, next, err := paging.Page("ops:"+req.GetName(), names, req.GetPageToken(), int(req.GetPageSize()))
	out := &longrunningpb.ListOperationsResponse{NextPageToken: next}
	for _, k := range page {
		out.Operations = append(out.Operations, proto.Clone(o.s.ops[k]).(*longrunningpb.Operation))
	}
	o.s.mu.Unlock()
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	return out, nil
}

// mixins are the bindings networkservices_v1.yaml adds: Locations and
// Operations.
var mixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.GetLocation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
	{Selector: "google.longrunning.Operations.CancelOperation",
		Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=projects/*/locations/*/operations/*}:cancel"}, Body: "*"},
	{Selector: "google.longrunning.Operations.DeleteOperation",
		Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}/operations"}},
}

// Mixins are the bindings beyond the API's own.
func Mixins() []*annotations.HttpRule { return mixins }

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.networkservices.v1"}, Mixins: mixins, SnakeCaseMasks: true}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler is the JSON API over s. A Secure Web Proxy gateway's
// JSON also carries where its ports are listened on here, as a
// forwarding rule's does: cloudburrowAddress (the first port's host
// address) and cloudburrowAddresses (PORT=HOST:PORT for each).
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/gateways") {
			t.ServeHTTP(w, r)
			return
		}
		rec := &bufferedWriter{h: http.Header{}, code: http.StatusOK}
		t.ServeHTTP(rec, r)
		body := rec.buf.Bytes()
		if rec.code == http.StatusOK {
			body = s.annotateGateways(body)
		}
		for k, v := range rec.h {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.code)
		_, _ = w.Write(body)
	})
}

type bufferedWriter struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header         { return b.h }
func (b *bufferedWriter) WriteHeader(c int)           { b.code = c }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }

// annotateGateways adds the listener addresses to a gateway or a list of
// them.
func (s *Server) annotateGateways(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	add := func(g map[string]any) {
		name, _ := g["name"].(string)
		addrs := s.GatewayAddresses(name)
		if len(addrs) == 0 {
			return
		}
		var all []any
		for _, a := range addrs {
			all = append(all, a)
		}
		g["cloudburrowAddress"] = addrs[0][strings.Index(addrs[0], "=")+1:]
		g["cloudburrowAddresses"] = all
	}
	if l, ok := m["gateways"].([]any); ok {
		for _, it := range l {
			if g, ok := it.(map[string]any); ok {
				add(g)
			}
		}
	} else if _, ok := m["name"]; ok {
		add(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// Handles is whether a request on the Compute Engine port is this API's:
// a path under one of its collections or one of its operations.
// Certificate Manager, Network Security and Serverless VPC Access serve
// the same /v1/projects/P/locations paths there, and answer the
// locations.
func (s *Server) Handles(r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/v1/projects/") {
		return false
	}
	p := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	if len(p) < 5 || p[2] != "locations" {
		return false
	}
	c := strings.SplitN(p[4], ":", 2)[0]
	if colls[c] {
		return true
	}
	if c == "operations" && len(p) >= 6 {
		return s.HasOperation(strings.Join(p[:5], "/") + "/" + strings.SplitN(p[5], ":", 2)[0])
	}
	return false
}
