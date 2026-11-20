package firestoreadmin

// Which API serves a database, and whether its key lets it (#1880, #1881).
//
// The Admin API is the record of every database's type: a Native mode
// database's documents are the Firestore emulator's, in this pod, and a
// Datastore mode database's entities are the Datastore emulator's, in its
// own pod, behind its own call front. Each front asks Access before it
// passes a data call on: the Firestore front in process (AccessGate), the
// Datastore front over the cluster (AccessPath, internal/callfront). As on
// Google, the Firestore API refuses a Datastore mode database and the
// Datastore API a Native mode one, with FAILED_PRECONDITION; and either
// refuses a database whose customer-managed key is not accessible
// (cmek.go).
//
// Two allowances keep what was served before served. A database the Admin
// API has no record of is served, as the emulators serve any database ID
// (#1550, seen.go). And the Datastore API serves the `(default)` database
// of a project whose `(default)` is Native mode: the Datastore emulator has
// always served every project's `(default)`, apart from the Firestore
// emulator's.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// AccessPath is frontstate.FirestoreAccessPath.
const AccessPath = frontstate.FirestoreAccessPath

// The APIs a database is served through.
const (
	APIFirestore = "firestore"
	APIDatastore = "datastore"
)

// Access is the refusal a data call through api gets for the database
// name ("projects/p/databases/d"), or nil.
func (a *Admin) Access(ctx context.Context, name, api string) error {
	if dbNameRe.FindStringSubmatch(name) == nil {
		return nil
	}
	a.mu.Lock()
	db, ok := a.databases[name]
	known := ok && db.DeleteTime == nil
	var typ adminpb.Database_DatabaseType
	if known {
		typ = db.GetType()
	}
	a.mu.Unlock()
	if !known {
		return nil
	}
	switch {
	case api == APIFirestore && typ == adminpb.Database_DATASTORE_MODE:
		return status.Errorf(codes.FailedPrecondition, "The Cloud Firestore API is not available for Firestore in Datastore Mode database %s.", name)
	case api == APIDatastore && typ == adminpb.Database_FIRESTORE_NATIVE && !strings.HasSuffix(name, "/databases/"+DefaultDatabase):
		return status.Errorf(codes.FailedPrecondition, "The Cloud Datastore API is not available for Firestore in Native mode database %s.", name)
	}
	return a.keyAccess(ctx, name)
}

// AccessAnswer is AccessPath's answer: the refusal's gRPC code and
// message, or code 0.
type AccessAnswer struct {
	Code    codes.Code `json:"code"`
	Message string     `json:"message,omitempty"`
	// The database's version window, for the Datastore front's reads at
	// an earlier time (#1967): its earliest version time and whether
	// point-in-time recovery is enabled, when the API knows the
	// database.
	EarliestVersionTime *time.Time `json:"earliestVersionTime,omitempty"`
	PITR                bool       `json:"pitr,omitempty"`
}

// serveAccess answers AccessPath: GET ?name=projects/p/databases/d&api=datastore.
func (a *Admin) serveAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	api := q.Get("api")
	if q.Get("name") == "" || (api != APIFirestore && api != APIDatastore) {
		http.Error(w, "name= and api=firestore|datastore are required", http.StatusBadRequest)
		return
	}
	var out AccessAnswer
	if err := a.Access(r.Context(), q.Get("name"), api); err != nil {
		st, _ := status.FromError(err)
		out = AccessAnswer{Code: st.Code(), Message: st.Message()}
	}
	if earliest, pitr, ok := a.Window(q.Get("name")); ok && api == APIDatastore {
		out.EarliestVersionTime, out.PITR = &earliest, pitr
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// AccessGate refuses the Firestore API's data calls on a database Access
// refuses. It is an intercept of the call front, ahead of the others.
type AccessGate struct{ a *Admin }

// AccessGate is the gate over a's databases.
func (a *Admin) AccessGate() *AccessGate { return &AccessGate{a: a} }

const firestoreServicePrefix = "/google.firestore.v1.Firestore/"

// callDatabase is the database a Firestore data call names, or "".
func callDatabase(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		if !strings.HasPrefix(r.URL.Path, firestoreServicePrefix) {
			return ""
		}
		return databaseOf(netfwd.ResourceOf(r.Header))
	}
	if strings.HasPrefix(r.URL.Path, firestoreServicePrefix) {
		// WebChannel: the database is a parameter.
		return databaseOf(r.URL.Query().Get("database"))
	}
	// Every REST documents call, executePipeline's too.
	if m := restDocumentsPath.FindStringSubmatch(r.URL.Path); m != nil {
		return m[1]
	}
	return ""
}

func (g *AccessGate) refusal(r *http.Request) error {
	db := callDatabase(r)
	if db == "" {
		return nil
	}
	return g.a.Access(r.Context(), db, APIFirestore)
}

// Claims reports whether r is a data call Access refuses.
func (g *AccessGate) Claims(r *http.Request) bool { return g.refusal(r) != nil }

// ServeHTTP answers a call Claims claimed with its refusal.
func (g *AccessGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := g.refusal(r)
	if err == nil {
		err = status.Error(codes.Unavailable, "the database's access changed while the call was answered; retry it")
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		st, _ := status.FromError(err)
		netfwd.WriteGRPCStatus(w, int(st.Code()), st.Message())
		return
	}
	writeError(w, err)
}
