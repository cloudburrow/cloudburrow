// Package firestoreadmin serves the Firestore Admin API
// (google.firestore.admin.v1.FirestoreAdmin, with its long-running
// operations) in front of Google's Firestore emulator, which serves the
// documents API alone and answers every admin call 404 (#1331).
//
// The call front in the Firestore pod (internal/callfront) hands it the
// admin calls, over gRPC and over REST, and passes every other call to the
// emulator unchanged.
//
// What it keeps, in memory, as the emulator keeps its documents:
//
//   - Databases. `(default)` is described for every project until it is
//     deleted; CreateDatabase adds a named one. The emulator serves any
//     database ID it is sent, each with its own documents (measured), so a
//     created database is the emulator's database of that ID. Deleting one
//     clears its documents through the emulator's own reset endpoint.
//   - Composite indexes and field overrides (single-field index settings
//     and TTL). They are stored and returned READY at once, but change
//     nothing: the emulator answers any query without an index, and does
//     not delete expired documents.
//
// Operations are done when they are returned, as Google's are once they
// have finished, with the metadata Google documents for each.
//
// ImportDocuments, ExportDocuments, BulkDeleteDocuments, backups, backup
// schedules, restores, clones and user credentials answer UNIMPLEMENTED
// (501 over REST), each naming why.
package firestoreadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultLocation is the location `(default)` is described in: the emulator
// has none, and nam5 is the multi-region Google's own examples create it in.
const DefaultLocation = "nam5"

// DefaultDatabase is the ID of the database every project starts with.
const DefaultDatabase = "(default)"

const (
	hour = time.Hour
	week = 7 * 24 * time.Hour
)

// Admin is the Firestore Admin API, its state, and the emulator it fronts.
type Admin struct {
	adminpb.UnimplementedFirestoreAdminServer
	longrunningpb.UnimplementedOperationsServer

	// emulator is the emulator's address, for clearing a deleted
	// database's documents; "" clears nothing (tests).
	emulator string
	client   *http.Client
	now      func() time.Time
	grpc     *grpc.Server

	mu sync.Mutex
	// databases by name, per project; a project absent from seeded has
	// not been touched, and gets `(default)` when it is.
	databases map[string]*adminpb.Database
	seeded    map[string]bool
	indexes   map[string]*adminpb.Index // by name
	fields    map[string]*adminpb.Field // overrides, by name
	ops       map[string]*longrunningpb.Operation
	opOrder   []string
}

// New returns the API for the emulator at emulator (host:port).
func New(emulator string) *Admin {
	a := &Admin{
		emulator:  emulator,
		client:    &http.Client{Timeout: 30 * time.Second},
		now:       func() time.Time { return time.Now().UTC() },
		databases: map[string]*adminpb.Database{},
		seeded:    map[string]bool{},
		indexes:   map[string]*adminpb.Index{},
		fields:    map[string]*adminpb.Field{},
		ops:       map[string]*longrunningpb.Operation{},
	}
	a.grpc = grpc.NewServer()
	adminpb.RegisterFirestoreAdminServer(a.grpc, a)
	longrunningpb.RegisterOperationsServer(a.grpc, a)
	return a
}

// GRPCServer is the gRPC server of the API alone.
func (a *Admin) GRPCServer() *grpc.Server { return a.grpc }

const (
	adminService = "/google.firestore.admin.v1.FirestoreAdmin/"
	opsService   = "/google.longrunning.Operations/"
)

// Claims reports whether r is an admin call rather than a documents call:
// a gRPC call to FirestoreAdmin or Operations, or a REST call under
// /v1/projects/ that is not under a database's documents.
func (a *Admin) Claims(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		return strings.HasPrefix(r.URL.Path, adminService) || strings.HasPrefix(r.URL.Path, opsService)
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/v1/projects/") {
		return false
	}
	return !documentsPath.MatchString(p)
}

var documentsPath = regexp.MustCompile(`^/v1/projects/[^/]+/databases/[^/]+/documents(?:[/:]|$)`)

// ServeHTTP answers an admin call Claims claimed.
func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		a.grpc.ServeHTTP(w, r)
		return
	}
	a.serveREST(w, r)
}

// --- names ---

var (
	databaseIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{2,61}[a-z0-9]$`)
	uuidLikeRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	projectRe    = regexp.MustCompile(`^projects/([^/]+)$`)
	dbNameRe     = regexp.MustCompile(`^projects/([^/]+)/databases/([^/]+)$`)
	cgRe         = regexp.MustCompile(`^(projects/[^/]+/databases/[^/]+)/collectionGroups/([^/]+)$`)
	indexNameRe  = regexp.MustCompile(`^(projects/[^/]+/databases/[^/]+)/collectionGroups/([^/]+)/indexes/([^/]+)$`)
	fieldNameRe  = regexp.MustCompile(`^(projects/[^/]+/databases/[^/]+)/collectionGroups/([^/]+)/fields/(.+)$`)
	opNameRe     = regexp.MustCompile(`^(projects/[^/]+/databases/[^/]+)/operations/([^/]+)$`)
)

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// --- databases ---

// seed gives a project its `(default)` the first time it is named.
// a.mu is held.
func (a *Admin) seed(project string) {
	if a.seeded[project] {
		return
	}
	a.seeded[project] = true
	now := a.now()
	db := &adminpb.Database{
		Name: "projects/" + project + "/databases/" + DefaultDatabase, Uid: uuid(),
		CreateTime: timestamppb.New(now), UpdateTime: timestamppb.New(now),
		LocationId: DefaultLocation, Type: adminpb.Database_FIRESTORE_NATIVE,
		ConcurrencyMode:               adminpb.Database_PESSIMISTIC,
		PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED,
		AppEngineIntegrationMode:      adminpb.Database_DISABLED,
		DeleteProtectionState:         adminpb.Database_DELETE_PROTECTION_DISABLED,
		DatabaseEdition:               adminpb.Database_STANDARD,
	}
	a.databases[db.Name] = db
}

// describe is the database as Google returns it: its version retention,
// earliest version time and etag follow from its state now.
func (a *Admin) describe(db *adminpb.Database) *adminpb.Database {
	out := proto.Clone(db).(*adminpb.Database)
	retention := hour
	if out.PointInTimeRecoveryEnablement == adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED {
		retention = week
	}
	out.VersionRetentionPeriod = durationpb.New(retention)
	earliest := a.now().Add(-retention)
	if c := out.CreateTime.AsTime(); earliest.Before(c) {
		earliest = c
	}
	out.EarliestVersionTime = timestamppb.New(earliest)
	out.Etag = etag(out)
	return out
}

func etag(db *adminpb.Database) string {
	c := proto.Clone(db).(*adminpb.Database)
	c.Etag, c.EarliestVersionTime = "", nil
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	s := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(s[:12])
}

// database is the live database named name. a.mu is held.
func (a *Admin) database(name string) (*adminpb.Database, error) {
	m := dbNameRe.FindStringSubmatch(name)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid database name %q: want projects/{project}/databases/{database}", name)
	}
	a.seed(m[1])
	db, ok := a.databases[name]
	if !ok || db.DeleteTime != nil {
		return nil, status.Errorf(codes.NotFound, "The database %s does not exist.", name)
	}
	return db, nil
}

func (a *Admin) ListDatabases(_ context.Context, req *adminpb.ListDatabasesRequest) (*adminpb.ListDatabasesResponse, error) {
	m := projectRe.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}", req.GetParent())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seed(m[1])
	resp := &adminpb.ListDatabasesResponse{}
	for name, db := range a.databases {
		if strings.HasPrefix(name, req.GetParent()+"/databases/") && (db.DeleteTime == nil || req.GetShowDeleted()) {
			resp.Databases = append(resp.Databases, a.describe(db))
		}
	}
	sort.Slice(resp.Databases, func(i, j int) bool { return resp.Databases[i].Name < resp.Databases[j].Name })
	return resp, nil
}

func (a *Admin) GetDatabase(_ context.Context, req *adminpb.GetDatabaseRequest) (*adminpb.Database, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	db, err := a.database(req.GetName())
	if err != nil {
		return nil, err
	}
	return a.describe(db), nil
}

func (a *Admin) CreateDatabase(_ context.Context, req *adminpb.CreateDatabaseRequest) (*longrunningpb.Operation, error) {
	m := projectRe.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}", req.GetParent())
	}
	id := req.GetDatabaseId()
	if id != DefaultDatabase && (!databaseIDRe.MatchString(id) || uuidLikeRe.MatchString(id)) {
		return nil, status.Errorf(codes.InvalidArgument, "Database ID %q is invalid: it must be 4-63 characters of lowercase letters, numbers and hyphens, start with a letter and end with a letter or number, or be \"(default)\".", id)
	}
	in := req.GetDatabase()
	if in == nil {
		in = &adminpb.Database{}
	}
	if in.GetLocationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "location_id is required.")
	}
	switch in.GetType() {
	case adminpb.Database_DATASTORE_MODE:
		return nil, status.Error(codes.Unimplemented, "a Datastore mode database is not served here: the Firestore emulator serves Native mode, and Datastore mode is the Datastore service")
	case adminpb.Database_DATABASE_TYPE_UNSPECIFIED:
		return nil, status.Error(codes.InvalidArgument, "type is required: FIRESTORE_NATIVE or DATASTORE_MODE.")
	}
	if in.GetDatabaseEdition() == adminpb.Database_ENTERPRISE {
		return nil, status.Error(codes.Unimplemented, "the Enterprise edition is not served here: the Firestore emulator is the Standard edition")
	}
	if in.GetCmekConfig() != nil {
		return nil, status.Error(codes.Unimplemented, "customer-managed encryption (cmek_config) is not served here: the emulator does not encrypt with Cloud KMS keys")
	}
	name := req.GetParent() + "/databases/" + id
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seed(m[1])
	if old, ok := a.databases[name]; ok && old.DeleteTime == nil {
		return nil, status.Errorf(codes.AlreadyExists, "Database already exists. Please use another database_id")
	}
	now := a.now()
	db := &adminpb.Database{
		Name: name, Uid: uuid(), CreateTime: timestamppb.New(now), UpdateTime: timestamppb.New(now),
		LocationId: in.GetLocationId(), Type: in.GetType(),
		ConcurrencyMode:               orDefault(in.GetConcurrencyMode(), adminpb.Database_PESSIMISTIC),
		PointInTimeRecoveryEnablement: orDefault(in.GetPointInTimeRecoveryEnablement(), adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED),
		AppEngineIntegrationMode:      orDefault(in.GetAppEngineIntegrationMode(), adminpb.Database_DISABLED),
		DeleteProtectionState:         orDefault(in.GetDeleteProtectionState(), adminpb.Database_DELETE_PROTECTION_DISABLED),
		DatabaseEdition:               adminpb.Database_STANDARD,
		Tags:                          in.GetTags(),
	}
	a.databases[name] = db
	return a.done(name, &adminpb.CreateDatabaseMetadata{}, a.describe(db))
}

func orDefault[E ~int32](v, def E) E {
	if v == 0 {
		return def
	}
	return v
}

// mutableDatabaseFields are the fields UpdateDatabase changes, by mask path.
var mutableDatabaseFields = map[string]func(dst, src *adminpb.Database){
	"concurrency_mode": func(d, s *adminpb.Database) {
		d.ConcurrencyMode = orDefault(s.ConcurrencyMode, adminpb.Database_PESSIMISTIC)
	},
	"point_in_time_recovery_enablement": func(d, s *adminpb.Database) {
		d.PointInTimeRecoveryEnablement = orDefault(s.PointInTimeRecoveryEnablement, adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED)
	},
	"app_engine_integration_mode": func(d, s *adminpb.Database) {
		d.AppEngineIntegrationMode = orDefault(s.AppEngineIntegrationMode, adminpb.Database_DISABLED)
	},
	"delete_protection_state": func(d, s *adminpb.Database) {
		d.DeleteProtectionState = orDefault(s.DeleteProtectionState, adminpb.Database_DELETE_PROTECTION_DISABLED)
	},
}

// snake turns a REST mask path (camelCase) into the proto field name.
func snake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (a *Admin) UpdateDatabase(_ context.Context, req *adminpb.UpdateDatabaseRequest) (*longrunningpb.Operation, error) {
	in := req.GetDatabase()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "database is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		for p := range mutableDatabaseFields {
			paths = append(paths, p)
		}
		sort.Strings(paths)
	}
	for i, p := range paths {
		paths[i] = snake(p)
		if mutableDatabaseFields[paths[i]] == nil {
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q cannot be updated: only concurrency_mode, point_in_time_recovery_enablement, app_engine_integration_mode and delete_protection_state can", p)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	db, err := a.database(in.GetName())
	if err != nil {
		return nil, err
	}
	if in.GetEtag() != "" && in.GetEtag() != etag(a.describe(db)) {
		return nil, status.Error(codes.Aborted, "the database's etag does not match: it has changed since it was read")
	}
	for _, p := range paths {
		mutableDatabaseFields[p](db, in)
	}
	db.UpdateTime = timestamppb.New(a.now())
	return a.done(db.Name, &adminpb.UpdateDatabaseMetadata{}, a.describe(db))
}

func (a *Admin) DeleteDatabase(ctx context.Context, req *adminpb.DeleteDatabaseRequest) (*longrunningpb.Operation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	db, err := a.database(req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetEtag() != "" && req.GetEtag() != etag(a.describe(db)) {
		return nil, status.Error(codes.Aborted, "the database's etag does not match: it has changed since it was read")
	}
	if db.DeleteProtectionState == adminpb.Database_DELETE_PROTECTION_ENABLED {
		return nil, status.Errorf(codes.FailedPrecondition, "The database %s has delete protection enabled. Disable delete protection before deleting it.", db.Name)
	}
	if err := a.clearDocuments(ctx, db.Name); err != nil {
		return nil, err
	}
	for n := range a.indexes {
		if strings.HasPrefix(n, db.Name+"/") {
			delete(a.indexes, n)
		}
	}
	for n := range a.fields {
		if strings.HasPrefix(n, db.Name+"/") {
			delete(a.fields, n)
		}
	}
	gone := a.describe(db)
	gone.DeleteTime = timestamppb.New(a.now())
	delete(a.databases, db.Name)
	return a.done(db.Name, &adminpb.DeleteDatabaseMetadata{}, gone)
}

// clearDocuments deletes every document of the database in the emulator,
// through the emulator's reset endpoint, which clears one database alone.
func (a *Admin) clearDocuments(ctx context.Context, dbName string) error {
	if a.emulator == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+a.emulator+"/emulator/v1/"+dbName+"/documents", nil)
	if err != nil {
		return status.Errorf(codes.Internal, "clearing %s: %v", dbName, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return status.Errorf(codes.Unavailable, "the emulator did not clear the documents of %s: %v", dbName, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return status.Errorf(codes.Unavailable, "the emulator answered %s clearing the documents of %s", resp.Status, dbName)
	}
	return nil
}

// --- indexes ---

func (a *Admin) CreateIndex(_ context.Context, req *adminpb.CreateIndexRequest) (*longrunningpb.Operation, error) {
	m := cgRe.FindStringSubmatch(req.GetParent())
	if m == nil || m[2] == "-" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/databases/{database}/collectionGroups/{collection_id}", req.GetParent())
	}
	in := proto.Clone(req.GetIndex()).(*adminpb.Index)
	if in == nil || len(in.Fields) == 0 {
		return nil, status.Error(codes.InvalidArgument, "index.fields is required")
	}
	vector := false
	named := 0
	for _, f := range in.Fields {
		if f.GetFieldPath() == "" {
			return nil, status.Error(codes.InvalidArgument, "every index field needs a field_path")
		}
		if f.GetValueMode() == nil {
			return nil, status.Errorf(codes.InvalidArgument, "index field %q needs one of order, array_config or vector_config", f.GetFieldPath())
		}
		if f.GetVectorConfig() != nil {
			vector = true
		}
		if f.GetFieldPath() != "__name__" {
			named++
		}
	}
	if named < 2 && !vector {
		return nil, status.Error(codes.InvalidArgument, "this index is not necessary, configure using single field index controls")
	}
	if in.QueryScope == adminpb.Index_QUERY_SCOPE_UNSPECIFIED {
		in.QueryScope = adminpb.Index_COLLECTION
	}
	// Google appends __name__, in the direction of the last field, to an
	// index that does not end in it.
	if last := in.Fields[len(in.Fields)-1]; last.GetFieldPath() != "__name__" && !vector {
		order := adminpb.Index_IndexField_ASCENDING
		if last.GetOrder() == adminpb.Index_IndexField_DESCENDING {
			order = adminpb.Index_IndexField_DESCENDING
		}
		in.Fields = append(in.Fields, &adminpb.Index_IndexField{FieldPath: "__name__",
			ValueMode: &adminpb.Index_IndexField_Order_{Order: order}})
	}
	in.State = adminpb.Index_READY
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.database(m[1]); err != nil {
		return nil, err
	}
	for n, x := range a.indexes {
		if strings.HasPrefix(n, req.GetParent()+"/indexes/") && sameIndex(x, in) {
			return nil, status.Error(codes.AlreadyExists, "index already exists")
		}
	}
	in.Name = req.GetParent() + "/indexes/" + "CICAg" + randomID(6)
	a.indexes[in.Name] = in
	now := timestamppb.New(a.now())
	return a.done(m[1], &adminpb.IndexOperationMetadata{StartTime: now, EndTime: now, Index: in.Name,
		State: adminpb.OperationState_SUCCESSFUL, ProgressDocuments: &adminpb.Progress{}, ProgressBytes: &adminpb.Progress{}},
		proto.Clone(in).(*adminpb.Index))
}

func sameIndex(x, y *adminpb.Index) bool {
	c := func(i *adminpb.Index) *adminpb.Index {
		i = proto.Clone(i).(*adminpb.Index)
		i.Name, i.State = "", adminpb.Index_STATE_UNSPECIFIED
		return i
	}
	return proto.Equal(c(x), c(y))
}

func (a *Admin) ListIndexes(_ context.Context, req *adminpb.ListIndexesRequest) (*adminpb.ListIndexesResponse, error) {
	m := cgRe.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/databases/{database}/collectionGroups/{collection_id}", req.GetParent())
	}
	if req.GetFilter() != "" {
		return nil, status.Errorf(codes.InvalidArgument, "filter %q is not supported", req.GetFilter())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.database(m[1]); err != nil {
		return nil, err
	}
	var all []*adminpb.Index
	for n, x := range a.indexes {
		im := indexNameRe.FindStringSubmatch(n)
		if im[1] == m[1] && (m[2] == "-" || im[2] == m[2]) {
			all = append(all, proto.Clone(x).(*adminpb.Index))
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	page, next, err := paginate(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &adminpb.ListIndexesResponse{Indexes: all[page[0]:page[1]], NextPageToken: next}, nil
}

func (a *Admin) GetIndex(_ context.Context, req *adminpb.GetIndexRequest) (*adminpb.Index, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	x, err := a.index(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(x).(*adminpb.Index), nil
}

func (a *Admin) DeleteIndex(_ context.Context, req *adminpb.DeleteIndexRequest) (*emptypb.Empty, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.index(req.GetName()); err != nil {
		return nil, err
	}
	delete(a.indexes, req.GetName())
	return &emptypb.Empty{}, nil
}

// index is the index named name. a.mu is held.
func (a *Admin) index(name string) (*adminpb.Index, error) {
	m := indexNameRe.FindStringSubmatch(name)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid index name %q", name)
	}
	if _, err := a.database(m[1]); err != nil {
		return nil, err
	}
	x, ok := a.indexes[name]
	if !ok {
		return nil, status.Error(codes.NotFound, "index not found")
	}
	return x, nil
}

// paginate is the [start, end) of a page of n items, and the next token.
func paginate(n int, size int32, token string) ([2]int, string, error) {
	start := 0
	if token != "" {
		v, err := strconv.Atoi(token)
		if err != nil || v < 0 || v > n {
			return [2]int{}, "", status.Errorf(codes.InvalidArgument, "invalid page_token %q", token)
		}
		start = v
	}
	end := n
	if size > 0 && start+int(size) < n {
		end = start + int(size)
	}
	next := ""
	if end < n {
		next = strconv.Itoa(end)
	}
	return [2]int{start, end}, next, nil
}

// --- fields ---

const defaultCG, defaultField = "__default__", "*"

// defaultIndexes are the single-field indexes Firestore keeps for a field
// that has no override: ascending, descending and array-contains, at
// collection scope.
func defaultIndexes(fieldPath string) []*adminpb.Index {
	mk := func(v isValueMode) *adminpb.Index {
		f := &adminpb.Index_IndexField{FieldPath: fieldPath}
		v.set(f)
		return &adminpb.Index{QueryScope: adminpb.Index_COLLECTION, Fields: []*adminpb.Index_IndexField{f}, State: adminpb.Index_READY}
	}
	return []*adminpb.Index{
		mk(order(adminpb.Index_IndexField_ASCENDING)),
		mk(order(adminpb.Index_IndexField_DESCENDING)),
		mk(arrayContains{}),
	}
}

type isValueMode interface {
	set(*adminpb.Index_IndexField)
}
type order adminpb.Index_IndexField_Order
type arrayContains struct{}

func (o order) set(f *adminpb.Index_IndexField) {
	f.ValueMode = &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_Order(o)}
}
func (arrayContains) set(f *adminpb.Index_IndexField) {
	f.ValueMode = &adminpb.Index_IndexField_ArrayConfig_{ArrayConfig: adminpb.Index_IndexField_CONTAINS}
}

// field is the field as Google describes it: its override, or the
// database's default settings it inherits. a.mu is held.
func (a *Admin) field(name string) (*adminpb.Field, []string, error) {
	m := fieldNameRe.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "invalid field name %q: want projects/{project}/databases/{database}/collectionGroups/{collection_id}/fields/{field_id}", name)
	}
	if _, err := a.database(m[1]); err != nil {
		return nil, nil, err
	}
	ancestor := m[1] + "/collectionGroups/" + defaultCG + "/fields/" + defaultField
	if f, ok := a.fields[name]; ok {
		out := proto.Clone(f).(*adminpb.Field)
		if out.IndexConfig == nil {
			// A TTL policy alone: the index settings are still inherited.
			out.IndexConfig = a.inherited(name, ancestor, m[3])
		}
		return out, m, nil
	}
	if name == ancestor {
		return &adminpb.Field{Name: name, IndexConfig: &adminpb.Field_IndexConfig{Indexes: defaultIndexes(defaultField)}}, m, nil
	}
	return &adminpb.Field{Name: name, IndexConfig: a.inherited(name, ancestor, m[3])}, m, nil
}

// inherited is the index settings a field without its own inherits from
// the database's default ones. a.mu is held.
func (a *Admin) inherited(name, ancestor, fieldPath string) *adminpb.Field_IndexConfig {
	if name == ancestor {
		return &adminpb.Field_IndexConfig{Indexes: defaultIndexes(defaultField)}
	}
	cfg := &adminpb.Field_IndexConfig{Indexes: defaultIndexes(fieldPath), UsesAncestorConfig: true, AncestorField: ancestor}
	if d, ok := a.fields[ancestor]; ok && d.IndexConfig != nil {
		cfg.Indexes = nil
		for _, x := range d.IndexConfig.Indexes {
			x = proto.Clone(x).(*adminpb.Index)
			for _, f := range x.Fields {
				f.FieldPath = fieldPath
			}
			cfg.Indexes = append(cfg.Indexes, x)
		}
	}
	return cfg
}

func (a *Admin) GetField(_ context.Context, req *adminpb.GetFieldRequest) (*adminpb.Field, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, _, err := a.field(req.GetName())
	return f, err
}

func (a *Admin) UpdateField(_ context.Context, req *adminpb.UpdateFieldRequest) (*longrunningpb.Operation, error) {
	in := req.GetField()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "field is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"index_config", "ttl_config"}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, m, err := a.field(in.GetName())
	if err != nil {
		return nil, err
	}
	isDefault := m[2] == defaultCG && m[3] == defaultField
	meta := &adminpb.FieldOperationMetadata{Field: cur.Name, State: adminpb.OperationState_SUCCESSFUL,
		ProgressDocuments: &adminpb.Progress{}, ProgressBytes: &adminpb.Progress{}}
	override, has := a.fields[cur.Name]
	if !has {
		override = &adminpb.Field{Name: cur.Name}
	}
	var indexBefore []*adminpb.Index
	indexChanged := false
	for _, p := range paths {
		switch snake(p) {
		case "index_config":
			indexChanged = true
			before := cur.GetIndexConfig().GetIndexes()
			if in.GetIndexConfig() == nil {
				override.IndexConfig = nil
				if isDefault {
					return nil, status.Error(codes.InvalidArgument, "the default index settings cannot be removed, only changed")
				}
			} else {
				var idx []*adminpb.Index
				for _, x := range in.GetIndexConfig().GetIndexes() {
					x = proto.Clone(x).(*adminpb.Index)
					if len(x.Fields) != 1 || x.Fields[0].GetValueMode() == nil {
						return nil, status.Error(codes.InvalidArgument, "a single-field index has one field, with an order or an array_config")
					}
					if x.Fields[0].FieldPath == "" {
						x.Fields[0].FieldPath = m[3]
					}
					if x.QueryScope == adminpb.Index_QUERY_SCOPE_UNSPECIFIED {
						x.QueryScope = adminpb.Index_COLLECTION
					}
					x.State = adminpb.Index_READY
					idx = append(idx, x)
				}
				override.IndexConfig = &adminpb.Field_IndexConfig{Indexes: idx}
			}
			indexBefore = before
		case "ttl_config":
			if isDefault && in.GetTtlConfig() != nil {
				return nil, status.Error(codes.InvalidArgument, "a TTL policy cannot be set on the default field settings")
			}
			if in.GetTtlConfig() == nil {
				if override.TtlConfig != nil {
					meta.TtlConfigDelta = &adminpb.FieldOperationMetadata_TtlConfigDelta{ChangeType: adminpb.FieldOperationMetadata_TtlConfigDelta_REMOVE}
				}
				override.TtlConfig = nil
			} else {
				if override.TtlConfig == nil {
					meta.TtlConfigDelta = &adminpb.FieldOperationMetadata_TtlConfigDelta{ChangeType: adminpb.FieldOperationMetadata_TtlConfigDelta_ADD}
				}
				override.TtlConfig = &adminpb.Field_TtlConfig{State: adminpb.Field_TtlConfig_ACTIVE}
			}
		default:
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q cannot be updated: only index_config and ttl_config can", p)
		}
	}
	if override.IndexConfig == nil && override.TtlConfig == nil {
		delete(a.fields, cur.Name)
	} else {
		a.fields[cur.Name] = override
	}
	out, _, _ := a.field(cur.Name)
	if indexChanged {
		meta.IndexConfigDeltas = deltas(indexBefore, out.GetIndexConfig().GetIndexes())
	}
	now := timestamppb.New(a.now())
	meta.StartTime, meta.EndTime = now, now
	return a.done(m[1], meta, out)
}

func deltas(before, after []*adminpb.Index) []*adminpb.FieldOperationMetadata_IndexConfigDelta {
	in := func(x *adminpb.Index, set []*adminpb.Index) bool {
		for _, y := range set {
			if sameIndex(x, y) {
				return true
			}
		}
		return false
	}
	var out []*adminpb.FieldOperationMetadata_IndexConfigDelta
	for _, x := range after {
		if !in(x, before) {
			out = append(out, &adminpb.FieldOperationMetadata_IndexConfigDelta{ChangeType: adminpb.FieldOperationMetadata_IndexConfigDelta_ADD, Index: x})
		}
	}
	for _, x := range before {
		if !in(x, after) {
			out = append(out, &adminpb.FieldOperationMetadata_IndexConfigDelta{ChangeType: adminpb.FieldOperationMetadata_IndexConfigDelta_REMOVE, Index: x})
		}
	}
	return out
}

func (a *Admin) ListFields(_ context.Context, req *adminpb.ListFieldsRequest) (*adminpb.ListFieldsResponse, error) {
	m := cgRe.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/databases/{database}/collectionGroups/{collection_id}", req.GetParent())
	}
	var keep func(*adminpb.Field) bool
	switch strings.ReplaceAll(req.GetFilter(), " ", "") {
	case "indexConfig.usesAncestorConfig:false", "index_config.uses_ancestor_config:false":
		keep = func(f *adminpb.Field) bool { return f.IndexConfig != nil }
	case "ttlConfig:*", "ttl_config:*":
		keep = func(f *adminpb.Field) bool { return f.TtlConfig != nil }
	default:
		return nil, status.Error(codes.InvalidArgument, "ListFields lists explicitly overridden fields alone: the filter must be indexConfig.usesAncestorConfig:false or ttlConfig:*")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.database(m[1]); err != nil {
		return nil, err
	}
	var all []*adminpb.Field
	for n, f := range a.fields {
		fm := fieldNameRe.FindStringSubmatch(n)
		if fm[1] == m[1] && (m[2] == "-" || fm[2] == m[2]) && keep(f) {
			out, _, _ := a.field(n)
			all = append(all, out)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	page, next, err := paginate(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &adminpb.ListFieldsResponse{Fields: all[page[0]:page[1]], NextPageToken: next}, nil
}

// --- refused ---

const (
	noExport  = "is not served here: the emulator has no managed export or import, and an export it could write would not be in Firestore's own format. Use the emulator's export (gcloud emulators firestore start --export-on-exit) instead"
	noBackups = "is not served here: the emulator keeps no backups or versions to restore from"
	noCreds   = "is not served here: user credentials are for Firestore with MongoDB compatibility, which the emulator is not"
)

func refuse(method, why string) error {
	return status.Errorf(codes.Unimplemented, "%s %s", method, why)
}

func (a *Admin) ExportDocuments(context.Context, *adminpb.ExportDocumentsRequest) (*longrunningpb.Operation, error) {
	return nil, refuse("ExportDocuments", noExport)
}
func (a *Admin) ImportDocuments(context.Context, *adminpb.ImportDocumentsRequest) (*longrunningpb.Operation, error) {
	return nil, refuse("ImportDocuments", noExport)
}
func (a *Admin) BulkDeleteDocuments(context.Context, *adminpb.BulkDeleteDocumentsRequest) (*longrunningpb.Operation, error) {
	return nil, refuse("BulkDeleteDocuments", "is not served here: the emulator has no bulk delete by collection; delete the documents, or the database")
}
func (a *Admin) GetBackup(context.Context, *adminpb.GetBackupRequest) (*adminpb.Backup, error) {
	return nil, refuse("GetBackup", noBackups)
}
func (a *Admin) ListBackups(context.Context, *adminpb.ListBackupsRequest) (*adminpb.ListBackupsResponse, error) {
	return nil, refuse("ListBackups", noBackups)
}
func (a *Admin) DeleteBackup(context.Context, *adminpb.DeleteBackupRequest) (*emptypb.Empty, error) {
	return nil, refuse("DeleteBackup", noBackups)
}
func (a *Admin) RestoreDatabase(context.Context, *adminpb.RestoreDatabaseRequest) (*longrunningpb.Operation, error) {
	return nil, refuse("RestoreDatabase", noBackups)
}
func (a *Admin) CloneDatabase(context.Context, *adminpb.CloneDatabaseRequest) (*longrunningpb.Operation, error) {
	return nil, refuse("CloneDatabase", noBackups)
}
func (a *Admin) CreateBackupSchedule(context.Context, *adminpb.CreateBackupScheduleRequest) (*adminpb.BackupSchedule, error) {
	return nil, refuse("CreateBackupSchedule", noBackups)
}
func (a *Admin) GetBackupSchedule(context.Context, *adminpb.GetBackupScheduleRequest) (*adminpb.BackupSchedule, error) {
	return nil, refuse("GetBackupSchedule", noBackups)
}
func (a *Admin) ListBackupSchedules(context.Context, *adminpb.ListBackupSchedulesRequest) (*adminpb.ListBackupSchedulesResponse, error) {
	return nil, refuse("ListBackupSchedules", noBackups)
}
func (a *Admin) UpdateBackupSchedule(context.Context, *adminpb.UpdateBackupScheduleRequest) (*adminpb.BackupSchedule, error) {
	return nil, refuse("UpdateBackupSchedule", noBackups)
}
func (a *Admin) DeleteBackupSchedule(context.Context, *adminpb.DeleteBackupScheduleRequest) (*emptypb.Empty, error) {
	return nil, refuse("DeleteBackupSchedule", noBackups)
}
func (a *Admin) CreateUserCreds(context.Context, *adminpb.CreateUserCredsRequest) (*adminpb.UserCreds, error) {
	return nil, refuse("CreateUserCreds", noCreds)
}
func (a *Admin) GetUserCreds(context.Context, *adminpb.GetUserCredsRequest) (*adminpb.UserCreds, error) {
	return nil, refuse("GetUserCreds", noCreds)
}
func (a *Admin) ListUserCreds(context.Context, *adminpb.ListUserCredsRequest) (*adminpb.ListUserCredsResponse, error) {
	return nil, refuse("ListUserCreds", noCreds)
}
func (a *Admin) EnableUserCreds(context.Context, *adminpb.EnableUserCredsRequest) (*adminpb.UserCreds, error) {
	return nil, refuse("EnableUserCreds", noCreds)
}
func (a *Admin) DisableUserCreds(context.Context, *adminpb.DisableUserCredsRequest) (*adminpb.UserCreds, error) {
	return nil, refuse("DisableUserCreds", noCreds)
}
func (a *Admin) ResetUserPassword(context.Context, *adminpb.ResetUserPasswordRequest) (*adminpb.UserCreds, error) {
	return nil, refuse("ResetUserPassword", noCreds)
}
func (a *Admin) DeleteUserCreds(context.Context, *adminpb.DeleteUserCredsRequest) (*emptypb.Empty, error) {
	return nil, refuse("DeleteUserCreds", noCreds)
}

// --- operations ---

// done records a finished operation under the database dbName.
// a.mu is held.
func (a *Admin) done(dbName string, meta, resp proto.Message) (*longrunningpb.Operation, error) {
	m, err := anypb.New(meta)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation metadata: %v", err)
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation response: %v", err)
	}
	op := &longrunningpb.Operation{Name: dbName + "/operations/" + randomID(18), Metadata: m, Done: true,
		Result: &longrunningpb.Operation_Response{Response: r}}
	a.ops[op.Name] = op
	a.opOrder = append(a.opOrder, op.Name)
	// Keep the last 1000, so a long-lived instance does not grow forever.
	if len(a.opOrder) > 1000 {
		delete(a.ops, a.opOrder[0])
		a.opOrder = a.opOrder[1:]
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

func (a *Admin) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	op, ok := a.ops[req.GetName()]
	if !ok {
		if opNameRe.MatchString(req.GetName()) {
			return nil, status.Errorf(codes.NotFound, "Operation %s not found.", req.GetName())
		}
		return nil, status.Errorf(codes.InvalidArgument, "invalid operation name %q", req.GetName())
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

func (a *Admin) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	return a.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
}

func (a *Admin) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	parent := strings.TrimSuffix(req.GetName(), "/operations")
	if !dbNameRe.MatchString(parent) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name %q: want projects/{project}/databases/{database}", req.GetName())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var all []*longrunningpb.Operation
	for i := len(a.opOrder) - 1; i >= 0; i-- {
		if n := a.opOrder[i]; strings.HasPrefix(n, parent+"/operations/") {
			all = append(all, proto.Clone(a.ops[n]).(*longrunningpb.Operation))
		}
	}
	page, next, err := paginate(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &longrunningpb.ListOperationsResponse{Operations: all[page[0]:page[1]], NextPageToken: next}, nil
}

func (a *Admin) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.ops[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "Operation %s not found.", req.GetName())
	}
	delete(a.ops, req.GetName())
	return &emptypb.Empty{}, nil
}

// CancelOperation of a finished operation does nothing, as Google's does.
func (a *Admin) CancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	if _, err := a.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
