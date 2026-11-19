package callfront

// Copies of a Datastore database's entities (#1884): the reads and writes
// the Datastore Admin API's exports and imports (datastoreadmin.go) and
// the Firestore Admin API's exports, imports, bulk deletes, backups,
// restores and clones of a Datastore mode database make, straight from the
// emulator, past the access gate. The Firestore front reaches them over
// the cluster at frontstate.DatastoreDumpPath, DatastoreLoadPath,
// DatastoreDeletePath, DatastoreExportPath and DatastoreImportPath.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// entityFilter is the entities a copy takes: of the kinds and in the
// namespaces named, every kind and every namespace when empty ("" is the
// default namespace).
type entityFilter struct {
	Kinds      []string `json:"kinds,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

func (f entityFilter) wantsKind(k string) bool {
	if len(f.Kinds) == 0 {
		return true
	}
	for _, w := range f.Kinds {
		if w == k {
			return true
		}
	}
	return false
}

func (f entityFilter) wantsNamespace(ns string) bool {
	if len(f.Namespaces) == 0 {
		return true
	}
	for _, w := range f.Namespaces {
		if w == ns {
			return true
		}
	}
	return false
}

// emulatorDB is the database ID the emulator is sent: "" for (default).
func emulatorDB(database string) string {
	if database == "(default)" {
		return ""
	}
	return database
}

// namespaces is every namespace of the database with an entity, the
// default ("") first.
func (d *datastoreDatabases) namespaces(ctx context.Context, project, dbID string) ([]string, error) {
	keys, err := d.queryAll(ctx, project, dbID, "", "__namespace__", true)
	if err != nil {
		return nil, fmt.Errorf("list the namespaces: %w", err)
	}
	out := []string{""}
	for _, e := range keys {
		if p := e.GetKey().GetPath(); len(p) > 0 && p[len(p)-1].GetName() != "" {
			out = append(out, p[len(p)-1].GetName())
		}
	}
	return out, nil
}

// queryAll is every entity of kind (every kind when "") in the namespace,
// their keys alone when keysOnly.
func (d *datastoreDatabases) queryAll(ctx context.Context, project, dbID, ns, kind string, keysOnly bool) ([]*pb.Entity, error) {
	c, err := d.client()
	if err != nil {
		return nil, err
	}
	ctx = routed(ctx, project, dbID)
	var out []*pb.Entity
	var cursor []byte
	for {
		q := &pb.Query{StartCursor: cursor}
		if keysOnly {
			q.Projection = []*pb.Projection{{Property: &pb.PropertyReference{Name: "__key__"}}}
		}
		if kind != "" {
			q.Kind = []*pb.KindExpression{{Name: kind}}
		}
		resp, err := c.RunQuery(ctx, &pb.RunQueryRequest{ProjectId: project, DatabaseId: dbID,
			PartitionId: &pb.PartitionId{ProjectId: project, DatabaseId: dbID, NamespaceId: ns},
			QueryType:   &pb.RunQueryRequest_Query{Query: q}})
		if err != nil {
			return nil, err
		}
		batch := resp.GetBatch()
		for _, r := range batch.GetEntityResults() {
			// The emulator's queries answer every database's entities
			// (measured: a kind query in one named database returns
			// another's), so those of other databases are dropped
			// by their keys, which name their database (#1885). The
			// metadata kinds' keys name none; a namespace of another
			// database only costs a query that finds nothing.
			if strings.HasPrefix(kind, "__") || r.GetEntity().GetKey().GetPartitionId().GetDatabaseId() == dbID {
				out = append(out, r.GetEntity())
			}
		}
		if batch.GetMoreResults() != pb.QueryResultBatch_NOT_FINISHED || len(batch.GetEntityResults()) == 0 {
			return out, nil
		}
		cursor = batch.GetEndCursor()
	}
}

// kindOf is the kind of the entity a key names.
func kindOf(k *pb.Key) string {
	if p := k.GetPath(); len(p) > 0 {
		return p[len(p)-1].GetKind()
	}
	return ""
}

// readEntities is every entity of the database the filter takes, by
// namespace, then key order; the emulator's own (__kind__) are left out.
func (d *datastoreDatabases) readEntities(ctx context.Context, project, database string, f entityFilter) ([]*pb.Entity, error) {
	dbID := emulatorDB(database)
	nss, err := d.namespaces(ctx, project, dbID)
	if err != nil {
		return nil, err
	}
	var out []*pb.Entity
	for _, ns := range nss {
		if !f.wantsNamespace(ns) {
			continue
		}
		kinds := []string{""}
		if len(f.Kinds) > 0 {
			kinds = append([]string(nil), f.Kinds...)
			sort.Strings(kinds)
		}
		for _, k := range kinds {
			es, err := d.queryAll(ctx, project, dbID, ns, k, false)
			if err != nil {
				return nil, fmt.Errorf("read the entities of namespace %q: %w", ns, err)
			}
			for _, e := range es {
				if !strings.HasPrefix(kindOf(e.GetKey()), "__") {
					out = append(out, e)
				}
			}
		}
	}
	return out, nil
}

// commitAll applies muts in commits of at most 500 mutations, Google's
// limit.
func (d *datastoreDatabases) commitAll(ctx context.Context, project, dbID string, muts []*pb.Mutation) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	ctx = routed(ctx, project, dbID)
	for len(muts) > 0 {
		n := min(len(muts), 500)
		if _, err := c.Commit(ctx, &pb.CommitRequest{ProjectId: project, DatabaseId: dbID,
			Mode: pb.CommitRequest_NON_TRANSACTIONAL, Mutations: muts[:n]}); err != nil {
			return err
		}
		muts = muts[n:]
	}
	return nil
}

// writeEntities upserts es into the database, each in its own namespace,
// whichever project and database they were read from.
func (d *datastoreDatabases) writeEntities(ctx context.Context, project, database string, es []*pb.Entity) error {
	dbID := emulatorDB(database)
	muts := make([]*pb.Mutation, 0, len(es))
	for _, e := range es {
		e = proto.Clone(e).(*pb.Entity)
		if e.Key == nil {
			return status.Error(codes.InvalidArgument, "an entity without a key")
		}
		ns := e.Key.GetPartitionId().GetNamespaceId()
		e.Key.PartitionId = &pb.PartitionId{ProjectId: project, DatabaseId: dbID, NamespaceId: ns}
		muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Upsert{Upsert: e}})
	}
	if err := d.commitAll(ctx, project, dbID, muts); err != nil {
		return fmt.Errorf("write the entities: %w", err)
	}
	if dbID != "" {
		d.SawResource("projects/" + project + "/databases/" + dbID)
	}
	return nil
}

// deleteEntities deletes the entities of the database the filter takes,
// and returns how many.
func (d *datastoreDatabases) deleteEntities(ctx context.Context, project, database string, f entityFilter) (int, error) {
	es, err := d.readEntities(ctx, project, database, f)
	if err != nil {
		return 0, err
	}
	muts := make([]*pb.Mutation, 0, len(es))
	for _, e := range es {
		muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Delete{Delete: e.Key}})
	}
	if err := d.commitAll(ctx, project, emulatorDB(database), muts); err != nil {
		return 0, fmt.Errorf("delete the entities: %w", err)
	}
	return len(es), nil
}

func entityBytes(es []*pb.Entity) int64 {
	var n int64
	for _, e := range es {
		n += int64(proto.Size(e))
	}
	return n
}

// copyPaths are the paths of the Firestore front's copies.
var copyPaths = map[string]bool{
	frontstate.DatastoreDumpPath: true, frontstate.DatastoreLoadPath: true, frontstate.DatastoreDeletePath: true,
	frontstate.DatastoreExportPath: true, frontstate.DatastoreImportPath: true,
}

// copyResult is what a copy path answers.
type copyResult struct {
	Entities  int64  `json:"entities"`
	Bytes     int64  `json:"bytes"`
	OutputURI string `json:"outputUri,omitempty"`
}

// copyRequest is a copy path's JSON body.
type copyRequest struct {
	entityFilter
	// OutputURIPrefix is an export's gs://BUCKET[/PATH]; Exact writes
	// the export there, as the Firestore Admin API's ExportDocuments
	// does, rather than in a folder named after its start time, as the
	// Datastore Admin API's ExportEntities does.
	OutputURIPrefix string `json:"outputUriPrefix,omitempty"`
	Exact           bool   `json:"exact,omitempty"`
	// InputURI is an import's export: its folder or its
	// overall_export_metadata file.
	InputURI string `json:"inputUri,omitempty"`
}

// serveCopy answers the copy paths.
func (a *datastoreAdmin) serveCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	project, database := r.URL.Query().Get("project"), r.URL.Query().Get("database")
	if project == "" || database == "" || project == MarkerProject {
		http.Error(w, "project= and database= are required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply := func(res any, err error) {
		if err != nil {
			st, _ := status.FromError(err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": st.Code(), "message": st.Message()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
	if r.URL.Path == frontstate.DatastoreDumpPath {
		es, err := a.dbs.readEntities(ctx, project, database, entityFilter{})
		if err != nil {
			reply(nil, err)
			return
		}
		parts := make([][]byte, 0, len(es))
		for _, e := range es {
			b, err := proto.Marshal(e)
			if err != nil {
				reply(nil, err)
				return
			}
			parts = append(parts, b)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(frontstate.Pack(parts...))
		return
	}
	if r.URL.Path == frontstate.DatastoreLoadPath {
		parts, err := frontstate.Unpack(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		es := make([]*pb.Entity, 0, len(parts))
		for _, p := range parts {
			e := &pb.Entity{}
			if err := proto.Unmarshal(p, e); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			es = append(es, e)
		}
		err = a.dbs.writeEntities(ctx, project, database, es)
		reply(copyResult{Entities: int64(len(es)), Bytes: entityBytes(es)}, err)
		return
	}
	var req copyRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	switch r.URL.Path {
	case frontstate.DatastoreDeletePath:
		n, err := a.dbs.deleteEntities(ctx, project, database, req.entityFilter)
		reply(copyResult{Entities: int64(n)}, err)
	case frontstate.DatastoreExportPath:
		res, err := a.export(ctx, project, database, req.entityFilter, req.OutputURIPrefix, req.Exact)
		reply(res, err)
	case frontstate.DatastoreImportPath:
		res, err := a.importEntities(ctx, project, database, req.entityFilter, req.InputURI)
		reply(res, err)
	}
}
