package storage

// The gRPC API, google.storage.v2.Storage (#1321), on the JSON API's port
// as h2c, as storage.googleapis.com serves both on one host. Every method
// is answered by the JSON API's own handler: the gRPC request becomes the
// JSON request that means the same thing, served in process, and its answer
// becomes the gRPC response. So the two APIs share one store and agree by
// construction, on preconditions, generations, checksums, soft delete and
// retention alike. A method or field the JSON API cannot serve faithfully
// answers UNIMPLEMENTED, naming it.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

// grpcChunk is the most data one ReadObject or BidiReadObject message
// carries, as Cloud Storage sends (2 MiB).
const grpcChunk = 2 << 20

// grpcStorage serves google.storage.v2.Storage from s.
type grpcStorage struct {
	pb.UnimplementedStorageServer
	s *Server
}

// newGRPCServer returns the gRPC server for s.
func newGRPCServer(s *Server) *grpc.Server {
	gs := grpc.NewServer(grpc.MaxRecvMsgSize(64 << 20))
	pb.RegisterStorageServer(gs, &grpcStorage{s: s})
	return gs
}

// isGRPC reports whether r is a gRPC call: HTTP/2 with a gRPC content type.
func isGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// --- the in-process JSON call -------------------------------------------

// jsonReply is a JSON API answer.
type jsonReply struct {
	status int
	header http.Header
	body   []byte
}

// grpcRecorder is the in-process ResponseWriter.
type grpcRecorder struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (r *grpcRecorder) Header() http.Header { return r.header }
func (r *grpcRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}
func (r *grpcRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.buf.Write(b)
}

// call serves one JSON API request in process. path is escaped; body, when
// not nil, is JSON or media.
func (g *grpcStorage) call(ctx context.Context, method, path string, q url.Values, hdr http.Header, body []byte) (*jsonReply, error) {
	u := "http://localhost" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "%v", err)
	}
	req.RemoteAddr = "127.0.0.1:0"
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// Stored bytes are served as stored: gRPC never transcodes.
	req.Header.Set("Accept-Encoding", "gzip")
	rec := &grpcRecorder{header: http.Header{}}
	g.s.route(rec, req)
	g.s.wakeNotifier()
	g.s.wakeChannels()
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return &jsonReply{status: rec.status, header: rec.header, body: rec.buf.Bytes()}, nil
}

// do is call for a request answered with JSON: a JSON error becomes a gRPC
// status, and a success is decoded into out (when not nil).
func (g *grpcStorage) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return grpcstatus.Errorf(codes.Internal, "%v", err)
		}
		raw = b
	}
	rep, err := g.call(ctx, method, path, q, nil, raw)
	if err != nil {
		return err
	}
	if err := replyError(rep); err != nil {
		return err
	}
	if out != nil && len(bytes.TrimSpace(rep.body)) > 0 {
		if err := json.Unmarshal(rep.body, out); err != nil {
			return grpcstatus.Errorf(codes.Internal, "decoding the JSON API's answer: %v", err)
		}
	}
	return nil
}

// replyError is the gRPC status a JSON API error answer means, or nil.
func replyError(rep *jsonReply) error {
	if rep.status < 300 || rep.status == http.StatusPermanentRedirect {
		return nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(rep.body))
	if json.Unmarshal(rep.body, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(rep.status)
	}
	return grpcstatus.Error(grpcCodeFor(rep.status), msg)
}

// grpcCodeFor is the gRPC code for a JSON API status, as Cloud Storage maps
// them.
func grpcCodeFor(st int) codes.Code {
	switch st {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound, http.StatusGone:
		return codes.NotFound
	case http.StatusConflict:
		return codes.AlreadyExists
	case http.StatusNotModified, http.StatusPreconditionFailed:
		return codes.FailedPrecondition
	case http.StatusRequestedRangeNotSatisfiable:
		return codes.OutOfRange
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case 499:
		return codes.Canceled
	case http.StatusRequestEntityTooLarge:
		return codes.ResourceExhausted
	}
	return codes.Internal
}

// --- names and parameters -------------------------------------------------

// bucketID is a v2 bucket name's bucket: projects/{project}/buckets/{bucket}.
func bucketID(name string) (string, error) {
	rest, ok := strings.CutPrefix(name, "projects/")
	if ok {
		if _, b, found := strings.Cut(rest, "/buckets/"); found && b != "" && !strings.Contains(b, "/") {
			return b, nil
		}
	}
	return "", grpcstatus.Errorf(codes.InvalidArgument, "bucket %q is not projects/{project}/buckets/{bucket}", name)
}

// bucketName is a bucket's v2 name.
func bucketName(id string) string { return "projects/_/buckets/" + id }

func projectID(parent string) (string, error) {
	p, ok := strings.CutPrefix(parent, "projects/")
	if !ok || p == "" || strings.Contains(p, "/") {
		return "", grpcstatus.Errorf(codes.InvalidArgument, "parent %q is not projects/{project}", parent)
	}
	return p, nil
}

func bucketPath(b string) string { return jsonPrefix + "b/" + url.PathEscape(b) }
func objectPath(b, o string) string {
	return bucketPath(b) + "/o/" + url.PathEscape(o)
}

// pre adds a precondition to q under name, when set.
func pre(q url.Values, name string, v *int64) {
	if v != nil {
		q.Set(name, strconv.FormatInt(*v, 10))
	}
}

func setIf(q url.Values, name, v string) {
	if v != "" {
		q.Set(name, v)
	}
}

func setGen(q url.Values, name string, g int64) {
	if g != 0 {
		q.Set(name, strconv.FormatInt(g, 10))
	}
}

// noCSEK refuses customer-supplied encryption keys, which the JSON API
// does not serve.
func noCSEK(p *pb.CommonObjectRequestParams) error {
	if p != nil && (p.GetEncryptionAlgorithm() != "" || len(p.GetEncryptionKeyBytes()) > 0) {
		return grpcstatus.Error(codes.Unimplemented, "customer-supplied encryption keys are not implemented")
	}
	return nil
}

// --- buckets ----------------------------------------------------------------

func (g *grpcStorage) CreateBucket(ctx context.Context, req *pb.CreateBucketRequest) (*pb.Bucket, error) {
	project, err := projectID(req.GetParent())
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if req.GetBucket() != nil {
		if body, err = bucketToJSON(req.GetBucket(), nil); err != nil {
			return nil, err
		}
	}
	if b := req.GetBucket().GetProject(); b != "" {
		if project, err = projectID(b); err != nil {
			return nil, err
		}
	}
	body["name"] = req.GetBucketId()
	q := url.Values{"project": {project}, "projection": {"full"}}
	setIf(q, "predefinedAcl", req.GetPredefinedAcl())
	setIf(q, "predefinedDefaultObjectAcl", req.GetPredefinedDefaultObjectAcl())
	if req.GetEnableObjectRetention() {
		q.Set("enableObjectRetention", "true")
	}
	var out jsonBucket
	if err := g.do(ctx, http.MethodPost, jsonPrefix+"b", q, body, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) GetBucket(ctx context.Context, req *pb.GetBucketRequest) (*pb.Bucket, error) {
	b, err := bucketID(req.GetName())
	if err != nil {
		return nil, err
	}
	q := url.Values{"projection": {"full"}}
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	var out jsonBucket
	if err := g.do(ctx, http.MethodGet, bucketPath(b), q, nil, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) ListBuckets(ctx context.Context, req *pb.ListBucketsRequest) (*pb.ListBucketsResponse, error) {
	project, err := projectID(req.GetParent())
	if err != nil {
		return nil, err
	}
	q := url.Values{"project": {project}, "projection": {"full"}}
	setIf(q, "prefix", req.GetPrefix())
	setIf(q, "pageToken", req.GetPageToken())
	if req.GetPageSize() > 0 {
		q.Set("maxResults", strconv.Itoa(int(req.GetPageSize())))
	}
	var out struct {
		Items         []jsonBucket
		NextPageToken string
	}
	if err := g.do(ctx, http.MethodGet, jsonPrefix+"b", q, nil, &out); err != nil {
		return nil, err
	}
	resp := &pb.ListBucketsResponse{NextPageToken: out.NextPageToken}
	for _, b := range out.Items {
		resp.Buckets = append(resp.Buckets, b.proto())
	}
	return resp, nil
}

func (g *grpcStorage) DeleteBucket(ctx context.Context, req *pb.DeleteBucketRequest) (*emptypb.Empty, error) {
	b, err := bucketID(req.GetName())
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	if err := g.do(ctx, http.MethodDelete, bucketPath(b), q, nil, nil); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (g *grpcStorage) UpdateBucket(ctx context.Context, req *pb.UpdateBucketRequest) (*pb.Bucket, error) {
	b, err := bucketID(req.GetBucket().GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "update_mask is required")
	}
	body, err := bucketToJSON(req.GetBucket(), paths)
	if err != nil {
		return nil, err
	}
	q := url.Values{"projection": {"full"}}
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	setIf(q, "predefinedAcl", req.GetPredefinedAcl())
	setIf(q, "predefinedDefaultObjectAcl", req.GetPredefinedDefaultObjectAcl())
	var out jsonBucket
	if err := g.do(ctx, http.MethodPatch, bucketPath(b), q, body, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) LockBucketRetentionPolicy(ctx context.Context, req *pb.LockBucketRetentionPolicyRequest) (*pb.Bucket, error) {
	b, err := bucketID(req.GetBucket())
	if err != nil {
		return nil, err
	}
	q := url.Values{"ifMetagenerationMatch": {strconv.FormatInt(req.GetIfMetagenerationMatch(), 10)}}
	var out jsonBucket
	if err := g.do(ctx, http.MethodPost, bucketPath(b)+"/lockRetentionPolicy", q, nil, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

// iamBucket is the bucket an IAM resource names. Managed folders' policies
// (projects/_/buckets/b/managedFolders/f) are not served over gRPC.
func iamBucket(resource string) (string, error) {
	b, err := bucketID(resource)
	if err != nil {
		if strings.Contains(resource, "/managedFolders/") {
			return "", grpcstatus.Error(codes.Unimplemented, "managed folder IAM policies are not implemented over gRPC")
		}
		return "", err
	}
	return b, nil
}

type jsonPolicy struct {
	Version  int32  `json:"version,omitempty"`
	Etag     string `json:"etag,omitempty"`
	Bindings []struct {
		Role      string   `json:"role"`
		Members   []string `json:"members"`
		Condition *struct {
			Title       string `json:"title,omitempty"`
			Description string `json:"description,omitempty"`
			Expression  string `json:"expression"`
		} `json:"condition,omitempty"`
	} `json:"bindings,omitempty"`
}

func (p jsonPolicy) proto() *iampb.Policy {
	out := &iampb.Policy{Version: p.Version}
	out.Etag, _ = base64.StdEncoding.DecodeString(p.Etag)
	for _, b := range p.Bindings {
		pb := &iampb.Binding{Role: b.Role, Members: b.Members}
		if c := b.Condition; c != nil {
			pb.Condition = &expr.Expr{Title: c.Title, Description: c.Description, Expression: c.Expression}
		}
		out.Bindings = append(out.Bindings, pb)
	}
	return out
}

func (g *grpcStorage) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	b, err := iamBucket(req.GetResource())
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if v := req.GetOptions().GetRequestedPolicyVersion(); v != 0 {
		q.Set("optionsRequestedPolicyVersion", strconv.Itoa(int(v)))
	}
	var out jsonPolicy
	if err := g.do(ctx, http.MethodGet, bucketPath(b)+"/iam", q, nil, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	b, err := iamBucket(req.GetResource())
	if err != nil {
		return nil, err
	}
	if req.GetUpdateMask() != nil {
		return nil, grpcstatus.Error(codes.Unimplemented, "SetIamPolicy update_mask is not implemented")
	}
	p := req.GetPolicy()
	body := map[string]any{"version": p.GetVersion()}
	if len(p.GetEtag()) > 0 {
		body["etag"] = base64.StdEncoding.EncodeToString(p.GetEtag())
	}
	var bindings []map[string]any
	for _, bd := range p.GetBindings() {
		m := map[string]any{"role": bd.GetRole(), "members": bd.GetMembers()}
		if c := bd.GetCondition(); c != nil {
			m["condition"] = map[string]any{"title": c.GetTitle(), "description": c.GetDescription(), "expression": c.GetExpression()}
		}
		bindings = append(bindings, m)
	}
	body["bindings"] = bindings
	var out jsonPolicy
	if err := g.do(ctx, http.MethodPut, bucketPath(b)+"/iam", nil, body, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	b, err := iamBucket(req.GetResource())
	if err != nil {
		return nil, err
	}
	q := url.Values{"permissions": req.GetPermissions()}
	var out struct{ Permissions []string }
	if err := g.do(ctx, http.MethodGet, bucketPath(b)+"/iam/testPermissions", q, nil, &out); err != nil {
		return nil, err
	}
	return &iampb.TestIamPermissionsResponse{Permissions: out.Permissions}, nil
}

// --- object metadata ------------------------------------------------------

func (g *grpcStorage) GetObject(ctx context.Context, req *pb.GetObjectRequest) (*pb.Object, error) {
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return nil, err
	}
	b, err := bucketID(req.GetBucket())
	if err != nil {
		return nil, err
	}
	q := url.Values{"projection": {"full"}}
	setGen(q, "generation", req.GetGeneration())
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", req.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	if req.GetSoftDeleted() {
		q.Set("softDeleted", "true")
	}
	setIf(q, "restoreToken", req.GetRestoreToken())
	return g.getObject(ctx, b, req.GetObject(), q)
}

func (g *grpcStorage) getObject(ctx context.Context, b, o string, q url.Values) (*pb.Object, error) {
	var out jsonObject
	if err := g.do(ctx, http.MethodGet, objectPath(b, o), q, nil, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) ListObjects(ctx context.Context, req *pb.ListObjectsRequest) (*pb.ListObjectsResponse, error) {
	b, err := bucketID(req.GetParent())
	if err != nil {
		return nil, err
	}
	if req.GetFilter() != "" {
		return nil, grpcstatus.Error(codes.Unimplemented, "ListObjects filter is not implemented")
	}
	q := url.Values{"projection": {"full"}}
	setIf(q, "prefix", req.GetPrefix())
	setIf(q, "delimiter", req.GetDelimiter())
	setIf(q, "pageToken", req.GetPageToken())
	setIf(q, "startOffset", req.GetLexicographicStart())
	setIf(q, "endOffset", req.GetLexicographicEnd())
	setIf(q, "matchGlob", req.GetMatchGlob())
	if req.GetPageSize() > 0 {
		q.Set("maxResults", strconv.Itoa(int(req.GetPageSize())))
	}
	for name, on := range map[string]bool{"versions": req.GetVersions(), "includeTrailingDelimiter": req.GetIncludeTrailingDelimiter(),
		"softDeleted": req.GetSoftDeleted(), "includeFoldersAsPrefixes": req.GetIncludeFoldersAsPrefixes()} {
		if on {
			q.Set(name, "true")
		}
	}
	var out struct {
		Items         []jsonObject
		Prefixes      []string
		NextPageToken string
	}
	if err := g.do(ctx, http.MethodGet, bucketPath(b)+"/o", q, nil, &out); err != nil {
		return nil, err
	}
	resp := &pb.ListObjectsResponse{Prefixes: out.Prefixes, NextPageToken: out.NextPageToken}
	for _, o := range out.Items {
		resp.Objects = append(resp.Objects, o.proto())
	}
	return resp, nil
}

func (g *grpcStorage) DeleteObject(ctx context.Context, req *pb.DeleteObjectRequest) (*emptypb.Empty, error) {
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return nil, err
	}
	b, err := bucketID(req.GetBucket())
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	setGen(q, "generation", req.GetGeneration())
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", req.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	if err := g.do(ctx, http.MethodDelete, objectPath(b, req.GetObject()), q, nil, nil); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (g *grpcStorage) UpdateObject(ctx context.Context, req *pb.UpdateObjectRequest) (*pb.Object, error) {
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return nil, err
	}
	o := req.GetObject()
	b, err := bucketID(o.GetBucket())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "update_mask is required")
	}
	body, err := objectToJSON(o, paths)
	if err != nil {
		return nil, err
	}
	q := url.Values{"projection": {"full"}}
	setGen(q, "generation", o.GetGeneration())
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", req.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	setIf(q, "predefinedAcl", req.GetPredefinedAcl())
	if req.GetOverrideUnlockedRetention() {
		q.Set("overrideUnlockedRetention", "true")
	}
	var out jsonObject
	if err := g.do(ctx, http.MethodPatch, objectPath(b, o.GetName()), q, body, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) ComposeObject(ctx context.Context, req *pb.ComposeObjectRequest) (*pb.Object, error) {
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return nil, err
	}
	d := req.GetDestination()
	b, err := bucketID(d.GetBucket())
	if err != nil {
		return nil, err
	}
	dest, err := objectToJSON(d, nil)
	if err != nil {
		return nil, err
	}
	addChecksums(dest, req.GetObjectChecksums())
	var srcs []map[string]any
	for _, s := range req.GetSourceObjects() {
		m := map[string]any{"name": s.GetName()}
		if s.GetGeneration() != 0 {
			m["generation"] = strconv.FormatInt(s.GetGeneration(), 10)
		}
		if p := s.GetObjectPreconditions(); p != nil && p.IfGenerationMatch != nil {
			m["objectPreconditions"] = map[string]any{"ifGenerationMatch": strconv.FormatInt(*p.IfGenerationMatch, 10)}
		}
		srcs = append(srcs, m)
	}
	q := url.Values{}
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	setIf(q, "destinationPredefinedAcl", req.GetDestinationPredefinedAcl())
	setIf(q, "kmsKeyName", req.GetKmsKey())
	var out jsonObject
	if err := g.do(ctx, http.MethodPost, objectPath(b, d.GetName())+"/compose", q,
		map[string]any{"destination": dest, "sourceObjects": srcs, "deleteSourceObjects": req.GetDeleteSourceObjects()}, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) RewriteObject(ctx context.Context, req *pb.RewriteObjectRequest) (*pb.RewriteResponse, error) {
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return nil, err
	}
	if req.GetCopySourceEncryptionAlgorithm() != "" || len(req.GetCopySourceEncryptionKeyBytes()) > 0 {
		return nil, grpcstatus.Error(codes.Unimplemented, "customer-supplied encryption keys are not implemented")
	}
	sb, err := bucketID(req.GetSourceBucket())
	if err != nil {
		return nil, err
	}
	db, err := bucketID(req.GetDestinationBucket())
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if req.GetDestination() != nil {
		if body, err = objectToJSON(req.GetDestination(), nil); err != nil {
			return nil, err
		}
	}
	addChecksums(body, req.GetObjectChecksums())
	q := url.Values{}
	setGen(q, "sourceGeneration", req.GetSourceGeneration())
	setIf(q, "rewriteToken", req.GetRewriteToken())
	setIf(q, "destinationKmsKeyName", req.GetDestinationKmsKey())
	setIf(q, "destinationPredefinedAcl", req.GetDestinationPredefinedAcl())
	if req.GetMaxBytesRewrittenPerCall() > 0 {
		q.Set("maxBytesRewrittenPerCall", strconv.FormatInt(req.GetMaxBytesRewrittenPerCall(), 10))
	}
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", req.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	pre(q, "ifSourceGenerationMatch", req.IfSourceGenerationMatch)
	pre(q, "ifSourceGenerationNotMatch", req.IfSourceGenerationNotMatch)
	pre(q, "ifSourceMetagenerationMatch", req.IfSourceMetagenerationMatch)
	pre(q, "ifSourceMetagenerationNotMatch", req.IfSourceMetagenerationNotMatch)
	q.Set("projection", "full")
	path := objectPath(sb, req.GetSourceObject()) + "/rewriteTo/b/" + url.PathEscape(db) + "/o/" + url.PathEscape(req.GetDestinationName())
	var out struct {
		TotalBytesRewritten string
		ObjectSize          string
		Done                bool
		RewriteToken        string
		Resource            *jsonObject
	}
	if err := g.do(ctx, http.MethodPost, path, q, body, &out); err != nil {
		return nil, err
	}
	resp := &pb.RewriteResponse{Done: out.Done, RewriteToken: out.RewriteToken}
	resp.TotalBytesRewritten, _ = strconv.ParseInt(out.TotalBytesRewritten, 10, 64)
	resp.ObjectSize, _ = strconv.ParseInt(out.ObjectSize, 10, 64)
	if out.Resource != nil {
		resp.Resource = out.Resource.proto()
	}
	return resp, nil
}

func (g *grpcStorage) RestoreObject(ctx context.Context, req *pb.RestoreObjectRequest) (*pb.Object, error) {
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return nil, err
	}
	b, err := bucketID(req.GetBucket())
	if err != nil {
		return nil, err
	}
	q := url.Values{"projection": {"full"}}
	setGen(q, "generation", req.GetGeneration())
	setIf(q, "restoreToken", req.GetRestoreToken())
	if req.CopySourceAcl != nil {
		q.Set("copySourceAcl", strconv.FormatBool(req.GetCopySourceAcl()))
	}
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", req.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	var out jsonObject
	if err := g.do(ctx, http.MethodPost, objectPath(b, req.GetObject())+"/restore", q, nil, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

func (g *grpcStorage) MoveObject(ctx context.Context, req *pb.MoveObjectRequest) (*pb.Object, error) {
	b, err := bucketID(req.GetBucket())
	if err != nil {
		return nil, err
	}
	q := url.Values{"projection": {"full"}}
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", req.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	pre(q, "ifSourceGenerationMatch", req.IfSourceGenerationMatch)
	pre(q, "ifSourceGenerationNotMatch", req.IfSourceGenerationNotMatch)
	pre(q, "ifSourceMetagenerationMatch", req.IfSourceMetagenerationMatch)
	pre(q, "ifSourceMetagenerationNotMatch", req.IfSourceMetagenerationNotMatch)
	path := objectPath(b, req.GetSourceObject()) + "/moveTo/o/" + url.PathEscape(req.GetDestinationObject())
	var out jsonObject
	if err := g.do(ctx, http.MethodPost, path, q, nil, &out); err != nil {
		return nil, err
	}
	return out.proto(), nil
}

// --- reads ------------------------------------------------------------------

// readMedia reads [off, off+n) of a pinned generation (n < 0: to the end).
func (g *grpcStorage) readMedia(ctx context.Context, b, o string, gen, off, n int64) ([]byte, error) {
	q := url.Values{"alt": {"media"}, "generation": {strconv.FormatInt(gen, 10)}}
	hdr := http.Header{}
	switch {
	case n == 0:
		return nil, nil
	case n < 0:
		hdr.Set("Range", fmt.Sprintf("bytes=%d-", off))
	default:
		hdr.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	}
	rep, err := g.call(ctx, http.MethodGet, objectPath(b, o), q, hdr, nil)
	if err != nil {
		return nil, err
	}
	if err := replyError(rep); err != nil {
		return nil, err
	}
	return rep.body, nil
}

// span resolves a read's offset and length against the object's size, as
// ReadObject and BidiReadObject define them: a negative offset counts from
// the end, and a length of 0 reads to the end.
func span(size, off, n int64) (start, length int64, err error) {
	if off < 0 {
		off = max(0, size+off)
	}
	if off > size {
		return 0, 0, grpcstatus.Errorf(codes.OutOfRange, "read_offset %d is past the object's %d byte(s)", off, size)
	}
	if n < 0 {
		return 0, 0, grpcstatus.Errorf(codes.InvalidArgument, "a read's length cannot be negative")
	}
	length = size - off
	if n > 0 && n < length {
		length = n
	}
	return off, length, nil
}

func checksummed(b []byte) *pb.ChecksummedData {
	c := crc32.Checksum(b, crcTable)
	return &pb.ChecksummedData{Content: b, Crc32C: &c}
}

func (g *grpcStorage) ReadObject(req *pb.ReadObjectRequest, stream pb.Storage_ReadObjectServer) error {
	ctx := stream.Context()
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return err
	}
	b, err := bucketID(req.GetBucket())
	if err != nil {
		return err
	}
	q := url.Values{"projection": {"full"}}
	setGen(q, "generation", req.GetGeneration())
	pre(q, "ifGenerationMatch", req.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", req.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", req.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", req.IfMetagenerationNotMatch)
	obj, err := g.getObject(ctx, b, req.GetObject(), q)
	if err != nil {
		return err
	}
	start, length, err := span(obj.GetSize(), req.GetReadOffset(), req.GetReadLimit())
	if err != nil {
		return err
	}
	data, err := g.readMedia(ctx, b, req.GetObject(), obj.GetGeneration(), start, length)
	if err != nil {
		return err
	}
	first := &pb.ReadObjectResponse{Metadata: obj}
	if start == 0 && length == obj.GetSize() {
		first.ObjectChecksums = obj.GetChecksums()
	} else {
		first.ContentRange = &pb.ContentRange{Start: start, End: start + length, CompleteLength: obj.GetSize()}
	}
	for i := 0; ; i += grpcChunk {
		msg := first
		if i > 0 {
			msg = &pb.ReadObjectResponse{}
		}
		if i < len(data) {
			msg.ChecksummedData = checksummed(data[i:min(len(data), i+grpcChunk)])
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
		if i+grpcChunk >= len(data) {
			return nil
		}
	}
}

func (g *grpcStorage) BidiReadObject(stream pb.Storage_BidiReadObjectServer) error {
	ctx := stream.Context()
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	spec := req.GetReadObjectSpec()
	if spec == nil {
		return grpcstatus.Error(codes.InvalidArgument, "the first BidiReadObjectRequest must carry read_object_spec")
	}
	if err := noCSEK(spec.GetCommonObjectRequestParams()); err != nil {
		return err
	}
	b, err := bucketID(spec.GetBucket())
	if err != nil {
		return err
	}
	gen := spec.GetGeneration()
	if h := spec.GetReadHandle().GetHandle(); gen == 0 && len(h) == 8 {
		gen = int64(binary.BigEndian.Uint64(h))
	}
	q := url.Values{"projection": {"full"}}
	setGen(q, "generation", gen)
	pre(q, "ifGenerationMatch", spec.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", spec.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", spec.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", spec.IfMetagenerationNotMatch)
	obj, err := g.getObject(ctx, b, spec.GetObject(), q)
	if err != nil {
		return err
	}
	handle := binary.BigEndian.AppendUint64(nil, uint64(obj.GetGeneration()))
	if err := stream.Send(&pb.BidiReadObjectResponse{Metadata: obj, ReadHandle: &pb.BidiReadHandle{Handle: handle}}); err != nil {
		return err
	}
	for {
		for _, rr := range req.GetReadRanges() {
			if err := g.sendRange(ctx, stream, b, obj, rr); err != nil {
				return err
			}
		}
		if req, err = stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (g *grpcStorage) sendRange(ctx context.Context, stream pb.Storage_BidiReadObjectServer, b string, obj *pb.Object, rr *pb.ReadRange) error {
	start, length, err := span(obj.GetSize(), rr.GetReadOffset(), rr.GetReadLength())
	if err != nil {
		return err
	}
	data, err := g.readMedia(ctx, b, obj.GetName(), obj.GetGeneration(), start, length)
	if err != nil {
		return err
	}
	for i := 0; ; i += grpcChunk {
		end := min(len(data), i+grpcChunk)
		part := &pb.ObjectRangeData{
			ChecksummedData: checksummed(data[i:end]),
			ReadRange:       &pb.ReadRange{ReadOffset: start + int64(i), ReadLength: int64(end - i), ReadId: rr.GetReadId()},
			RangeEnd:        end == len(data),
		}
		if err := stream.Send(&pb.BidiReadObjectResponse{ObjectDataRanges: []*pb.ObjectRangeData{part}}); err != nil {
			return err
		}
		if end == len(data) {
			return nil
		}
	}
}

// --- writes -----------------------------------------------------------------

// The JSON API's resumable sessions carry every gRPC write: a write that
// names no upload_id starts one, so a single code path persists chunks,
// checks preconditions at the finish and verifies the checksums.

// uploadIDOf is the upload_id in a resumable session's Location.
func uploadIDOf(loc string) (string, error) {
	u, err := url.Parse(loc)
	if err != nil || u.Query().Get("upload_id") == "" {
		return "", grpcstatus.Errorf(codes.Internal, "the resumable session's Location %q names no upload_id", loc)
	}
	return u.Query().Get("upload_id"), nil
}

func (g *grpcStorage) StartResumableWrite(ctx context.Context, req *pb.StartResumableWriteRequest) (*pb.StartResumableWriteResponse, error) {
	if err := noCSEK(req.GetCommonObjectRequestParams()); err != nil {
		return nil, err
	}
	id, err := g.startWrite(ctx, req.GetWriteObjectSpec(), req.GetObjectChecksums())
	if err != nil {
		return nil, err
	}
	return &pb.StartResumableWriteResponse{UploadId: id}, nil
}

func (g *grpcStorage) startWrite(ctx context.Context, spec *pb.WriteObjectSpec, sums *pb.ObjectChecksums) (string, error) {
	if spec.GetAppendable() {
		return "", grpcstatus.Error(codes.Unimplemented, "appendable objects are not implemented")
	}
	r := spec.GetResource()
	b, err := bucketID(r.GetBucket())
	if err != nil {
		return "", err
	}
	body, err := objectToJSON(r, nil)
	if err != nil {
		return "", err
	}
	addChecksums(body, sums)
	q := url.Values{"uploadType": {"resumable"}, "name": {r.GetName()}}
	pre(q, "ifGenerationMatch", spec.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", spec.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", spec.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", spec.IfMetagenerationNotMatch)
	setIf(q, "predefinedAcl", spec.GetPredefinedAcl())
	setIf(q, "kmsKeyName", r.GetKmsKey())
	raw, _ := json.Marshal(body)
	hdr := http.Header{"Content-Type": {"application/json; charset=UTF-8"}}
	if spec.ObjectSize != nil {
		hdr.Set("X-Upload-Content-Length", strconv.FormatInt(spec.GetObjectSize(), 10))
	}
	if ct := r.GetContentType(); ct != "" {
		hdr.Set("X-Upload-Content-Type", ct)
	}
	rep, err := g.call(ctx, http.MethodPost, uploadPrefix+"b/"+url.PathEscape(b)+"/o", q, hdr, raw)
	if err != nil {
		return "", err
	}
	if err := replyError(rep); err != nil {
		return "", err
	}
	return uploadIDOf(rep.header.Get("Location"))
}

// putChunk sends data at first to session id; total < 0 means more is to
// come. It returns the bytes persisted, or the object once it is final.
func (g *grpcStorage) putChunk(ctx context.Context, id string, first int64, data []byte, total int64, sums *pb.ObjectChecksums) (int64, *pb.Object, error) {
	q := url.Values{"uploadType": {"resumable"}, "upload_id": {id}}
	hdr := http.Header{}
	tot := "*"
	if total >= 0 {
		tot = strconv.FormatInt(total, 10)
	}
	if len(data) == 0 {
		hdr.Set("Content-Range", "bytes */"+tot)
	} else {
		hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%s", first, first+int64(len(data))-1, tot))
	}
	var hashes []string
	if c := sums.GetCrc32C(); sums != nil && sums.Crc32C != nil {
		hashes = append(hashes, "crc32c="+base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, c)))
	}
	if m := sums.GetMd5Hash(); len(m) > 0 {
		hashes = append(hashes, "md5="+base64.StdEncoding.EncodeToString(m))
	}
	if len(hashes) > 0 {
		hdr.Set("X-Goog-Hash", strings.Join(hashes, ","))
	}
	if data == nil {
		data = []byte{}
	}
	hdr.Set("Content-Type", "application/octet-stream")
	rep, err := g.call(ctx, http.MethodPut, uploadPrefix+"b/_/o", q, hdr, data)
	if err != nil {
		return 0, nil, err
	}
	if err := replyError(rep); err != nil {
		return 0, nil, err
	}
	if rep.status == http.StatusPermanentRedirect {
		var n int64
		if r := rep.header.Get("Range"); r != "" {
			if _, last, ok := strings.Cut(strings.TrimPrefix(r, "bytes="), "-"); ok {
				l, _ := strconv.ParseInt(last, 10, 64)
				n = l + 1
			}
		}
		return n, nil, nil
	}
	var o jsonObject
	if err := json.Unmarshal(rep.body, &o); err != nil {
		return 0, nil, grpcstatus.Errorf(codes.Internal, "decoding the JSON API's answer: %v", err)
	}
	obj := o.proto()
	return obj.GetSize(), obj, nil
}

func (g *grpcStorage) QueryWriteStatus(ctx context.Context, req *pb.QueryWriteStatusRequest) (*pb.QueryWriteStatusResponse, error) {
	n, obj, err := g.putChunk(ctx, req.GetUploadId(), 0, nil, -1, nil)
	if err != nil {
		return nil, err
	}
	if obj != nil {
		return &pb.QueryWriteStatusResponse{WriteStatus: &pb.QueryWriteStatusResponse_Resource{Resource: obj}}, nil
	}
	return &pb.QueryWriteStatusResponse{WriteStatus: &pb.QueryWriteStatusResponse_PersistedSize{PersistedSize: n}}, nil
}

func (g *grpcStorage) CancelResumableWrite(ctx context.Context, req *pb.CancelResumableWriteRequest) (*pb.CancelResumableWriteResponse, error) {
	rep, err := g.call(ctx, http.MethodDelete, uploadPrefix+"b/_/o", url.Values{"uploadType": {"resumable"}, "upload_id": {req.GetUploadId()}}, nil, nil)
	if err != nil {
		return nil, err
	}
	if rep.status != 499 {
		if err := replyError(rep); err != nil {
			return nil, err
		}
	}
	return &pb.CancelResumableWriteResponse{}, nil
}

// writer is one gRPC write in progress over a resumable session.
type writer struct {
	g         *grpcStorage
	id        string
	persisted int64
	buf       []byte
	obj       *pb.Object
}

// writeMsg is what WriteObject and BidiWriteObject messages share.
type writeMsg struct {
	uploadID string
	spec     *pb.WriteObjectSpec
	params   *pb.CommonObjectRequestParams
	offset   int64
	data     *pb.ChecksummedData
	sums     *pb.ObjectChecksums
	finish   bool
	flush    bool
}

// open starts or resumes the session the first message names.
func (g *grpcStorage) open(ctx context.Context, m writeMsg) (*writer, error) {
	if err := noCSEK(m.params); err != nil {
		return nil, err
	}
	w := &writer{g: g}
	switch {
	case m.uploadID != "":
		w.id = m.uploadID
		n, obj, err := g.putChunk(ctx, w.id, 0, nil, -1, nil)
		if err != nil {
			return nil, err
		}
		w.persisted, w.obj = n, obj
	case m.spec != nil:
		id, err := g.startWrite(ctx, m.spec, nil)
		if err != nil {
			return nil, err
		}
		w.id = id
	default:
		return nil, grpcstatus.Error(codes.InvalidArgument, "the first write message must carry upload_id or write_object_spec")
	}
	return w, nil
}

// add takes one message's data, persisting whole 256 KiB units as they
// fill (all of them when flush is set), and the rest at the finish.
func (w *writer) add(ctx context.Context, m writeMsg) error {
	if w.obj != nil {
		return nil
	}
	if d := m.data; d != nil {
		if d.Crc32C != nil && crc32.Checksum(d.GetContent(), crcTable) != d.GetCrc32C() {
			return grpcstatus.Errorf(codes.InvalidArgument, "the data at write_offset %d does not match its crc32c", m.offset)
		}
		end := w.persisted + int64(len(w.buf))
		content := d.GetContent()
		switch {
		case m.offset > end:
			return grpcstatus.Errorf(codes.OutOfRange, "write_offset %d is past the %d byte(s) received", m.offset, end)
		case m.offset < end:
			skip := end - m.offset
			if skip >= int64(len(content)) {
				content = nil
			} else {
				content = content[skip:]
			}
		}
		w.buf = append(w.buf, content...)
	}
	if m.finish {
		total := w.persisted + int64(len(w.buf))
		_, obj, err := w.g.putChunk(ctx, w.id, w.persisted, w.buf, total, m.sums)
		if err != nil {
			return err
		}
		if obj == nil {
			return grpcstatus.Error(codes.Internal, "the write did not finish")
		}
		w.obj, w.buf, w.persisted = obj, nil, total
		return nil
	}
	if len(w.buf) >= grpcChunk*4 || (m.flush && len(w.buf) >= chunkUnit) {
		return w.persist(ctx)
	}
	return nil
}

// persist sends the buffer's whole 256 KiB units.
func (w *writer) persist(ctx context.Context) error {
	n := len(w.buf) / chunkUnit * chunkUnit
	if n == 0 || w.obj != nil {
		return nil
	}
	got, _, err := w.g.putChunk(ctx, w.id, w.persisted, w.buf[:n], -1, nil)
	if err != nil {
		return err
	}
	w.buf = append([]byte(nil), w.buf[got-w.persisted:]...)
	w.persisted = got
	return nil
}

func (g *grpcStorage) WriteObject(stream pb.Storage_WriteObjectServer) error {
	ctx := stream.Context()
	var w *writer
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if w == nil {
				return grpcstatus.Error(codes.InvalidArgument, "no write message was sent")
			}
			if w.obj != nil {
				return stream.SendAndClose(&pb.WriteObjectResponse{WriteStatus: &pb.WriteObjectResponse_Resource{Resource: w.obj}})
			}
			if err := w.persist(ctx); err != nil {
				return err
			}
			return stream.SendAndClose(&pb.WriteObjectResponse{WriteStatus: &pb.WriteObjectResponse_PersistedSize{PersistedSize: w.persisted}})
		}
		if err != nil {
			return err
		}
		m := writeMsg{uploadID: req.GetUploadId(), spec: req.GetWriteObjectSpec(), params: req.GetCommonObjectRequestParams(),
			offset: req.GetWriteOffset(), data: req.GetChecksummedData(), sums: req.GetObjectChecksums(), finish: req.GetFinishWrite()}
		if w == nil {
			if w, err = g.open(ctx, m); err != nil {
				return err
			}
		}
		if err := w.add(ctx, m); err != nil {
			return err
		}
	}
}

func (g *grpcStorage) BidiWriteObject(stream pb.Storage_BidiWriteObjectServer) error {
	ctx := stream.Context()
	var w *writer
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if w != nil && w.obj == nil {
				return w.persist(ctx)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if req.GetAppendObjectSpec() != nil {
			return grpcstatus.Error(codes.Unimplemented, "appendable objects are not implemented")
		}
		m := writeMsg{uploadID: req.GetUploadId(), spec: req.GetWriteObjectSpec(), params: req.GetCommonObjectRequestParams(),
			offset: req.GetWriteOffset(), data: req.GetChecksummedData(), sums: req.GetObjectChecksums(),
			finish: req.GetFinishWrite(), flush: req.GetFlush()}
		if w == nil {
			if w, err = g.open(ctx, m); err != nil {
				return err
			}
		}
		if err := w.add(ctx, m); err != nil {
			return err
		}
		switch {
		case w.obj != nil && (m.finish || req.GetStateLookup()):
			if err := stream.Send(&pb.BidiWriteObjectResponse{WriteStatus: &pb.BidiWriteObjectResponse_Resource{Resource: w.obj}}); err != nil {
				return err
			}
		case req.GetStateLookup():
			if err := stream.Send(&pb.BidiWriteObjectResponse{WriteStatus: &pb.BidiWriteObjectResponse_PersistedSize{PersistedSize: w.persisted}}); err != nil {
				return err
			}
		}
	}
}

// addChecksums sets an object's expected crc32c and md5Hash in its JSON
// metadata, which the JSON API verifies against the bytes.
func addChecksums(m map[string]any, sums *pb.ObjectChecksums) {
	if sums == nil {
		return
	}
	if c := sums.Crc32C; c != nil {
		m["crc32c"] = base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, *c))
	}
	if h := sums.GetMd5Hash(); len(h) > 0 {
		m["md5Hash"] = base64.StdEncoding.EncodeToString(h)
	}
}

// GRPCMethods is every google.storage.v2.Storage method, as Service/Method,
// for the coverage registry (tools/coverage).
func GRPCMethods() []string {
	d := pb.Storage_ServiceDesc
	var out []string
	for _, m := range d.Methods {
		out = append(out, d.ServiceName+"/"+m.MethodName)
	}
	for _, m := range d.Streams {
		out = append(out, d.ServiceName+"/"+m.StreamName)
	}
	sort.Strings(out)
	return out
}
