package storage

// The Storage Control API's gRPC service, google.storage.control.v2.
// StorageControl, for Anywhere Caches (#1444), and google.longrunning.
// Operations for their operations, on the same h2c port as
// google.storage.v2.Storage. cloud.google.com/go/storage/control/apiv2's
// gRPC client calls them (its REST client has no path for Anywhere Caches).
// As grpc.go does, every call is the JSON API's own handler, served in
// process (anywherecache.go, relocate.go), so the two APIs share one store.
// The IntelligenceConfig methods (#1519) call the store directly
// (intelligence.go), as control.go's REST mapping of them does. The other
// StorageControl methods answer UNIMPLEMENTED: folders and managed folders
// are served on the v2 REST surface (control.go) and the JSON API.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	spb "google.golang.org/genproto/googleapis/rpc/status"
)

type grpcControl struct {
	controlpb.UnimplementedStorageControlServer
	g *grpcStorage
}

type grpcOperations struct {
	longrunningpb.UnimplementedOperationsServer
	g *grpcStorage
}

// controlDo is a JSON API call whose refusal becomes the Storage Control
// API's code: failedPrecondition is FAILED_PRECONDITION, as control.go maps
// it on REST.
func (g *grpcStorage) controlDo(ctx context.Context, method, path string, q url.Values, hdr http.Header, body any, out any) error {
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return grpcstatus.Errorf(codes.Internal, "%v", err)
		}
		raw = b
	}
	rep, err := g.call(ctx, method, path, q, hdr, raw)
	if err != nil {
		return err
	}
	if rep.status >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rep.body, &e)
		code := grpcCodeFor(rep.status)
		if len(e.Error.Errors) > 0 && e.Error.Errors[0].Reason == "failedPrecondition" {
			code = codes.FailedPrecondition
		}
		msg := e.Error.Message
		if msg == "" {
			msg = http.StatusText(rep.status)
		}
		return grpcstatus.Error(code, msg)
	}
	if out != nil && len(rep.body) > 0 {
		if err := json.Unmarshal(rep.body, out); err != nil {
			return grpcstatus.Errorf(codes.Internal, "decoding the JSON API's answer: %v", err)
		}
	}
	return nil
}

// cacheName splits projects/{project}/buckets/{bucket}/anywhereCaches/{id}.
func cacheName(name string) (string, string, error) {
	rest, ok := strings.CutPrefix(name, "projects/")
	if ok {
		if _, r, found := strings.Cut(rest, "/buckets/"); found {
			if b, id, found := strings.Cut(r, "/anywhereCaches/"); found && b != "" && id != "" && !strings.Contains(id, "/") {
				return b, id, nil
			}
		}
	}
	return "", "", grpcstatus.Errorf(codes.InvalidArgument, "name %q is not projects/{project}/buckets/{bucket}/anywhereCaches/{anywhere_cache}", name)
}

// operationPath splits projects/{project}/buckets/{bucket}/operations/{id}.
func operationPath(name string) (string, error) {
	rest, ok := strings.CutPrefix(name, "projects/")
	if ok {
		if _, r, found := strings.Cut(rest, "/buckets/"); found {
			if b, id, found := strings.Cut(r, "/operations/"); found && b != "" && id != "" && !strings.Contains(id, "/") {
				return bucketPath(b) + "/operations/" + url.PathEscape(id), nil
			}
		}
	}
	return "", grpcstatus.Errorf(codes.InvalidArgument, "name %q is not projects/{project}/buckets/{bucket}/operations/{operation}", name)
}

func cachePath(b, id string) string { return bucketPath(b) + "/anywhereCaches/" + url.PathEscape(id) }

func durationSeconds(d *durationpb.Duration) string {
	if d.GetNanos() != 0 {
		return strconv.FormatInt(d.GetSeconds(), 10) + "." + strconv.Itoa(int(d.GetNanos())) + "s"
	}
	return strconv.FormatInt(d.GetSeconds(), 10) + "s"
}

// cacheFromJSON is a JSON API AnywhereCache as the proto.
func cacheFromJSON(m map[string]any) *controlpb.AnywhereCache {
	str := func(k string) string { s, _ := m[k].(string); return s }
	c := &controlpb.AnywhereCache{
		Name: folderResource(str("bucket")) + "/anywhereCaches/" + str("anywhereCacheId"),
		Zone: str("zone"), AdmissionPolicy: str("admissionPolicy"), State: str("state"),
		CreateTime: jsonTime(str("createTime")), UpdateTime: jsonTime(str("updateTime")),
	}
	c.PendingUpdate, _ = m["pendingUpdate"].(bool)
	if b, ok := m["ingestOnWrite"].(bool); ok {
		c.IngestOnWrite = &b
	}
	if n, err := strconv.ParseInt(strings.TrimSuffix(str("ttl"), "s"), 10, 64); err == nil {
		c.Ttl = durationpb.New(time.Duration(n) * time.Second)
	}
	return c
}

func jsonTime(s string) *timestamppb.Timestamp {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

// anyFromJSON is a JSON object with an @type as an Any of that message,
// or nil when the type is not one this binary knows.
func anyFromJSON(m map[string]any) (*anypb.Any, error) {
	t, _ := m["@type"].(string)
	mt, err := protoregistry.GlobalTypes.FindMessageByURL(t)
	if err != nil {
		return nil, nil
	}
	body := map[string]any{}
	for k, v := range m {
		if k != "@type" {
			body[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	msg := mt.New().Interface()
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, msg); err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "%s: %v", t, err)
	}
	return anypb.New(msg)
}

// operationFromJSON is a JSON API operation as a google.longrunning.Operation.
func operationFromJSON(m map[string]any) (*longrunningpb.Operation, error) {
	op := &longrunningpb.Operation{}
	op.Name, _ = m["name"].(string)
	op.Done, _ = m["done"].(bool)
	if md, ok := m["metadata"].(map[string]any); ok {
		a, err := anyFromJSON(md)
		if err != nil {
			return nil, err
		}
		op.Metadata = a
	}
	if e, ok := m["error"].(map[string]any); ok {
		code, _ := e["code"].(float64)
		msg, _ := e["message"].(string)
		op.Result = &longrunningpb.Operation_Error{Error: &spb.Status{Code: int32(code), Message: msg}}
	} else if r, ok := m["response"].(map[string]any); ok {
		a, err := anyFromJSON(r)
		if err != nil {
			return nil, err
		}
		if a == nil {
			a, _ = anypb.New(&emptypb.Empty{})
		}
		op.Result = &longrunningpb.Operation_Response{Response: a}
	}
	return op, nil
}

func (c *grpcControl) CreateAnywhereCache(ctx context.Context, req *controlpb.CreateAnywhereCacheRequest) (*longrunningpb.Operation, error) {
	b, err := bucketID(req.GetParent())
	if err != nil {
		return nil, err
	}
	in := req.GetAnywhereCache()
	body := map[string]any{"zone": in.GetZone()}
	if in.GetTtl() != nil {
		body["ttl"] = durationSeconds(in.GetTtl())
	}
	if in.GetAdmissionPolicy() != "" {
		body["admissionPolicy"] = in.GetAdmissionPolicy()
	}
	if in.IngestOnWrite != nil {
		body["ingestOnWrite"] = in.GetIngestOnWrite()
	}
	hdr := http.Header{}
	if req.GetRequestId() != "" {
		hdr.Set(cacheRequestHeader, req.GetRequestId())
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodPost, bucketPath(b)+"/anywhereCaches", nil, hdr, body, &out); err != nil {
		return nil, err
	}
	return operationFromJSON(out)
}

func (c *grpcControl) UpdateAnywhereCache(ctx context.Context, req *controlpb.UpdateAnywhereCacheRequest) (*longrunningpb.Operation, error) {
	in := req.GetAnywhereCache()
	b, id, err := cacheName(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "update_mask is required")
	}
	body := map[string]any{}
	for _, p := range paths {
		all := p == "*"
		if all || p == "ttl" {
			body["ttl"] = durationSeconds(in.GetTtl())
		}
		if all || p == "admission_policy" {
			body["admissionPolicy"] = in.GetAdmissionPolicy()
		}
		if all || p == "ingest_on_write" {
			body["ingestOnWrite"] = in.GetIngestOnWrite()
		}
		if !all && p != "ttl" && p != "admission_policy" && p != "ingest_on_write" {
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "update_mask path %q is not a mutable AnywhereCache field: ttl, admission_policy and ingest_on_write are", p)
		}
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodPatch, cachePath(b, id), nil, nil, body, &out); err != nil {
		return nil, err
	}
	return operationFromJSON(out)
}

func (c *grpcControl) cacheCall(ctx context.Context, method, name, verb string) (*controlpb.AnywhereCache, error) {
	b, id, err := cacheName(name)
	if err != nil {
		return nil, err
	}
	path := cachePath(b, id)
	if verb != "" {
		path += "/" + verb
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, method, path, nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return cacheFromJSON(out), nil
}

func (c *grpcControl) DisableAnywhereCache(ctx context.Context, req *controlpb.DisableAnywhereCacheRequest) (*controlpb.AnywhereCache, error) {
	return c.cacheCall(ctx, http.MethodPost, req.GetName(), "disable")
}

func (c *grpcControl) PauseAnywhereCache(ctx context.Context, req *controlpb.PauseAnywhereCacheRequest) (*controlpb.AnywhereCache, error) {
	return c.cacheCall(ctx, http.MethodPost, req.GetName(), "pause")
}

func (c *grpcControl) ResumeAnywhereCache(ctx context.Context, req *controlpb.ResumeAnywhereCacheRequest) (*controlpb.AnywhereCache, error) {
	return c.cacheCall(ctx, http.MethodPost, req.GetName(), "resume")
}

func (c *grpcControl) GetAnywhereCache(ctx context.Context, req *controlpb.GetAnywhereCacheRequest) (*controlpb.AnywhereCache, error) {
	return c.cacheCall(ctx, http.MethodGet, req.GetName(), "")
}

func (c *grpcControl) ListAnywhereCaches(ctx context.Context, req *controlpb.ListAnywhereCachesRequest) (*controlpb.ListAnywhereCachesResponse, error) {
	b, err := bucketID(req.GetParent())
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if req.GetPageSize() > 0 {
		q.Set("pageSize", strconv.Itoa(int(req.GetPageSize())))
	}
	if req.GetPageToken() != "" {
		q.Set("pageToken", req.GetPageToken())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"nextPageToken"`
	}
	if err := c.g.controlDo(ctx, http.MethodGet, bucketPath(b)+"/anywhereCaches", q, nil, nil, &out); err != nil {
		return nil, err
	}
	resp := &controlpb.ListAnywhereCachesResponse{NextPageToken: out.Next}
	for _, m := range out.Items {
		resp.AnywhereCaches = append(resp.AnywhereCaches, cacheFromJSON(m))
	}
	return resp, nil
}

func (o *grpcOperations) GetOperation(ctx context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	path, err := operationPath(req.GetName())
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := o.g.controlDo(ctx, http.MethodGet, path, nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return operationFromJSON(out)
}

func (o *grpcOperations) CancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	path, err := operationPath(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := o.g.controlDo(ctx, http.MethodPost, path+"/cancel", nil, nil, nil, nil); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// controlGRPCError is a refusal of the store as the Storage Control API's
// gRPC status, as control.go maps it on REST.
func controlGRPCError(err error) error {
	e, ok := err.(*httpError)
	if !ok {
		return grpcstatus.Errorf(codes.Internal, "%v", err)
	}
	_, s := controlStatus(e)
	code := codes.Internal
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		if strings.EqualFold(strings.ReplaceAll(c.String(), "_", ""), strings.ReplaceAll(s, "_", "")) {
			code = c
		}
	}
	return grpcstatus.Error(code, e.message)
}

// The IntelligenceConfig methods (intelligence.go), one pair per kind.

func (c *grpcControl) getIntelligence(name, kind string) (*controlpb.IntelligenceConfig, error) {
	out, err := c.g.s.GetIntelligenceConfig(name, kind)
	if err != nil {
		return nil, controlGRPCError(err)
	}
	return out, nil
}

func (c *grpcControl) updateIntelligence(in *controlpb.IntelligenceConfig, paths []string, kind string) (*controlpb.IntelligenceConfig, error) {
	out, err := c.g.s.UpdateIntelligenceConfig(in, paths, kind)
	if err != nil {
		return nil, controlGRPCError(err)
	}
	return out, nil
}

func (c *grpcControl) GetProjectIntelligenceConfig(_ context.Context, req *controlpb.GetProjectIntelligenceConfigRequest) (*controlpb.IntelligenceConfig, error) {
	return c.getIntelligence(req.GetName(), "projects")
}

func (c *grpcControl) UpdateProjectIntelligenceConfig(_ context.Context, req *controlpb.UpdateProjectIntelligenceConfigRequest) (*controlpb.IntelligenceConfig, error) {
	return c.updateIntelligence(req.GetIntelligenceConfig(), req.GetUpdateMask().GetPaths(), "projects")
}

func (c *grpcControl) GetFolderIntelligenceConfig(_ context.Context, req *controlpb.GetFolderIntelligenceConfigRequest) (*controlpb.IntelligenceConfig, error) {
	return c.getIntelligence(req.GetName(), "folders")
}

func (c *grpcControl) UpdateFolderIntelligenceConfig(_ context.Context, req *controlpb.UpdateFolderIntelligenceConfigRequest) (*controlpb.IntelligenceConfig, error) {
	return c.updateIntelligence(req.GetIntelligenceConfig(), req.GetUpdateMask().GetPaths(), "folders")
}

func (c *grpcControl) GetOrganizationIntelligenceConfig(_ context.Context, req *controlpb.GetOrganizationIntelligenceConfigRequest) (*controlpb.IntelligenceConfig, error) {
	return c.getIntelligence(req.GetName(), "organizations")
}

func (c *grpcControl) UpdateOrganizationIntelligenceConfig(_ context.Context, req *controlpb.UpdateOrganizationIntelligenceConfigRequest) (*controlpb.IntelligenceConfig, error) {
	return c.updateIntelligence(req.GetIntelligenceConfig(), req.GetUpdateMask().GetPaths(), "organizations")
}
