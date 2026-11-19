package queryfront

// Named Datastore databases (#1885). The Datastore emulator keeps a key's
// database in its entity store, so a lookup in one named database misses
// another's entities, but its query indexes are kept by project and
// namespace alone: a query in one named database of a project returns the
// entities of every database of it, its counts count them, and a kindless
// or metadata query lists their kinds and namespaces. Google keeps each
// database apart, and every call, lookups, queries, aggregations, GQL and
// transactions alike, sees the named database alone.
//
// The front keeps them apart by giving each named database a project of its
// own in the emulator: database d of project p is the emulator's project
// EmulatorProject(p, d), its default database. Every Datastore call naming
// a named database is rewritten on its way to the emulator (the request's
// project and database, every partition ID in it, and its routing header)
// and its answer back (every partition ID naming such a project names p and
// d again), so the emulator's own queries, cursors, offsets, limits,
// metadata kinds and transactions are scoped exactly by database. Calls to
// the default database pass unchanged.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// emulatorProjectPrefix begins every emulator project a named database is
// kept in. With it the ID is longer than 30 characters, which no project of
// Google's can have.
const emulatorProjectPrefix = "cloudburrow-database-"

// NamedDatabase reports whether database is a named database, not the
// default one ("" or "(default)").
func NamedDatabase(database string) bool { return database != "" && database != "(default)" }

// EmulatorProject is the emulator project named database d of project p is
// kept in: the prefix, d's length, d and p, so it is told back exactly.
func EmulatorProject(p, d string) string {
	return emulatorProjectPrefix + strconv.Itoa(len(d)) + "-" + d + "-" + p
}

// FromEmulatorProject is the project and named database an emulator project
// keeps, if it keeps one.
func FromEmulatorProject(ep string) (project, database string, ok bool) {
	rest, ok := strings.CutPrefix(ep, emulatorProjectPrefix)
	if !ok {
		return "", "", false
	}
	n, rest, ok := strings.Cut(rest, "-")
	l, err := strconv.Atoi(n)
	if !ok || err != nil || l <= 0 || len(rest) < l+2 || rest[l] != '-' {
		return "", "", false
	}
	return rest[l+1:], rest[:l], true
}

// walkPartitions calls f on every partition ID in m.
func walkPartitions(m protoreflect.Message, f func(*pb.PartitionId)) {
	if p, ok := m.Interface().(*pb.PartitionId); ok {
		f(p)
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					walkPartitions(mv.Message(), f)
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					walkPartitions(l.Get(i).Message(), f)
				}
			}
		case fd.Message() != nil:
			walkPartitions(v.Message(), f)
		}
		return true
	})
}

// toEmulator rewrites a partition ID of a call to project's database: one
// naming a named database names its emulator project.
func toEmulator(p *pb.PartitionId, project string) {
	if !NamedDatabase(p.GetDatabaseId()) {
		return
	}
	if p.ProjectId == "" {
		p.ProjectId = project
	}
	p.ProjectId, p.DatabaseId = EmulatorProject(p.ProjectId, p.DatabaseId), ""
}

// fromEmulator rewrites a partition ID of the emulator's answer back.
func fromEmulator(p *pb.PartitionId) {
	if project, database, ok := FromEmulatorProject(p.GetProjectId()); ok {
		p.ProjectId, p.DatabaseId = project, database
	}
}

// request is a Datastore request: every one has a project_id and a
// database_id.
type request interface {
	proto.Message
	GetProjectId() string
	GetDatabaseId() string
}

// callKeys are the keys of a call that name the entities it reads or
// writes, which, with no database of their own, are the call's.
func callKeys(req proto.Message) []*pb.Key {
	var keys []*pb.Key
	switch r := req.(type) {
	case *pb.LookupRequest:
		keys = r.GetKeys()
	case *pb.AllocateIdsRequest:
		keys = r.GetKeys()
	case *pb.ReserveIdsRequest:
		keys = r.GetKeys()
	case *pb.CommitRequest:
		for _, m := range r.GetMutations() {
			switch op := m.GetOperation().(type) {
			case *pb.Mutation_Insert:
				keys = append(keys, op.Insert.GetKey())
			case *pb.Mutation_Update:
				keys = append(keys, op.Update.GetKey())
			case *pb.Mutation_Upsert:
				keys = append(keys, op.Upsert.GetKey())
			case *pb.Mutation_Delete:
				keys = append(keys, op.Delete)
			}
		}
	}
	return keys
}

// callPartitions are the call's own partition IDs: its keys' and its
// query's.
func callPartitions(req proto.Message) []*pb.PartitionId {
	var out []*pb.PartitionId
	for _, k := range callKeys(req) {
		if k != nil && k.PartitionId != nil {
			out = append(out, k.PartitionId)
		}
	}
	switch r := req.(type) {
	case *pb.RunQueryRequest:
		out = append(out, r.GetPartitionId())
	case *pb.RunAggregationQueryRequest:
		out = append(out, r.GetPartitionId())
	}
	return out
}

// mapRequest is req, if it names a named database, rewritten to its
// emulator project, and whether it was.
func mapRequest[R request](req R) (R, bool) {
	project, database := req.GetProjectId(), req.GetDatabaseId()
	if !NamedDatabase(database) {
		return req, false
	}
	out := proto.Clone(req).(R)
	// A partition of the call's own that names the project and no
	// database is the call's database.
	for _, p := range callPartitions(out) {
		if p != nil && p.DatabaseId == "" && (p.ProjectId == "" || p.ProjectId == project) {
			p.DatabaseId = database
		}
	}
	walkPartitions(out.ProtoReflect(), func(p *pb.PartitionId) { toEmulator(p, project) })
	m := out.ProtoReflect()
	fields := m.Descriptor().Fields()
	m.Set(fields.ByName("project_id"), protoreflect.ValueOfString(EmulatorProject(project, database)))
	m.Set(fields.ByName("database_id"), protoreflect.ValueOfString(""))
	return out, true
}

// mapResponse rewrites the emulator's answer back.
func mapResponse(m proto.Message) {
	if m != nil {
		walkPartitions(m.ProtoReflect(), fromEmulator)
	}
}

// routedTo is ctx with the outgoing routing header of the emulator project.
func routedTo(ctx context.Context, emulatorProject string) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("x-goog-request-params", "project_id="+url.QueryEscape(emulatorProject))
	md.Delete("google-cloud-resource-prefix")
	return metadata.NewOutgoingContext(ctx, md)
}

// databasesClient is a Datastore client of the emulator that keeps named
// databases in their emulator projects.
type databasesClient struct {
	c pb.DatastoreClient
	// hold, if not nil, is closed when the named databases' entities
	// kept the old way have been moved (callfront's MigrateDatabases):
	// until then a call naming a named database waits.
	hold <-chan struct{}
}

// DatabasesClient is c, a client of the Datastore emulator, with each named
// database a call names kept in its own emulator project (#1885).
func DatabasesClient(c pb.DatastoreClient) pb.DatastoreClient {
	return DatabasesClientHeld(c, nil)
}

// DatabasesClientHeld is DatabasesClient, whose calls naming a named
// database wait for hold to close, if it is not nil.
func DatabasesClientHeld(c pb.DatastoreClient, hold <-chan struct{}) pb.DatastoreClient {
	if d, ok := c.(databasesClient); ok {
		if hold == nil {
			return d
		}
		c = d.c
	}
	return databasesClient{c, hold}
}

// maxHold is the longest a call waits for the move before it is answered
// UNAVAILABLE, which clients retry.
var maxHold = time.Minute

// waitHold waits for hold to close.
func waitHold(ctx context.Context, hold <-chan struct{}) error {
	if hold == nil {
		return nil
	}
	select {
	case <-hold:
		return nil
	default:
	}
	t := time.NewTimer(maxHold)
	defer t.Stop()
	select {
	case <-hold:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-t.C:
		return status.Error(codes.Unavailable, "the Datastore emulator is moving named databases' entities to their own projects; retry")
	}
}

func mappedHeld[Req request, Resp proto.Message](ctx context.Context, hold <-chan struct{}, req Req, call func(context.Context, Req, ...grpc.CallOption) (Resp, error), opts []grpc.CallOption) (Resp, error) {
	out, ok := mapRequest(req)
	if !ok {
		return call(ctx, req, opts...)
	}
	if err := waitHold(ctx, hold); err != nil {
		var zero Resp
		return zero, err
	}
	resp, err := call(routedTo(ctx, out.GetProjectId()), out, opts...)
	if err == nil {
		mapResponse(resp)
	}
	return resp, err
}

func (d databasesClient) Lookup(ctx context.Context, in *pb.LookupRequest, opts ...grpc.CallOption) (*pb.LookupResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.Lookup, opts)
}

func (d databasesClient) RunQuery(ctx context.Context, in *pb.RunQueryRequest, opts ...grpc.CallOption) (*pb.RunQueryResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.RunQuery, opts)
}

func (d databasesClient) RunAggregationQuery(ctx context.Context, in *pb.RunAggregationQueryRequest, opts ...grpc.CallOption) (*pb.RunAggregationQueryResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.RunAggregationQuery, opts)
}

func (d databasesClient) BeginTransaction(ctx context.Context, in *pb.BeginTransactionRequest, opts ...grpc.CallOption) (*pb.BeginTransactionResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.BeginTransaction, opts)
}

func (d databasesClient) Commit(ctx context.Context, in *pb.CommitRequest, opts ...grpc.CallOption) (*pb.CommitResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.Commit, opts)
}

func (d databasesClient) Rollback(ctx context.Context, in *pb.RollbackRequest, opts ...grpc.CallOption) (*pb.RollbackResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.Rollback, opts)
}

func (d databasesClient) AllocateIds(ctx context.Context, in *pb.AllocateIdsRequest, opts ...grpc.CallOption) (*pb.AllocateIdsResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.AllocateIds, opts)
}

func (d databasesClient) ReserveIds(ctx context.Context, in *pb.ReserveIdsRequest, opts ...grpc.CallOption) (*pb.ReserveIdsResponse, error) {
	return mappedHeld(ctx, d.hold, in, d.c.ReserveIds, opts)
}

const datastoreService = "/google.datastore.v1.Datastore/"

// datastoreDataREST is a Datastore data call over REST.
var datastoreDataREST = regexp.MustCompile(`^/v1/projects/([^/:]+):(lookup|runQuery|runAggregationQuery|beginTransaction|commit|rollback|allocateIds|reserveIds)$`)

// maxPeek is the largest body read for its database.
const maxPeek = 32 << 20

// namesDatabase is the named database a Datastore data call names,
// "projects/p/databases/d", or "": its routing header's database_id, or,
// with none, its body's (field 9 of every Datastore request, databaseId in
// JSON; the project is field 8, projectId, or the REST path's).
func namesDatabase(r *http.Request) string {
	grpcCall := isGRPC(r)
	var pathProject string
	if grpcCall {
		if !strings.HasPrefix(r.URL.Path, datastoreService) {
			return ""
		}
		for _, h := range r.Header.Values("X-Goog-Request-Params") {
			if q, err := url.ParseQuery(h); err == nil && q.Get("database_id") != "" {
				if db := q.Get("database_id"); NamedDatabase(db) && q.Get("project_id") != "" {
					return "projects/" + q.Get("project_id") + "/databases/" + db
				}
				if !NamedDatabase(q.Get("database_id")) {
					return ""
				}
			}
		}
	} else {
		m := datastoreDataREST.FindStringSubmatch(r.URL.Path)
		if r.Method != http.MethodPost || m == nil {
			return ""
		}
		pathProject = m[1]
	}
	if r.Body == nil || r.ContentLength > maxPeek || r.Header.Get("Content-Encoding") != "" {
		return ""
	}
	orig := r.Body
	b, err := io.ReadAll(io.LimitReader(orig, maxPeek+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), orig), orig}
	if err != nil || len(b) > maxPeek {
		return ""
	}
	var project, db string
	switch {
	case grpcCall:
		// One uncompressed gRPC message: a flag byte, a length, the message.
		if len(b) < 5 || b[0] != 0 {
			return ""
		}
		n := binary.BigEndian.Uint32(b[1:5])
		if uint64(len(b)-5) < uint64(n) {
			return ""
		}
		project, db = protoString(b[5:5+n], 8), protoString(b[5:5+n], 9)
	case strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-protobuf"):
		db = protoString(b, 9)
	default:
		var body struct {
			DatabaseID string `json:"databaseId"`
		}
		if json.Unmarshal(b, &body) == nil {
			db = body.DatabaseID
		}
	}
	if pathProject != "" {
		project = pathProject
	}
	if !NamedDatabase(db) || project == "" {
		return ""
	}
	return "projects/" + project + "/databases/" + db
}

// protoString is string field num of a message: field 8 of every Datastore
// request is its project_id, 9 its database_id.
func protoString(b []byte, num protowire.Number) string {
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return ""
		}
		b = b[l:]
		if n == num && typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(b)
			if l < 0 {
				return ""
			}
			return string(v)
		}
		if l = protowire.ConsumeFieldValue(n, typ, b); l < 0 {
			return ""
		}
		b = b[l:]
	}
	return ""
}

// EntityToEmulator is e, an entity of project p's named database kept the
// way the emulator kept it before #1885 (in p, its key naming the
// database), as the front keeps it now: every partition ID in it, its key's
// and its key-valued properties', naming its database's emulator project.
func EntityToEmulator(e *pb.Entity, p string) *pb.Entity {
	out := proto.Clone(e).(*pb.Entity)
	walkPartitions(out.ProtoReflect(), func(pid *pb.PartitionId) { toEmulator(pid, p) })
	return out
}
