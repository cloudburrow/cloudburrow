package callfront

// The Datastore front's side of Datastore mode databases (#1880) and
// customer-managed keys (#1881).
//
// The Firestore Admin API, in the Firestore pod's front, is the record of
// every database's type and key. Before the Datastore front passes a data
// call on, it asks that front whether the Datastore API serves the
// database the call names (frontstate.FirestoreAccessPath, over the
// cluster), and answers Google's refusal itself when it does not: a Native
// mode database, or one whose key is not accessible. The answer is kept for
// gateTTL. With Firestore not served there is no Admin API, no Datastore
// mode database and no key, and every call passes, as before.
//
// It also clears a database the Admin API deletes
// (frontstate.DatastoreClearPath): every entity, in every namespace,
// straight from the emulator, past the gate, so a database whose key is
// disabled is deleted too.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/frontstate"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// gateTTL is how long the Admin API's answer for a database is kept.
const gateTTL = time.Second

const datastoreServicePrefix = "/google.datastore.v1.Datastore/"

// datastoreGate refuses the Datastore calls the Firestore Admin API says
// the Datastore API does not serve.
type datastoreGate struct {
	// admin is the Firestore front's address (host:port).
	admin  string
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	answers map[string]gateAnswer
}

type gateAnswer struct {
	at  time.Time
	err error
}

func newDatastoreGate(admin string) *datastoreGate {
	return &datastoreGate{admin: admin, client: &http.Client{Timeout: 10 * time.Second},
		now: time.Now, answers: map[string]gateAnswer{}}
}

// callDatastoreDatabase is the database a Datastore data call names,
// "projects/p/databases/d" ("(default)" for none), or "".
func callDatastoreDatabase(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		if !strings.HasPrefix(r.URL.Path, datastoreServicePrefix) {
			return ""
		}
		res := netfwd.ResourceOf(r.Header)
		parts := strings.Split(res, "/")
		switch {
		case len(parts) == 2 && parts[0] == "projects" && parts[1] != "":
			return res + "/databases/(default)"
		case len(parts) >= 4 && parts[0] == "projects" && parts[2] == "databases":
			return strings.Join(parts[:4], "/")
		}
		return ""
	}
	if r.Method != http.MethodPost || r.Body == nil || r.ContentLength > maxPeek || r.Header.Get("Content-Encoding") != "" {
		return ""
	}
	m := restCall.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxPeek+1))
	rest := io.MultiReader(strings.NewReader(string(b)), r.Body)
	r.Body = struct {
		io.Reader
		io.Closer
	}{rest, r.Body}
	if err != nil || len(b) > maxPeek {
		return ""
	}
	db := restDatabase(r.Header.Get("Content-Type"), b)
	if db == "" {
		db = "(default)"
	}
	return "projects/" + m[1] + "/databases/" + db
}

// access is the refusal the database's calls get, or nil.
func (g *datastoreGate) access(ctx context.Context, db string) error {
	now := g.now()
	g.mu.Lock()
	if a, ok := g.answers[db]; ok && now.Sub(a.at) < gateTTL {
		g.mu.Unlock()
		return a.err
	}
	g.mu.Unlock()
	err := g.ask(ctx, db)
	if status.Code(err) == codes.Unavailable {
		// The Admin API did not answer: not kept, so the next call asks.
		return err
	}
	g.mu.Lock()
	g.answers[db] = gateAnswer{at: now, err: err}
	g.mu.Unlock()
	return err
}

func (g *datastoreGate) ask(ctx context.Context, db string) error {
	u := "http://" + g.admin + frontstate.FirestoreAccessPath + "?api=datastore&name=" + url.QueryEscape(db)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return status.Errorf(codes.Internal, "the database %s: %v", db, err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return status.Errorf(codes.Unavailable, "the Firestore Admin API did not answer for %s: %v", db, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var ans struct {
		Code    codes.Code `json:"code"`
		Message string     `json:"message"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&ans) != nil {
		return status.Errorf(codes.Unavailable, "the Firestore Admin API answered %s for %s", resp.Status, db)
	}
	if ans.Code == codes.OK {
		return nil
	}
	return status.Error(ans.Code, ans.Message)
}

func (g *datastoreGate) refusal(r *http.Request) error {
	if r.URL.Path == frontstate.DatastoreClearPath {
		return nil
	}
	db := callDatastoreDatabase(r)
	if db == "" || strings.HasPrefix(db, "projects/"+MarkerProject+"/") {
		return nil
	}
	return g.access(r.Context(), db)
}

// Claims reports whether r is a data call the Datastore API does not
// serve.
func (g *datastoreGate) Claims(r *http.Request) bool { return g.refusal(r) != nil }

// ServeHTTP answers a call Claims claimed with its refusal.
func (g *datastoreGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := g.refusal(r)
	if err == nil {
		err = status.Error(codes.Unavailable, "the database's access changed while the call was answered; retry it")
	}
	st, _ := status.FromError(err)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		netfwd.WriteGRPCStatus(w, int(st.Code()), st.Message())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	code := apierror.From(err).HTTPStatus()
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": st.Message(), "status": rpccode.Code(st.Code()).String()}})
}

// routed is ctx with the routing header of the database's calls.
func routed(ctx context.Context, project, database string) context.Context {
	params := "project_id=" + url.QueryEscape(project)
	if database != "" {
		params += "&database_id=" + url.QueryEscape(database)
	}
	return metadata.AppendToOutgoingContext(ctx, "x-goog-request-params", params)
}

// forgetDatabase drops a database's kept answer.
func (g *datastoreGate) forgetDatabase(db string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.answers, db)
}

// clearDatabase deletes every entity of the database, in every namespace,
// and its marker.
func (d *datastoreDatabases) clearDatabase(ctx context.Context, project, database string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	dbID := database
	if dbID == "(default)" {
		dbID = ""
	}
	ctx = routed(ctx, project, dbID)
	part := func(ns string) *pb.PartitionId {
		return &pb.PartitionId{ProjectId: project, DatabaseId: dbID, NamespaceId: ns}
	}
	keysOf := func(ns, kind string) ([]*pb.Key, error) {
		var keys []*pb.Key
		var cursor []byte
		for {
			q := &pb.Query{Projection: []*pb.Projection{{Property: &pb.PropertyReference{Name: "__key__"}}}, StartCursor: cursor}
			if kind != "" {
				q.Kind = []*pb.KindExpression{{Name: kind}}
			}
			resp, err := c.RunQuery(ctx, &pb.RunQueryRequest{ProjectId: project, DatabaseId: dbID, PartitionId: part(ns),
				QueryType: &pb.RunQueryRequest_Query{Query: q}})
			if err != nil {
				return nil, err
			}
			batch := resp.GetBatch()
			for _, e := range batch.GetEntityResults() {
				keys = append(keys, e.GetEntity().GetKey())
			}
			if batch.GetMoreResults() != pb.QueryResultBatch_NOT_FINISHED || len(batch.GetEntityResults()) == 0 {
				return keys, nil
			}
			cursor = batch.GetEndCursor()
		}
	}
	namespaces := []string{""}
	nsKeys, err := keysOf("", "__namespace__")
	if err != nil {
		return fmt.Errorf("list the namespaces: %w", err)
	}
	for _, k := range nsKeys {
		if p := k.GetPath(); len(p) > 0 && p[len(p)-1].GetName() != "" {
			namespaces = append(namespaces, p[len(p)-1].GetName())
		}
	}
	for _, ns := range namespaces {
		keys, err := keysOf(ns, "")
		if err != nil {
			return fmt.Errorf("list the entities of namespace %q: %w", ns, err)
		}
		var muts []*pb.Mutation
		for _, k := range keys {
			if p := k.GetPath(); len(p) > 0 && strings.HasPrefix(p[len(p)-1].GetKind(), "__") {
				continue
			}
			muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Delete{Delete: k}})
		}
		// A commit takes at most 500 mutations.
		for len(muts) > 0 {
			n := min(len(muts), 500)
			if _, err := c.Commit(ctx, &pb.CommitRequest{ProjectId: project, DatabaseId: dbID,
				Mode: pb.CommitRequest_NON_TRANSACTIONAL, Mutations: muts[:n]}); err != nil {
				return fmt.Errorf("delete the entities of namespace %q: %w", ns, err)
			}
			muts = muts[n:]
		}
	}
	if dbID == "" {
		return nil
	}
	name := "projects/" + project + "/databases/" + database
	d.mu.Lock()
	delete(d.known, name)
	d.mu.Unlock()
	_, err = c.Commit(withRouting(ctx), &pb.CommitRequest{ProjectId: MarkerProject, Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Delete{Delete: markerKey(project, database)}}}})
	return err
}

// serveClear answers DatastoreClearPath.
func (d *datastoreDatabases) serveClear(w http.ResponseWriter, r *http.Request) {
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
	if err := d.clearDatabase(r.Context(), project, database); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if d.gate != nil {
		d.gate.forgetDatabase("projects/" + project + "/databases/" + database)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}\n"))
}
