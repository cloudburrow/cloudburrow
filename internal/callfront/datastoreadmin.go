package callfront

// The Datastore Admin API (google.datastore.admin.v1, #1884), served by the
// Datastore front, which the emulator does not serve:
//
//   - ExportEntities and ImportEntities, with entity filters, to and from
//     the instance's Cloud Storage (--storage) in the layout Google
//     documents for a managed export
//     (https://docs.cloud.google.com/datastore/docs/export-import-entities):
//     under the output URL prefix, a folder named after the export's start
//     time holding a <folder>.overall_export_metadata file and, for each
//     namespace and kind folder (all_namespaces or default_namespace or
//     namespace_<ns>, then all_kinds or kind_<kind>), a
//     <namespace folder>_<kind folder>.export_metadata file and output-N
//     files. The output files are LevelDB log files, as Google's are;
//     their records are google.datastore.v1 Entity messages, not the
//     legacy EntityProto Google writes, so an export imports back here,
//     but BigQuery does not load it.
//   - CreateIndex, GetIndex, ListIndexes and DeleteIndex: the indexes are
//     kept as entities of the emulator itself, in MarkerProject, beside the
//     named databases' markers, and are READY when created (the emulator
//     serves every query without them).
//   - The operations they start (google.longrunning.Operations, at
//     projects/{project}/operations/{id}), which finish before the call
//     returns, kept in memory.
//
// A call names its database in its routing header (database_id) or, over
// REST, its body's databaseId; the access gate refuses a Native mode one.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	adminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/grpchttp"
)

const (
	datastoreAdminPrefix = "/google.datastore.admin.v1.DatastoreAdmin/"
	operationsPrefix     = "/google.longrunning.Operations/"
	indexKind            = "Index"
)

var (
	adminREST     = regexp.MustCompile(`^/v1/projects/([^/:]+)(?::(export|import)|/(indexes|operations)(?:/([^/:]+))?(?::(cancel))?)$`)
	adminOpNameRe = regexp.MustCompile(`^projects/([^/]+)/operations/([^/]+)$`)
	adminProjRe   = regexp.MustCompile(`^projects/([^/]+)$`)
)

// datastoreAdmin is the Datastore Admin API and its operations.
type datastoreAdmin struct {
	adminpb.UnimplementedDatastoreAdminServer
	longrunningpb.UnimplementedOperationsServer

	dbs  *datastoreDatabases
	gcs  *firestoreadmin.GCS
	grpc *grpc.Server
	now  func() time.Time

	mu    sync.Mutex
	ops   map[string]*longrunningpb.Operation
	order []string
}

// newDatastoreAdmin is the Admin API over dbs's emulator, exporting to the
// Cloud Storage at storage ("" when Cloud Storage is not served).
func newDatastoreAdmin(dbs *datastoreDatabases, storage string) *datastoreAdmin {
	a := &datastoreAdmin{dbs: dbs, now: time.Now, ops: map[string]*longrunningpb.Operation{}}
	if storage != "" {
		a.gcs = firestoreadmin.NewGCS(strings.TrimSuffix(storage, "/"), &http.Client{Timeout: 5 * time.Minute})
	}
	a.grpc = grpchttp.NewServer()
	adminpb.RegisterDatastoreAdminServer(a.grpc, a)
	longrunningpb.RegisterOperationsServer(a.grpc, a)
	return a
}

// Stop stops the gRPC server (#1804).
func (a *datastoreAdmin) Stop(ctx context.Context) error { return grpchttp.Stop(ctx, a.grpc) }

// Claims claims the Admin API's calls, over gRPC and REST, and the copy
// paths.
func (a *datastoreAdmin) Claims(r *http.Request) bool {
	if copyPaths[r.URL.Path] {
		return true
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		return strings.HasPrefix(r.URL.Path, datastoreAdminPrefix) || strings.HasPrefix(r.URL.Path, operationsPrefix)
	}
	return adminREST.MatchString(r.URL.Path)
}

func (a *datastoreAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case copyPaths[r.URL.Path]:
		a.serveCopy(w, r)
	case strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc"):
		a.grpc.ServeHTTP(w, r)
	default:
		a.serveREST(w, r)
	}
}

// --- the database a call names ---

// callDatabase is the database a gRPC call's routing header names,
// "(default)" for none.
func callDatabase(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, h := range md.Get("x-goog-request-params") {
		q, err := url.ParseQuery(h)
		if err == nil && q.Get("database_id") != "" {
			return q.Get("database_id")
		}
	}
	if db := md.Get(restDatabaseKey); len(db) > 0 && db[0] != "" {
		return db[0]
	}
	return "(default)"
}

// restDatabaseKey carries a REST call's databaseId to the handlers.
const restDatabaseKey = "x-cloudburrow-database-id"

// served refuses a database the Datastore API does not serve.
func (a *datastoreAdmin) served(ctx context.Context, project, database string) error {
	if project == "" {
		return status.Error(codes.InvalidArgument, "project_id is required.")
	}
	if a.dbs.gate == nil {
		return nil
	}
	return a.dbs.gate.access(ctx, "projects/"+project+"/databases/"+database)
}

// --- operations ---

func opID() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "AS" + strings.TrimRight(base64.RawURLEncoding.EncodeToString(b), "=")
}

// finish records a finished operation of the project.
func (a *datastoreAdmin) finish(project string, meta, resp proto.Message) (*longrunningpb.Operation, error) {
	m, err := anypb.New(meta)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation metadata: %v", err)
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation response: %v", err)
	}
	op := &longrunningpb.Operation{Name: "projects/" + project + "/operations/" + opID(), Metadata: m, Done: true,
		Result: &longrunningpb.Operation_Response{Response: r}}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ops[op.Name] = op
	a.order = append(a.order, op.Name)
	// Keep the newest thousand.
	for len(a.order) > 1000 {
		delete(a.ops, a.order[0])
		a.order = a.order[1:]
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

func (a *datastoreAdmin) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	if !adminOpNameRe.MatchString(req.GetName()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid operation name %q: want projects/{project}/operations/{operation}", req.GetName())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	op, ok := a.ops[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Operation %s not found.", req.GetName())
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

func (a *datastoreAdmin) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	m := adminProjRe.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name %q: want projects/{project}", req.GetName())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var all []*longrunningpb.Operation
	for i := len(a.order) - 1; i >= 0; i-- {
		if op := a.ops[a.order[i]]; op != nil && strings.HasPrefix(op.Name, req.GetName()+"/operations/") {
			all = append(all, op)
		}
	}
	start, err := pageStart(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 1000
	}
	resp := &longrunningpb.ListOperationsResponse{}
	for i := start; i < len(all) && i < start+size; i++ {
		resp.Operations = append(resp.Operations, proto.Clone(all[i]).(*longrunningpb.Operation))
	}
	if start+size < len(all) {
		resp.NextPageToken = strconv.Itoa(start + size)
	}
	return resp, nil
}

func pageStart(tok string) (int, error) {
	if tok == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n < 0 {
		return 0, status.Errorf(codes.InvalidArgument, "invalid page_token %q", tok)
	}
	return n, nil
}

func (a *datastoreAdmin) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.ops[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "Operation %s not found.", req.GetName())
	}
	delete(a.ops, req.GetName())
	for i, n := range a.order {
		if n == req.GetName() {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	return &emptypb.Empty{}, nil
}

func (a *datastoreAdmin) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.ops[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "Operation %s not found.", req.GetName())
	}
	return nil, status.Error(codes.FailedPrecondition, "The operation has already completed.")
}

// --- exports and imports ---

func checkFilter(f *adminpb.EntityFilter) error {
	for _, k := range f.GetKinds() {
		if k == "" {
			return status.Error(codes.InvalidArgument, "entity_filter.kinds: a kind must not be empty.")
		}
		if strings.HasPrefix(k, "__") {
			return status.Errorf(codes.InvalidArgument, "entity_filter.kinds: the kind %q is reserved.", k)
		}
	}
	return nil
}

func filterOf(f *adminpb.EntityFilter) entityFilter {
	return entityFilter{Kinds: f.GetKinds(), Namespaces: f.GetNamespaceIds()}
}

func (a *datastoreAdmin) ExportEntities(ctx context.Context, req *adminpb.ExportEntitiesRequest) (*longrunningpb.Operation, error) {
	database := callDatabase(ctx)
	if err := a.served(ctx, req.GetProjectId(), database); err != nil {
		return nil, err
	}
	if req.GetOutputUrlPrefix() == "" {
		return nil, status.Error(codes.InvalidArgument, "output_url_prefix is required: gs://BUCKET_NAME[/NAMESPACE_PATH].")
	}
	if err := checkFilter(req.GetEntityFilter()); err != nil {
		return nil, err
	}
	start := a.now()
	res, err := a.export(ctx, req.GetProjectId(), database, filterOf(req.GetEntityFilter()), req.GetOutputUrlPrefix(), false)
	if err != nil {
		return nil, err
	}
	md := &adminpb.ExportEntitiesMetadata{
		Common: &adminpb.CommonMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(a.now()),
			OperationType: adminpb.OperationType_EXPORT_ENTITIES, Labels: req.GetLabels(), State: adminpb.CommonMetadata_SUCCESSFUL},
		ProgressEntities: &adminpb.Progress{WorkCompleted: res.Entities, WorkEstimated: res.Entities},
		ProgressBytes:    &adminpb.Progress{WorkCompleted: res.Bytes, WorkEstimated: res.Bytes},
		EntityFilter:     req.GetEntityFilter(), OutputUrlPrefix: req.GetOutputUrlPrefix()}
	return a.finish(req.GetProjectId(), md, &adminpb.ExportEntitiesResponse{OutputUrl: res.OutputURI})
}

func (a *datastoreAdmin) ImportEntities(ctx context.Context, req *adminpb.ImportEntitiesRequest) (*longrunningpb.Operation, error) {
	database := callDatabase(ctx)
	if err := a.served(ctx, req.GetProjectId(), database); err != nil {
		return nil, err
	}
	if req.GetInputUrl() == "" {
		return nil, status.Error(codes.InvalidArgument, "input_url is required: gs://BUCKET_NAME[/NAMESPACE_PATH]/OVERALL_EXPORT_METADATA_FILE.")
	}
	if !strings.HasSuffix(req.GetInputUrl(), ".overall_export_metadata") {
		return nil, status.Errorf(codes.InvalidArgument, "input_url %q must name an export's overall_export_metadata file.", req.GetInputUrl())
	}
	if err := checkFilter(req.GetEntityFilter()); err != nil {
		return nil, err
	}
	start := a.now()
	res, err := a.importEntities(ctx, req.GetProjectId(), database, filterOf(req.GetEntityFilter()), req.GetInputUrl())
	if err != nil {
		return nil, err
	}
	md := &adminpb.ImportEntitiesMetadata{
		Common: &adminpb.CommonMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(a.now()),
			OperationType: adminpb.OperationType_IMPORT_ENTITIES, Labels: req.GetLabels(), State: adminpb.CommonMetadata_SUCCESSFUL},
		ProgressEntities: &adminpb.Progress{WorkCompleted: res.Entities, WorkEstimated: res.Entities},
		ProgressBytes:    &adminpb.Progress{WorkCompleted: res.Bytes, WorkEstimated: res.Bytes},
		EntityFilter:     req.GetEntityFilter(), InputUrl: req.GetInputUrl()}
	return a.finish(req.GetProjectId(), md, &emptypb.Empty{})
}

func (a *datastoreAdmin) needStorage() error {
	if a.gcs == nil {
		return status.Error(codes.FailedPrecondition, "exports and imports are not served here: Cloud Storage is not enabled on this instance, and they go through the instance's own Cloud Storage. Start the instance with storage in --services")
	}
	return nil
}

// exportFolders is the namespace and kind folders of an export with the
// filter f, by folder name, with the namespace and kind each holds (""
// for all).
type exportFolder struct {
	name           string
	nsAll, kindAll bool
	ns, kind       string
}

func exportFolders(f entityFilter) []exportFolder {
	nss := []exportFolder{{name: "all_namespaces", nsAll: true}}
	if len(f.Namespaces) > 0 {
		nss = nil
		for _, ns := range f.Namespaces {
			n := "namespace_" + ns
			if ns == "" {
				n = "default_namespace"
			}
			nss = append(nss, exportFolder{name: n, ns: ns})
		}
	}
	var out []exportFolder
	for _, n := range nss {
		if len(f.Kinds) == 0 {
			out = append(out, exportFolder{name: n.name + "/all_kinds", nsAll: n.nsAll, ns: n.ns, kindAll: true})
			continue
		}
		for _, k := range f.Kinds {
			out = append(out, exportFolder{name: n.name + "/kind_" + k, nsAll: n.nsAll, ns: n.ns, kind: k})
		}
	}
	return out
}

func (e exportFolder) takes(ent *pb.Entity) bool {
	return (e.nsAll || ent.GetKey().GetPartitionId().GetNamespaceId() == e.ns) && (e.kindAll || kindOf(ent.GetKey()) == e.kind)
}

// export writes the database's entities the filter takes to Cloud
// Storage, under outputPrefix, in a folder named after its start time
// unless exact, and answers the overall_export_metadata file's URL.
func (a *datastoreAdmin) export(ctx context.Context, project, database string, f entityFilter, outputPrefix string, exact bool) (copyResult, error) {
	if err := a.needStorage(); err != nil {
		return copyResult{}, err
	}
	bucket, prefix, err := firestoreadmin.SplitGS(outputPrefix)
	if err != nil {
		return copyResult{}, err
	}
	start := a.now().UTC()
	if !exact || prefix == "" {
		// Google names the export's folder after its start time.
		stamp := start.Format("2006-01-02T15:04:05") + fmt.Sprintf("_%05d", start.Nanosecond()/10000%100000)
		prefix = strings.TrimPrefix(prefix+"/"+stamp, "/")
	}
	if err := a.gcs.BucketExists(ctx, bucket); err != nil {
		return copyResult{}, err
	}
	es, err := a.dbs.readEntities(ctx, project, database, f)
	if err != nil {
		return copyResult{}, status.Errorf(codes.Unavailable, "reading the entities: %v", err)
	}
	for _, folder := range exportFolders(f) {
		var recs [][]byte
		for _, e := range es {
			if !folder.takes(e) {
				continue
			}
			b, err := proto.Marshal(e)
			if err != nil {
				return copyResult{}, status.Errorf(codes.Internal, "encoding an entity: %v", err)
			}
			recs = append(recs, b)
		}
		if err := a.gcs.Put(ctx, bucket, prefix+"/"+folder.name+"/output-0", firestoreadmin.EncodeLog(recs)); err != nil {
			return copyResult{}, err
		}
		base := strings.ReplaceAll(folder.name, "/", "_")
		meta, _ := json.Marshal(map[string]any{"namespace": path.Dir(folder.name), "kind": path.Base(folder.name),
			"entities": len(recs), "outputs": []string{"output-0"}})
		if err := a.gcs.Put(ctx, bucket, prefix+"/"+folder.name+"/"+base+".export_metadata", meta); err != nil {
			return copyResult{}, err
		}
	}
	n, size := int64(len(es)), entityBytes(es)
	outURI := "gs://" + bucket + "/" + prefix
	md := &adminpb.ExportEntitiesMetadata{
		Common: &adminpb.CommonMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(a.now()),
			OperationType: adminpb.OperationType_EXPORT_ENTITIES, State: adminpb.CommonMetadata_SUCCESSFUL},
		ProgressEntities: &adminpb.Progress{WorkCompleted: n, WorkEstimated: n},
		ProgressBytes:    &adminpb.Progress{WorkCompleted: size, WorkEstimated: size},
		EntityFilter:     &adminpb.EntityFilter{Kinds: f.Kinds, NamespaceIds: f.Namespaces}, OutputUrlPrefix: outURI}
	overall, _ := proto.Marshal(md)
	file := prefix + "/" + path.Base(prefix) + ".overall_export_metadata"
	if err := a.gcs.Put(ctx, bucket, file, overall); err != nil {
		return copyResult{}, err
	}
	return copyResult{Entities: n, Bytes: size, OutputURI: "gs://" + bucket + "/" + file}, nil
}

var outputFileRe = regexp.MustCompile(`/output-\d+$`)

// importEntities writes the entities of the export at input (its folder,
// or its overall_export_metadata file) the filter takes into the
// database.
func (a *datastoreAdmin) importEntities(ctx context.Context, project, database string, f entityFilter, input string) (copyResult, error) {
	if err := a.needStorage(); err != nil {
		return copyResult{}, err
	}
	bucket, name, err := firestoreadmin.SplitGS(input)
	if err != nil {
		return copyResult{}, err
	}
	folder, file := name, name+"/"+path.Base(name)+".overall_export_metadata"
	if strings.HasSuffix(name, ".overall_export_metadata") {
		folder, file = path.Dir(name), name
	}
	if name == "" || folder == "." {
		return copyResult{}, status.Errorf(codes.InvalidArgument, "%s names a bucket alone: name the export's overall_export_metadata file", input)
	}
	if _, err := a.gcs.Get(ctx, bucket, file); err != nil {
		if status.Code(err) == codes.NotFound {
			return copyResult{}, status.Errorf(codes.InvalidArgument, "The export metadata file gs://%s/%s does not exist.", bucket, file)
		}
		return copyResult{}, err
	}
	names, err := a.gcs.List(ctx, bucket, folder+"/")
	if err != nil {
		return copyResult{}, err
	}
	var es []*pb.Entity
	seen := map[string]bool{}
	for _, n := range names {
		if !outputFileRe.MatchString(n) {
			continue
		}
		b, err := a.gcs.Get(ctx, bucket, n)
		if err != nil {
			return copyResult{}, err
		}
		recs, err := firestoreadmin.DecodeLog(b)
		if err != nil {
			return copyResult{}, status.Errorf(codes.InvalidArgument, "gs://%s/%s is not an export output file: %v", bucket, n, err)
		}
		for _, r := range recs {
			e := &pb.Entity{}
			if err := proto.Unmarshal(r, e); err != nil || e.Key == nil {
				return copyResult{}, status.Errorf(codes.InvalidArgument, "gs://%s/%s holds a record that is not an entity", bucket, n)
			}
			k := keyString(e.Key)
			if seen[k] || !f.wantsKind(kindOf(e.Key)) || !f.wantsNamespace(e.Key.GetPartitionId().GetNamespaceId()) {
				continue
			}
			seen[k] = true
			es = append(es, e)
		}
	}
	if err := a.dbs.writeEntities(ctx, project, database, es); err != nil {
		return copyResult{}, status.Errorf(codes.Unavailable, "%v", err)
	}
	return copyResult{Entities: int64(len(es)), Bytes: entityBytes(es)}, nil
}

// keyString identifies a key within its database.
func keyString(k *pb.Key) string {
	var b strings.Builder
	b.WriteString(k.GetPartitionId().GetNamespaceId())
	for _, p := range k.GetPath() {
		fmt.Fprintf(&b, "/%s:%d:%s", p.GetKind(), p.GetId(), p.GetName())
	}
	return b.String()
}

// --- indexes ---

func indexMarkerKey(project, database, id string) *pb.Key {
	return &pb.Key{PartitionId: &pb.PartitionId{ProjectId: MarkerProject},
		Path: []*pb.Key_PathElement{{Kind: indexKind, IdType: &pb.Key_PathElement_Name{Name: project + "/" + database + "/" + id}}}}
}

// indexes is the project's database's indexes, by ID.
func (a *datastoreAdmin) indexes(ctx context.Context, project, database string) ([]*adminpb.Index, error) {
	c, err := a.dbs.client()
	if err != nil {
		return nil, err
	}
	resp, err := c.RunQuery(withRouting(ctx), &pb.RunQueryRequest{ProjectId: MarkerProject,
		QueryType: &pb.RunQueryRequest_Query{Query: &pb.Query{Kind: []*pb.KindExpression{{Name: indexKind}}}}})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reading the indexes: %v", err)
	}
	var out []*adminpb.Index
	for _, r := range resp.GetBatch().GetEntityResults() {
		p := r.GetEntity().GetProperties()
		if p["project"].GetStringValue() != project || p["database"].GetStringValue() != database {
			continue
		}
		ix := &adminpb.Index{}
		if proto.Unmarshal(p["index"].GetBlobValue(), ix) == nil {
			out = append(out, ix)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IndexId < out[j].IndexId })
	return out, nil
}

func sameIndex(x, y *adminpb.Index) bool {
	if x.Kind != y.Kind || x.Ancestor != y.Ancestor || len(x.Properties) != len(y.Properties) {
		return false
	}
	for i := range x.Properties {
		if x.Properties[i].Name != y.Properties[i].Name || x.Properties[i].Direction != y.Properties[i].Direction {
			return false
		}
	}
	return true
}

func (a *datastoreAdmin) CreateIndex(ctx context.Context, req *adminpb.CreateIndexRequest) (*longrunningpb.Operation, error) {
	database := callDatabase(ctx)
	if err := a.served(ctx, req.GetProjectId(), database); err != nil {
		return nil, err
	}
	in := req.GetIndex()
	if in == nil || in.GetKind() == "" {
		return nil, status.Error(codes.InvalidArgument, "index.kind is required.")
	}
	if len(in.GetProperties()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "index.properties: an index needs at least one property.")
	}
	if in.GetAncestor() == adminpb.Index_ANCESTOR_MODE_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "index.ancestor is required: NONE or ALL_ANCESTORS.")
	}
	seen := map[string]bool{}
	for _, p := range in.GetProperties() {
		if p.GetName() == "" {
			return nil, status.Error(codes.InvalidArgument, "index.properties: a property needs a name.")
		}
		if p.GetDirection() == adminpb.Index_DIRECTION_UNSPECIFIED {
			return nil, status.Errorf(codes.InvalidArgument, "index.properties: the property %q needs a direction: ASCENDING or DESCENDING.", p.GetName())
		}
		if seen[p.GetName()] {
			return nil, status.Errorf(codes.InvalidArgument, "index.properties: the property %q is named twice.", p.GetName())
		}
		seen[p.GetName()] = true
	}
	if len(in.GetProperties()) < 2 && in.GetAncestor() == adminpb.Index_NONE {
		return nil, status.Error(codes.InvalidArgument, "A composite index needs at least two properties, or one with ancestor ALL_ANCESTORS: Datastore indexes every single property itself.")
	}
	have, err := a.indexes(ctx, req.GetProjectId(), database)
	if err != nil {
		return nil, err
	}
	for _, h := range have {
		if sameIndex(h, in) {
			return nil, status.Error(codes.AlreadyExists, "Index already exists.")
		}
	}
	start := a.now()
	ix := &adminpb.Index{ProjectId: req.GetProjectId(), IndexId: "CICAg" + strings.ToUpper(opID()[2:12]), Kind: in.Kind,
		Ancestor: in.Ancestor, Properties: in.Properties, State: adminpb.Index_READY}
	b, _ := proto.Marshal(ix)
	c, err := a.dbs.client()
	if err != nil {
		return nil, err
	}
	e := &pb.Entity{Key: indexMarkerKey(req.GetProjectId(), database, ix.IndexId), Properties: map[string]*pb.Value{
		"project":  {ValueType: &pb.Value_StringValue{StringValue: req.GetProjectId()}},
		"database": {ValueType: &pb.Value_StringValue{StringValue: database}},
		"index":    {ValueType: &pb.Value_BlobValue{BlobValue: b}, ExcludeFromIndexes: true},
	}}
	if _, err := c.Commit(withRouting(ctx), &pb.CommitRequest{ProjectId: MarkerProject, Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Upsert{Upsert: e}}}}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "keeping the index: %v", err)
	}
	md := &adminpb.IndexOperationMetadata{Common: &adminpb.CommonMetadata{StartTime: timestamppb.New(start),
		EndTime: timestamppb.New(a.now()), OperationType: adminpb.OperationType_CREATE_INDEX, State: adminpb.CommonMetadata_SUCCESSFUL},
		IndexId: ix.IndexId}
	return a.finish(req.GetProjectId(), md, ix)
}

func (a *datastoreAdmin) index(ctx context.Context, project, database, id string) (*adminpb.Index, error) {
	have, err := a.indexes(ctx, project, database)
	if err != nil {
		return nil, err
	}
	for _, h := range have {
		if h.IndexId == id {
			return h, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "Index %s not found.", id)
}

func (a *datastoreAdmin) GetIndex(ctx context.Context, req *adminpb.GetIndexRequest) (*adminpb.Index, error) {
	database := callDatabase(ctx)
	if err := a.served(ctx, req.GetProjectId(), database); err != nil {
		return nil, err
	}
	return a.index(ctx, req.GetProjectId(), database, req.GetIndexId())
}

func (a *datastoreAdmin) ListIndexes(ctx context.Context, req *adminpb.ListIndexesRequest) (*adminpb.ListIndexesResponse, error) {
	database := callDatabase(ctx)
	if err := a.served(ctx, req.GetProjectId(), database); err != nil {
		return nil, err
	}
	have, err := a.indexes(ctx, req.GetProjectId(), database)
	if err != nil {
		return nil, err
	}
	start, err := pageStart(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 1000
	}
	resp := &adminpb.ListIndexesResponse{}
	for i := start; i < len(have) && i < start+size; i++ {
		resp.Indexes = append(resp.Indexes, have[i])
	}
	if start+size < len(have) {
		resp.NextPageToken = strconv.Itoa(start + size)
	}
	return resp, nil
}

func (a *datastoreAdmin) DeleteIndex(ctx context.Context, req *adminpb.DeleteIndexRequest) (*longrunningpb.Operation, error) {
	database := callDatabase(ctx)
	if err := a.served(ctx, req.GetProjectId(), database); err != nil {
		return nil, err
	}
	ix, err := a.index(ctx, req.GetProjectId(), database, req.GetIndexId())
	if err != nil {
		return nil, err
	}
	start := a.now()
	c, err := a.dbs.client()
	if err != nil {
		return nil, err
	}
	if _, err := c.Commit(withRouting(ctx), &pb.CommitRequest{ProjectId: MarkerProject, Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Delete{Delete: indexMarkerKey(req.GetProjectId(), database, ix.IndexId)}}}}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "deleting the index: %v", err)
	}
	ix.State = adminpb.Index_DELETING
	md := &adminpb.IndexOperationMetadata{Common: &adminpb.CommonMetadata{StartTime: timestamppb.New(start),
		EndTime: timestamppb.New(a.now()), OperationType: adminpb.OperationType_DELETE_INDEX, State: adminpb.CommonMetadata_SUCCESSFUL},
		IndexId: ix.IndexId}
	return a.finish(req.GetProjectId(), md, ix)
}

// --- REST ---

func (a *datastoreAdmin) serveREST(w http.ResponseWriter, r *http.Request) {
	m := adminREST.FindStringSubmatch(r.URL.Path)
	if m == nil {
		restError(w, status.Error(codes.NotFound, "no such method"))
		return
	}
	project, verb, coll, id, cancel := m[1], m[2], m[3], m[4], m[5]
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		restError(w, status.Errorf(codes.InvalidArgument, "reading the body: %v", err))
		return
	}
	read := func(msg proto.Message) error {
		if len(body) == 0 {
			return nil
		}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, msg); err != nil {
			return status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err)
		}
		return nil
	}
	ctx := r.Context()
	var dbBody struct {
		DatabaseID string `json:"databaseId"`
	}
	_ = json.Unmarshal(body, &dbBody)
	db := dbBody.DatabaseID
	if db == "" {
		db = r.URL.Query().Get("databaseId")
	}
	if db != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(restDatabaseKey, db))
	}
	method := func(want string) bool {
		if r.Method == want {
			return true
		}
		restError(w, status.Errorf(codes.NotFound, "%s %s is not a method of the Datastore Admin API", r.Method, r.URL.Path))
		return false
	}
	var out proto.Message
	switch {
	case verb == "export":
		if !method(http.MethodPost) {
			return
		}
		req := &adminpb.ExportEntitiesRequest{}
		if err = read(req); err == nil {
			req.ProjectId = project
			out, err = a.ExportEntities(ctx, req)
		}
	case verb == "import":
		if !method(http.MethodPost) {
			return
		}
		req := &adminpb.ImportEntitiesRequest{}
		if err = read(req); err == nil {
			req.ProjectId = project
			out, err = a.ImportEntities(ctx, req)
		}
	case coll == "indexes" && id == "" && r.Method == http.MethodPost:
		ix := &adminpb.Index{}
		if err = read(ix); err == nil {
			out, err = a.CreateIndex(ctx, &adminpb.CreateIndexRequest{ProjectId: project, Index: ix})
		}
	case coll == "indexes" && id == "":
		if !method(http.MethodGet) {
			return
		}
		q := r.URL.Query()
		size, _ := strconv.Atoi(q.Get("pageSize"))
		out, err = a.ListIndexes(ctx, &adminpb.ListIndexesRequest{ProjectId: project, Filter: q.Get("filter"),
			PageSize: int32(size), PageToken: q.Get("pageToken")})
	case coll == "indexes" && r.Method == http.MethodDelete:
		out, err = a.DeleteIndex(ctx, &adminpb.DeleteIndexRequest{ProjectId: project, IndexId: id})
	case coll == "indexes":
		if !method(http.MethodGet) {
			return
		}
		out, err = a.GetIndex(ctx, &adminpb.GetIndexRequest{ProjectId: project, IndexId: id})
	case coll == "operations" && id == "":
		if !method(http.MethodGet) {
			return
		}
		q := r.URL.Query()
		size, _ := strconv.Atoi(q.Get("pageSize"))
		out, err = a.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: "projects/" + project,
			Filter: q.Get("filter"), PageSize: int32(size), PageToken: q.Get("pageToken")})
	case coll == "operations" && cancel != "":
		if !method(http.MethodPost) {
			return
		}
		out, err = a.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: "projects/" + project + "/operations/" + id})
	case coll == "operations" && r.Method == http.MethodDelete:
		out, err = a.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: "projects/" + project + "/operations/" + id})
	case coll == "operations":
		if !method(http.MethodGet) {
			return
		}
		out, err = a.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: "projects/" + project + "/operations/" + id})
	default:
		err = status.Errorf(codes.NotFound, "%s %s is not a method of the Datastore Admin API", r.Method, r.URL.Path)
	}
	if err != nil {
		restError(w, err)
		return
	}
	b, err := protojson.Marshal(out)
	if err != nil {
		restError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// restError renders err as Google's REST APIs do.
func restError(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	code := apierror.From(err).HTTPStatus()
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": st.Message(), "status": rpccode.Code(st.Code()).String()}})
}

// NewDatastoreAdmin is the Datastore Admin API over the Datastore emulator
// at upstream, exporting to the Cloud Storage at storage: for a test that
// registers it on a server of its own.
func NewDatastoreAdmin(upstream, storage string) adminpb.DatastoreAdminServer {
	return newDatastoreAdmin(newDatastoreDatabases(upstream, func(string, ...any) {}), storage)
}
