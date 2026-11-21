package logging

// The Observability API over gRPC (#2117): google.cloud.observability.v1's
// Observability service (googleapis google/cloud/observability/v1/
// observability.proto, which cloud.google.com/go/observability/apiv1's
// NewClient calls), with the google.cloud.location.Locations and
// google.longrunning.Operations mixins its service config names. Each
// method is its REST binding (the proto's google.api.http), served by the
// same handlers as REST (observability.go, tracescopes.go and
// obsresources.go), so the two transports never disagree.
//
// The Logging port serves google.longrunning.Operations for Cloud
// Logging's copy operations too, under names of the same shape: on gRPC a
// name Cloud Logging does not hold is the Observability API's, and a list
// is both services' operations, Cloud Logging's first. UNVERIFIED (docs
// re-read 2026-11-21: https://docs.cloud.google.com/stackdriver/docs/
// observability/set-defaults-for-observability-buckets): on Google the two
// are apart, at logging.googleapis.com and observability.googleapis.com.

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// RegisterGRPC adds what Register does, the Observability API and its
// Locations mixin, and an Operations service over both Cloud Logging's
// and the Observability API's operations, to the Logging port's gRPC
// server. The JSON transcoder keeps Register: the Observability API's REST
// is served apart (ServeObservability).
func (a *Admin) RegisterGRPC(g *grpc.Server) {
	loggingpb.RegisterMetricsServiceV2Server(g, metricsServer{a})
	loggingpb.RegisterConfigServiceV2Server(g, configServer{a: a})
	obspb.RegisterObservabilityServer(g, obsGRPC{a: a})
	locationpb.RegisterLocationsServer(g, obsLocationsGRPC{a: a})
	longrunningpb.RegisterOperationsServer(g, mergedOperations{a: a, logging: operationsServer{a: a}})
}

// obsCall serves one Observability method through its REST binding.
func (a *Admin) obsCall(method, path string, query url.Values, body proto.Message, out proto.Message) error {
	var raw []byte
	if body != nil {
		b, err := protojson.Marshal(body)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "encoding the request: %v", err)
		}
		raw = b
	}
	target := "/v1/" + strings.TrimPrefix(path, "/")
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	code, resp := a.CallObservability(method, target, raw)
	if code != http.StatusOK {
		_, msg, st, _, err := apierror.DecodeStatusJSON(bytes.NewReader(resp))
		if err != nil {
			return status.Errorf(codes.Internal, "HTTP %d: %s", code, resp)
		}
		return status.Error(codeOf(st, code), msg)
	}
	if out == nil {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(resp, out); err != nil {
		return status.Errorf(codes.Internal, "decoding the response: %v", err)
	}
	return nil
}

// codeOf is the canonical code a REST error's status names, else the
// HTTP status's.
func codeOf(name string, httpCode int) codes.Code {
	norm := func(s string) string { return strings.ToUpper(strings.ReplaceAll(s, "_", "")) }
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		if norm(c.String()) == norm(name) {
			return c
		}
	}
	switch httpCode {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.AlreadyExists
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	}
	return codes.Internal
}

// pageQuery is a list request's page_size and page_token.
func pageQuery(size int32, token string) url.Values {
	q := url.Values{}
	if size != 0 {
		q.Set("pageSize", strconv.Itoa(int(size)))
	}
	if token != "" {
		q.Set("pageToken", token)
	}
	return q
}

// maskQuery adds an update_mask, as REST takes it.
func maskQuery(q url.Values, m *fieldmaskpb.FieldMask) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if len(m.GetPaths()) > 0 {
		q.Set("updateMask", strings.Join(m.GetPaths(), ","))
	}
	return q
}

// need is INVALID_ARGUMENT for a missing name in a request.
func need(field, v string) error {
	if v == "" {
		return status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	return nil
}

type obsGRPC struct {
	obspb.UnimplementedObservabilityServer
	a *Admin
}

func (s obsGRPC) GetScope(_ context.Context, r *obspb.GetScopeRequest) (*obspb.Scope, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &obspb.Scope{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsGRPC) UpdateScope(_ context.Context, r *obspb.UpdateScopeRequest) (*obspb.Scope, error) {
	if err := need("scope.name", r.GetScope().GetName()); err != nil {
		return nil, err
	}
	out := &obspb.Scope{}
	return out, s.a.obsCall(http.MethodPatch, r.GetScope().GetName(), maskQuery(nil, r.GetUpdateMask()), r.GetScope(), out)
}

func (s obsGRPC) GetBucket(_ context.Context, r *obspb.GetBucketRequest) (*obspb.Bucket, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &obspb.Bucket{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsGRPC) ListBuckets(_ context.Context, r *obspb.ListBucketsRequest) (*obspb.ListBucketsResponse, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	q := pageQuery(r.GetPageSize(), r.GetPageToken())
	if r.GetShowDeleted() {
		q.Set("showDeleted", "true")
	}
	out := &obspb.ListBucketsResponse{}
	return out, s.a.obsCall(http.MethodGet, r.GetParent()+"/buckets", q, nil, out)
}

func (s obsGRPC) CreateBucket(_ context.Context, r *obspb.CreateBucketRequest) (*longrunningpb.Operation, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	out := &longrunningpb.Operation{}
	return out, s.a.obsCall(http.MethodPost, r.GetParent()+"/buckets", url.Values{"bucketId": {r.GetBucketId()}}, bucketOrEmpty(r.GetBucket()), out)
}

func bucketOrEmpty(b *obspb.Bucket) *obspb.Bucket {
	if b == nil {
		return &obspb.Bucket{}
	}
	return b
}

func (s obsGRPC) UpdateBucket(_ context.Context, r *obspb.UpdateBucketRequest) (*longrunningpb.Operation, error) {
	if err := need("bucket.name", r.GetBucket().GetName()); err != nil {
		return nil, err
	}
	out := &longrunningpb.Operation{}
	return out, s.a.obsCall(http.MethodPatch, r.GetBucket().GetName(), maskQuery(nil, r.GetUpdateMask()), r.GetBucket(), out)
}

func (s obsGRPC) GetDataset(_ context.Context, r *obspb.GetDatasetRequest) (*obspb.Dataset, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &obspb.Dataset{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsGRPC) ListDatasets(_ context.Context, r *obspb.ListDatasetsRequest) (*obspb.ListDatasetsResponse, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	out := &obspb.ListDatasetsResponse{}
	return out, s.a.obsCall(http.MethodGet, r.GetParent()+"/datasets", pageQuery(r.GetPageSize(), r.GetPageToken()), nil, out)
}

func (s obsGRPC) GetView(_ context.Context, r *obspb.GetViewRequest) (*obspb.View, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &obspb.View{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsGRPC) ListViews(_ context.Context, r *obspb.ListViewsRequest) (*obspb.ListViewsResponse, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	out := &obspb.ListViewsResponse{}
	return out, s.a.obsCall(http.MethodGet, r.GetParent()+"/views", pageQuery(r.GetPageSize(), r.GetPageToken()), nil, out)
}

func (s obsGRPC) GetLink(_ context.Context, r *obspb.GetLinkRequest) (*obspb.Link, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &obspb.Link{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsGRPC) ListLinks(_ context.Context, r *obspb.ListLinksRequest) (*obspb.ListLinksResponse, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	out := &obspb.ListLinksResponse{}
	return out, s.a.obsCall(http.MethodGet, r.GetParent()+"/links", pageQuery(r.GetPageSize(), r.GetPageToken()), nil, out)
}

func (s obsGRPC) CreateLink(_ context.Context, r *obspb.CreateLinkRequest) (*longrunningpb.Operation, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	link := r.GetLink()
	if link == nil {
		link = &obspb.Link{}
	}
	out := &longrunningpb.Operation{}
	return out, s.a.obsCall(http.MethodPost, r.GetParent()+"/links", url.Values{"linkId": {r.GetLinkId()}}, link, out)
}

func (s obsGRPC) UpdateLink(_ context.Context, r *obspb.UpdateLinkRequest) (*longrunningpb.Operation, error) {
	if err := need("link.name", r.GetLink().GetName()); err != nil {
		return nil, err
	}
	out := &longrunningpb.Operation{}
	return out, s.a.obsCall(http.MethodPatch, r.GetLink().GetName(), maskQuery(nil, r.GetUpdateMask()), r.GetLink(), out)
}

func (s obsGRPC) DeleteLink(_ context.Context, r *obspb.DeleteLinkRequest) (*longrunningpb.Operation, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &longrunningpb.Operation{}
	return out, s.a.obsCall(http.MethodDelete, r.GetName(), nil, nil, out)
}

func (s obsGRPC) GetTraceScope(_ context.Context, r *obspb.GetTraceScopeRequest) (*obspb.TraceScope, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &obspb.TraceScope{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsGRPC) ListTraceScopes(_ context.Context, r *obspb.ListTraceScopesRequest) (*obspb.ListTraceScopesResponse, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	out := &obspb.ListTraceScopesResponse{}
	return out, s.a.obsCall(http.MethodGet, r.GetParent()+"/traceScopes", pageQuery(r.GetPageSize(), r.GetPageToken()), nil, out)
}

func (s obsGRPC) CreateTraceScope(_ context.Context, r *obspb.CreateTraceScopeRequest) (*obspb.TraceScope, error) {
	if err := need("parent", r.GetParent()); err != nil {
		return nil, err
	}
	ts := r.GetTraceScope()
	if ts == nil {
		ts = &obspb.TraceScope{}
	}
	out := &obspb.TraceScope{}
	return out, s.a.obsCall(http.MethodPost, r.GetParent()+"/traceScopes", url.Values{"traceScopeId": {r.GetTraceScopeId()}}, ts, out)
}

func (s obsGRPC) UpdateTraceScope(_ context.Context, r *obspb.UpdateTraceScopeRequest) (*obspb.TraceScope, error) {
	if err := need("trace_scope.name", r.GetTraceScope().GetName()); err != nil {
		return nil, err
	}
	out := &obspb.TraceScope{}
	return out, s.a.obsCall(http.MethodPatch, r.GetTraceScope().GetName(), maskQuery(nil, r.GetUpdateMask()), r.GetTraceScope(), out)
}

func (s obsGRPC) DeleteTraceScope(_ context.Context, r *obspb.DeleteTraceScopeRequest) (*emptypb.Empty, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, s.a.obsCall(http.MethodDelete, r.GetName(), nil, nil, nil)
}

func (s obsGRPC) GetSettings(_ context.Context, r *obspb.GetSettingsRequest) (*obspb.Settings, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &obspb.Settings{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsGRPC) UpdateSettings(_ context.Context, r *obspb.UpdateSettingsRequest) (*longrunningpb.Operation, error) {
	if err := need("settings.name", r.GetSettings().GetName()); err != nil {
		return nil, err
	}
	out := &longrunningpb.Operation{}
	return out, s.a.obsCall(http.MethodPatch, r.GetSettings().GetName(), maskQuery(nil, r.GetUpdateMask()), r.GetSettings(), out)
}

// obsLocationsGRPC is the Locations mixin: the Observability API's
// locations of a project, folder or organization.
type obsLocationsGRPC struct {
	locationpb.UnimplementedLocationsServer
	a *Admin
}

func (s obsLocationsGRPC) GetLocation(_ context.Context, r *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	out := &locationpb.Location{}
	return out, s.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (s obsLocationsGRPC) ListLocations(_ context.Context, r *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if err := need("name", r.GetName()); err != nil {
		return nil, err
	}
	q := pageQuery(r.GetPageSize(), r.GetPageToken())
	if r.GetFilter() != "" {
		q.Set("filter", r.GetFilter())
	}
	out := &locationpb.ListLocationsResponse{}
	return out, s.a.obsCall(http.MethodGet, r.GetName()+"/locations", q, nil, out)
}

// mergedOperations is google.longrunning.Operations on the Logging port's
// gRPC: Cloud Logging's copy and done operations, then the Observability
// API's.
type mergedOperations struct {
	longrunningpb.UnimplementedOperationsServer
	a       *Admin
	logging operationsServer
}

func isNotFound(err error) bool { return status.Code(err) == codes.NotFound }

func (m mergedOperations) GetOperation(ctx context.Context, r *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	op, err := m.logging.GetOperation(ctx, r)
	if err == nil || !isNotFound(err) {
		return op, err
	}
	out := &longrunningpb.Operation{}
	return out, m.a.obsCall(http.MethodGet, r.GetName(), nil, nil, out)
}

func (m mergedOperations) CancelOperation(ctx context.Context, r *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	e, err := m.logging.CancelOperation(ctx, r)
	if err == nil || !isNotFound(err) {
		return e, err
	}
	return &emptypb.Empty{}, m.a.obsCall(http.MethodPost, r.GetName()+":cancel", nil, nil, nil)
}

func (m mergedOperations) DeleteOperation(_ context.Context, r *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, m.a.obsCall(http.MethodDelete, r.GetName(), nil, nil, nil)
}

func (m mergedOperations) WaitOperation(ctx context.Context, r *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	return m.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: r.GetName()})
}

// ListOperations: request_type= is Cloud Logging's filter and done= the
// Observability API's; with neither, both services' operations are listed.
func (m mergedOperations) ListOperations(ctx context.Context, r *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	f := strings.ReplaceAll(r.GetFilter(), " ", "")
	switch {
	case strings.HasPrefix(f, "request_type") || strings.HasPrefix(f, "requestType"):
		return m.logging.ListOperations(ctx, r)
	case strings.HasPrefix(f, "done"):
		return m.obsList(r.GetName(), f, r.GetPageSize(), r.GetPageToken())
	case f != "":
		return nil, status.Errorf(codes.InvalidArgument, "filter %q: only request_type=CopyLogEntries, done=true and done=false are supported", r.GetFilter())
	}
	var all []*longrunningpb.Operation
	for token := ""; ; {
		resp, err := m.logging.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: r.GetName(), PageSize: 100, PageToken: token})
		if err != nil {
			return nil, err
		}
		all = append(all, resp.GetOperations()...)
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	for token := ""; ; {
		resp, err := m.obsList(r.GetName(), "", 100, token)
		if err != nil && !isNotFound(err) {
			return nil, err
		}
		all = append(all, resp.GetOperations()...)
		if token = resp.GetNextPageToken(); err != nil || token == "" {
			break
		}
	}
	scope := "merged-operations|" + r.GetName()
	after, err := paging.DecodeToken(scope, r.GetPageToken())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "page_token: %v", err)
	}
	start := 0
	if after != "" {
		start = len(all)
		for i, o := range all {
			if o.GetName() == after {
				start = i + 1
				break
			}
		}
	}
	size := int(r.GetPageSize())
	if size <= 0 || size > 100 {
		size = 100
	}
	end := min(start+size, len(all))
	out := &longrunningpb.ListOperationsResponse{Operations: all[start:end]}
	if end < len(all) {
		out.NextPageToken = paging.EncodeToken(scope, all[end-1].GetName())
	}
	return out, nil
}

func (m mergedOperations) obsList(name, filter string, size int32, token string) (*longrunningpb.ListOperationsResponse, error) {
	q := pageQuery(size, token)
	if filter != "" {
		q.Set("filter", filter)
	}
	out := &longrunningpb.ListOperationsResponse{}
	if err := m.a.obsCall(http.MethodGet, name+"/operations", q, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}
