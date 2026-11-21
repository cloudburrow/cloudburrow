package iamgate

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
)

func checks(t *testing.T, svc *Service, full string, m proto.Message) []Check {
	t.Helper()
	r, ok := svc.Methods[full]
	if !ok {
		t.Fatalf("%s has no rule", full)
	}
	return r(m.ProtoReflect())
}

func TestFirestoreWrites(t *testing.T) {
	db := "projects/p/databases/(default)"
	doc := db + "/documents/c/x"
	exists := func(b bool) *firestorepb.Precondition {
		return &firestorepb.Precondition{ConditionType: &firestorepb.Precondition_Exists{Exists: b}}
	}
	upd := func(pre *firestorepb.Precondition) *firestorepb.Write {
		return &firestorepb.Write{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: doc}}, CurrentDocument: pre}
	}
	for _, c := range []struct {
		writes []*firestorepb.Write
		want   string
	}{
		{[]*firestorepb.Write{upd(exists(false))}, "datastore.entities.create"},
		{[]*firestorepb.Write{upd(exists(true))}, "datastore.entities.update"},
		{[]*firestorepb.Write{upd(nil)}, "datastore.entities.create,datastore.entities.update"},
		{[]*firestorepb.Write{{Operation: &firestorepb.Write_Delete{Delete: doc}}}, "datastore.entities.delete"},
		{nil, ""},
	} {
		got := checks(t, Firestore, fsData+"Commit", &firestorepb.CommitRequest{Database: db, Writes: c.writes})
		var perms []string
		for _, g := range got {
			if g.Resource != db {
				t.Errorf("resource %q, want %q", g.Resource, db)
			}
			perms = append(perms, g.Permission)
		}
		if strings.Join(perms, ",") != c.want {
			t.Errorf("Commit %v = %v, want %s", c.writes, perms, c.want)
		}
	}
	got := checks(t, Firestore, fsData+"GetDocument", &firestorepb.GetDocumentRequest{Name: doc})
	if len(got) != 1 || got[0] != (Check{"datastore.entities.get", db}) {
		t.Errorf("GetDocument = %v", got)
	}
	got = checks(t, Firestore, fsData+"RunQuery", &firestorepb.RunQueryRequest{Parent: db + "/documents"})
	if len(got) != 2 || got[1] != (Check{"datastore.entities.list", db}) {
		t.Errorf("RunQuery = %v", got)
	}
}

func TestSpannerRules(t *testing.T) {
	db := "projects/p/instances/i/databases/d"
	s := db + "/sessions/s"
	got := checks(t, Spanner, spData+"ExecuteSql", &spannerpb.ExecuteSqlRequest{Session: s})
	if len(got) != 1 || got[0] != (Check{"spanner.databases.select", db}) {
		t.Errorf("ExecuteSql = %v", got)
	}
	ro := &spannerpb.BeginTransactionRequest{Session: s, Options: &spannerpb.TransactionOptions{
		Mode: &spannerpb.TransactionOptions_ReadOnly_{ReadOnly: &spannerpb.TransactionOptions_ReadOnly{}}}}
	if got := checks(t, Spanner, spData+"BeginTransaction", ro); got[0].Permission != "spanner.databases.beginReadOnlyTransaction" {
		t.Errorf("read-only BeginTransaction = %v", got)
	}
	rw := &spannerpb.BeginTransactionRequest{Session: s, Options: &spannerpb.TransactionOptions{
		Mode: &spannerpb.TransactionOptions_ReadWrite_{ReadWrite: &spannerpb.TransactionOptions_ReadWrite{}}}}
	if got := checks(t, Spanner, spData+"BeginTransaction", rw); got[0].Permission != "spanner.databases.beginOrRollbackReadWriteTransaction" {
		t.Errorf("read-write BeginTransaction = %v", got)
	}
	if Spanner.Type(db) != "spanner.googleapis.com/Database" || Spanner.Type("projects/p/instances/i") != "spanner.googleapis.com/Instance" {
		t.Errorf("types: %q %q", Spanner.Type(db), Spanner.Type("projects/p/instances/i"))
	}
}

func TestBigtableRules(t *testing.T) {
	tbl := "projects/p/instances/i/tables/t"
	got := checks(t, Bigtable, btData+"CheckAndMutateRow", &btpb.CheckAndMutateRowRequest{TableName: tbl})
	if len(got) != 2 || got[0].Permission != "bigtable.tables.readRows" || got[1].Permission != "bigtable.tables.mutateRows" {
		t.Errorf("CheckAndMutateRow = %v", got)
	}
	view := tbl + "/authorizedViews/v"
	got = checks(t, Bigtable, btData+"ReadRows", &btpb.ReadRowsRequest{AuthorizedViewName: view})
	if len(got) != 1 || got[0] != (Check{"bigtable.authorizedViews.readRows", view}) {
		t.Errorf("ReadRows of a view = %v", got)
	}
}

// fakeHost is the host's evaluator granting held to every caller with
// bearer "granted", nothing to "denied", and recording the requests.
func fakeHost(t *testing.T, held ...string) (*iamcheck.Client, *[]iamcheck.Request) {
	t.Helper()
	var seen []iamcheck.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in iamcheck.Request
		_ = json.NewDecoder(r.Body).Decode(&in)
		seen = append(seen, in)
		d := iamcheck.Decision{Held: []string{}}
		switch in.Bearer {
		case "":
			d.Owner, d.Held = true, in.Permissions
		case "granted":
			d.Held = held
		case "denied":
		default:
			d.Unauthenticated = true
		}
		_ = json.NewEncoder(w).Encode(d)
	}))
	t.Cleanup(srv.Close)
	return &iamcheck.Client{Base: srv.URL}, &seen
}

func grpcBody(m proto.Message) []byte {
	b, _ := proto.Marshal(m)
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(b)))
	return append(hdr[:], b...)
}

func TestWrapGRPC(t *testing.T) {
	c, seen := fakeHost(t, "spanner.databases.select")
	g := &Gate{Client: c, Services: []*Service{Spanner}}
	var got []byte
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	body := grpcBody(&spannerpb.ExecuteSqlRequest{Session: "projects/p/instances/i/databases/d/sessions/s", Sql: "SELECT 1"})
	call := func(bearer string) *httptest.ResponseRecorder {
		got = nil
		r := httptest.NewRequest(http.MethodPost, spData+"ExecuteSql", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/grpc")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("granted"); !bytes.Equal(got, body) {
		t.Errorf("granted: the emulator saw %d bytes, want the request replayed (%d); status %v", len(got), len(body), w.Header())
	}
	if (*seen)[0].Type != "spanner.googleapis.com/Database" || (*seen)[0].Project != "p" {
		t.Errorf("check request = %+v", (*seen)[0])
	}
	w := call("denied")
	if got != nil || w.Header().Get("Grpc-Status") != "7" ||
		w.Header().Get("Grpc-Message") == "" {
		t.Errorf("denied: emulator saw %d bytes, header %v", len(got), w.Header())
	}
	if call("owner"); !bytes.Equal(got, body) {
		t.Errorf("the emulator token owner: not passed as the instance's own account")
	}
	w = call("forged")
	if got != nil || w.Header().Get("Grpc-Status") != "16" {
		t.Errorf("forged: header %v", w.Header())
	}
}

func TestWrapREST(t *testing.T) {
	c, _ := fakeHost(t, "datastore.entities.get")
	g := &Gate{Client: c, Services: []*Service{Firestore}}
	passed := false
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { passed = true }))
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/p/databases/(default)/documents/c/x", nil)
	r.Header.Set("Authorization", "Bearer granted")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !passed {
		t.Fatal("granted GetDocument did not pass")
	}
	passed = false
	r = httptest.NewRequest(http.MethodDelete, "/v1/projects/p/databases/(default)/documents/c/x", nil)
	r.Header.Set("Authorization", "Bearer granted")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if passed || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Missing or insufficient permissions.") ||
		!strings.Contains(w.Body.String(), "PERMISSION_DENIED") {
		t.Errorf("DeleteDocument without the permission: %d %s", w.Code, w.Body)
	}
	_ = context.Background()
}
