package callfront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// The named Datastore databases calls name (#1550). The Datastore emulator
// keeps each database's entities apart and serves any database ID it is
// sent, but cannot list a project's databases, so a named database's
// entities would survive the project's purge, which reaches the default
// database's. The Datastore call front keeps every named database a call
// names, from its routing header (x-goog-request-params:
// project_id=p&database_id=d, which netfwd reads into the call's
// resource), as an entity in the emulator itself, in MarkerProject: so the
// record lives exactly as long as the entities it is about, on the
// persistent volume when Datastore is persistent, and goes with them in a
// full reset. The host's purge asks the front for the project's databases
// (ForgetPath), which forgets them as it answers.

// MarkerProject is the project of the emulator the front keeps its
// markers in; no project of Google's can have the ID, which is longer
// than 30 characters.
const MarkerProject = "cloudburrow-datastore-named-databases"

// ForgetPath is frontstate.DatastoreForgetPath.
const ForgetPath = frontstate.DatastoreForgetPath

const markerKind = "Database"

// datastoreDatabases is the front's record of the named databases.
type datastoreDatabases struct {
	addr string
	logf func(string, ...any)

	once sync.Once
	conn *grpc.ClientConn
	err  error

	mu sync.Mutex
	// known is the databases recorded since the front started,
	// "projects/p/databases/d".
	known map[string]bool
}

func newDatastoreDatabases(upstream string, logf func(string, ...any)) *datastoreDatabases {
	return &datastoreDatabases{addr: upstream, logf: logf, known: map[string]bool{}}
}

func (d *datastoreDatabases) client() (pb.DatastoreClient, error) {
	d.once.Do(func() {
		d.conn, d.err = grpc.NewClient(d.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	})
	if d.err != nil {
		return nil, d.err
	}
	return pb.NewDatastoreClient(d.conn), nil
}

// namedDatabase splits "projects/p/databases/d" for a named d.
func namedDatabase(resource string) (project, database string, ok bool) {
	parts := strings.Split(resource, "/")
	if len(parts) < 4 || parts[0] != "projects" || parts[2] != "databases" || parts[1] == "" ||
		parts[3] == "" || parts[3] == "(default)" || parts[1] == MarkerProject {
		return "", "", false
	}
	return parts[1], parts[3], true
}

func markerKey(project, database string) *pb.Key {
	return &pb.Key{PartitionId: &pb.PartitionId{ProjectId: MarkerProject},
		Path: []*pb.Key_PathElement{{Kind: markerKind, IdType: &pb.Key_PathElement_Name{Name: project + "/" + database}}}}
}

// SawResource records a named database a call named.
func (d *datastoreDatabases) SawResource(resource string) {
	project, database, ok := namedDatabase(resource)
	if !ok {
		return
	}
	name := "projects/" + project + "/databases/" + database
	d.mu.Lock()
	seen := d.known[name]
	d.known[name] = true
	d.mu.Unlock()
	if seen {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.mark(ctx, project, database); err != nil {
		d.mu.Lock()
		delete(d.known, name)
		d.mu.Unlock()
		d.logf("call front: record the Datastore database %s: %v", name, err)
	}
}

func (d *datastoreDatabases) mark(ctx context.Context, project, database string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	e := &pb.Entity{Key: markerKey(project, database), Properties: map[string]*pb.Value{
		"project":  {ValueType: &pb.Value_StringValue{StringValue: project}},
		"database": {ValueType: &pb.Value_StringValue{StringValue: database}},
	}}
	_, err = c.Commit(withRouting(ctx), &pb.CommitRequest{ProjectId: MarkerProject, Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Upsert{Upsert: e}}}})
	return err
}

func withRouting(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-goog-request-params", "project_id="+MarkerProject)
}

// forget returns project's named databases, sorted, and forgets them.
func (d *datastoreDatabases) forget(ctx context.Context, project string) ([]string, error) {
	c, err := d.client()
	if err != nil {
		return nil, err
	}
	dbs := map[string]bool{}
	d.mu.Lock()
	prefix := "projects/" + project + "/databases/"
	for n := range d.known {
		if strings.HasPrefix(n, prefix) {
			dbs[strings.TrimPrefix(n, prefix)] = true
			delete(d.known, n)
		}
	}
	d.mu.Unlock()
	resp, err := c.RunQuery(withRouting(ctx), &pb.RunQueryRequest{ProjectId: MarkerProject,
		QueryType: &pb.RunQueryRequest_Query{Query: &pb.Query{
			Kind: []*pb.KindExpression{{Name: markerKind}},
			Filter: &pb.Filter{FilterType: &pb.Filter_PropertyFilter{PropertyFilter: &pb.PropertyFilter{
				Property: &pb.PropertyReference{Name: "project"}, Op: pb.PropertyFilter_EQUAL,
				Value: &pb.Value{ValueType: &pb.Value_StringValue{StringValue: project}}}}},
		}}})
	if err != nil {
		return nil, fmt.Errorf("read the markers: %w", err)
	}
	for _, r := range resp.GetBatch().GetEntityResults() {
		if v := r.GetEntity().GetProperties()["database"].GetStringValue(); v != "" {
			dbs[v] = true
		}
	}
	var muts []*pb.Mutation
	for _, db := range sortedKeys(dbs) {
		muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Delete{Delete: markerKey(project, db)}})
	}
	if len(muts) > 0 {
		if _, err := c.Commit(withRouting(ctx), &pb.CommitRequest{ProjectId: MarkerProject,
			Mode: pb.CommitRequest_NON_TRANSACTIONAL, Mutations: muts}); err != nil {
			return nil, fmt.Errorf("delete the markers: %w", err)
		}
	}
	return sortedKeys(dbs), nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// restCall is a Datastore REST call: POST /v1/projects/{project}:{method}.
var restCall = regexp.MustCompile(`^/v1/projects/([^/:]+):[A-Za-z]+$`)

// maxPeek is the largest REST body read for its databaseId; a larger one
// passes unread.
const maxPeek = 32 << 20

// SawRequest records the named database a Datastore REST call names in its
// body's databaseId, which no header carries; a gRPC call's routing header
// names it, and SawResource reads that.
func (d *datastoreDatabases) SawRequest(r *http.Request) {
	if r.Method != http.MethodPost || strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") ||
		r.Body == nil || r.ContentLength > maxPeek || r.Header.Get("Content-Encoding") != "" {
		return
	}
	m := restCall.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxPeek+1))
	rest := io.Reader(bytes.NewReader(b))
	if err != nil || len(b) > maxPeek {
		// Put back what was read and what is left, unread.
		rest = io.MultiReader(bytes.NewReader(b), r.Body)
	} else if db := restDatabase(r.Header.Get("Content-Type"), b); db != "" {
		d.SawResource("projects/" + m[1] + "/databases/" + db)
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{rest, r.Body}
}

// restDatabase is the database_id a REST body names: databaseId in JSON,
// or field 9 of the request message, which every Datastore request has it
// in, in the protobuf form the Java client sends.
func restDatabase(contentType string, b []byte) string {
	if strings.HasPrefix(contentType, "application/x-protobuf") {
		for len(b) > 0 {
			num, typ, n := protowire.ConsumeTag(b)
			if n < 0 {
				return ""
			}
			b = b[n:]
			if num == 9 && typ == protowire.BytesType {
				v, n := protowire.ConsumeBytes(b)
				if n < 0 {
					return ""
				}
				return string(v)
			}
			n = protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return ""
			}
			b = b[n:]
		}
		return ""
	}
	var body struct {
		DatabaseID string `json:"databaseId"`
	}
	if json.Unmarshal(b, &body) != nil {
		return ""
	}
	return body.DatabaseID
}

// Claims claims ForgetPath.
func (d *datastoreDatabases) Claims(r *http.Request) bool { return r.URL.Path == ForgetPath }

// ServeHTTP answers ForgetPath.
func (d *datastoreDatabases) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	project := r.URL.Query().Get("project")
	if project == "" {
		http.Error(w, "project= is required", http.StatusBadRequest)
		return
	}
	dbs, err := d.forget(r.Context(), project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]string{"databases": dbs})
}
