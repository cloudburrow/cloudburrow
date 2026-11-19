package main

import (
	"context"
	"net/url"
	"sort"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Datastore page's database (#1883). Google's Datastore pages browse
// any Datastore mode database of the project, chosen in a database picker;
// the screens read only (default) before. The database travels in the URL
// as ?database=, as the Firestore Data viewer's does (#1845), and every read
// and change the screens make addresses it: the Go client's, opened with
// NewClientWithDatabase, and the raw v1 calls', whose requests carry its
// database_id and the routing header the Datastore front reads.

// ScopeKey implements console.Scoper.
func (datastoreProvider) ScopeKey() string { return firestoreDatabaseKey }

var _ console.Scoper = datastoreProvider{}

// datastoreDatabaseIn is the database ctx's request names, or (default).
func datastoreDatabaseIn(ctx context.Context) string {
	if v := console.ScopeValue(ctx, firestoreDatabaseKey); v != "" {
		return v
	}
	return firestoreDefaultDatabase
}

// datastoreDatabaseID is the database ID a client is given: "" for
// (default).
func datastoreDatabaseID(ctx context.Context) string {
	if db := datastoreDatabaseIn(ctx); db != firestoreDefaultDatabase {
		return db
	}
	return ""
}

// datastoreDatabaseInterceptor addresses each raw v1 call to the database
// its context's request names: the request's database_id, and the routing
// header (project_id, database_id) Google's clients send, which the
// Datastore front reads.
func datastoreDatabaseInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	db := datastoreDatabaseID(ctx)
	if m, ok := req.(proto.Message); ok && db != "" {
		r := m.ProtoReflect()
		fields := r.Descriptor().Fields()
		if fd := fields.ByName("database_id"); fd != nil && fd.Kind() == protoreflect.StringKind {
			r.Set(fd, protoreflect.ValueOfString(db))
		}
		project := ""
		if fd := fields.ByName("project_id"); fd != nil && fd.Kind() == protoreflect.StringKind {
			project = r.Get(fd).String()
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "x-goog-request-params",
			"project_id="+url.QueryEscape(project)+"&database_id="+url.QueryEscape(db))
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// datastoreHome is where a page's entities are: its project and database,
// whose keys are shown as plain keys; a key elsewhere is a
// datastoreForeignKey.
type datastoreHome struct{ project, database string }

// homeOf is the home of ctx's request in project.
func homeOf(ctx context.Context, project string) datastoreHome {
	return datastoreHome{project: project, database: datastoreDatabaseID(ctx)}
}

// databaseScope is the page's database picker: (default), which the
// emulator serves without it being created, then the project's other
// Datastore mode databases, by ID, from the Firestore Admin API. A
// Firestore Native database is browsed on the Firestore page, as on
// Google's console.
func (p datastoreProvider) databaseScope(ctx context.Context, project string) *console.Scope {
	sc := &console.Scope{Key: firestoreDatabaseKey, Label: "Database", Selected: datastoreDatabaseIn(ctx),
		Default: firestoreDefaultDatabase, Options: []console.ScopeOption{{ID: firestoreDefaultDatabase}}}
	if p.firestore != "" {
		if c, err := admin.NewFirestoreAdminClient(ctx, localOpts(p.firestore)...); err == nil {
			if resp, err := c.ListDatabases(ctx, &adminpb.ListDatabasesRequest{Parent: "projects/" + project}); err == nil {
				var ids []string
				for _, db := range resp.GetDatabases() {
					if id := dbID(db.GetName()); id != firestoreDefaultDatabase && db.GetType() == adminpb.Database_DATASTORE_MODE {
						ids = append(ids, id)
					}
				}
				sort.Strings(ids)
				for _, id := range ids {
					sc.Options = append(sc.Options, console.ScopeOption{ID: id})
				}
			}
			_ = c.Close()
		}
	}
	known := false
	for _, o := range sc.Options {
		known = known || o.ID == sc.Selected
	}
	if !known {
		sc.Options = append(sc.Options, console.ScopeOption{ID: sc.Selected, Label: sc.Selected + " (not found)"})
	}
	return sc
}
