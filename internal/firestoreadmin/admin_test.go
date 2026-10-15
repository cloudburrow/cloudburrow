package firestoreadmin_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

// fakeEmulator records the calls the front passes it.
type fakeEmulator struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	_, _ = io.WriteString(w, "{}")
}

func (f *fakeEmulator) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// front serves the call front, with the Admin API, for a fake emulator.
func front(t *testing.T) (string, *fakeEmulator) {
	t.Helper()
	emu := &fakeEmulator{}
	es := httptest.NewServer(emu)
	t.Cleanup(es.Close)
	up := strings.TrimPrefix(es.URL, "http://")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := callfront.Server(callfront.HandlerWith(up, &calllog.Ring{}, t.Logf, firestoreadmin.New(up)), t.Logf)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String(), emu
}

func TestAdminOverGRPC(t *testing.T) {
	addr, emu := front(t)
	ctx := context.Background()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := admin.NewFirestoreAdminClient(ctx, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	list, err := c.ListDatabases(ctx, &adminpb.ListDatabasesRequest{Parent: "projects/p"})
	if err != nil || len(list.Databases) != 1 || list.Databases[0].Name != "projects/p/databases/(default)" ||
		list.Databases[0].Type != adminpb.Database_FIRESTORE_NATIVE {
		t.Fatalf("ListDatabases = %v, %v", list, err)
	}
	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "named",
		Database: &adminpb.Database{LocationId: "us-east1", Type: adminpb.Database_FIRESTORE_NATIVE}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := op.Wait(ctx)
	if err != nil || db.Name != "projects/p/databases/named" || db.ConcurrencyMode != adminpb.Database_PESSIMISTIC || db.Uid == "" {
		t.Fatalf("CreateDatabase = %v, %v", db, err)
	}
	if _, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "named",
		Database: &adminpb.Database{LocationId: "us-east1", Type: adminpb.Database_FIRESTORE_NATIVE}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second create: %v, want AlreadyExists", err)
	}
	if _, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "dsdb",
		Database: &adminpb.Database{LocationId: "us-east1", Type: adminpb.Database_DATASTORE_MODE}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a Datastore mode database: %v, want Unimplemented", err)
	}
	uop, err := c.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{
		Database:   &adminpb.Database{Name: db.Name, DeleteProtectionState: adminpb.Database_DELETE_PROTECTION_ENABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"delete_protection_state"}}})
	if err != nil {
		t.Fatal(err)
	}
	if db, err = uop.Wait(ctx); err != nil || db.DeleteProtectionState != adminpb.Database_DELETE_PROTECTION_ENABLED {
		t.Fatalf("UpdateDatabase = %v, %v", db, err)
	}
	if _, err := c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: db.Name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a protected database: %v, want FailedPrecondition", err)
	}

	iop, err := c.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: db.Name + "/collectionGroups/cities",
		Index: &adminpb.Index{QueryScope: adminpb.Index_COLLECTION, Fields: []*adminpb.Index_IndexField{
			{FieldPath: "country", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}},
			{FieldPath: "pop", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_DESCENDING}},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := iop.Wait(ctx)
	if err != nil || idx.State != adminpb.Index_READY || len(idx.Fields) != 3 || idx.Fields[2].FieldPath != "__name__" ||
		idx.Fields[2].GetOrder() != adminpb.Index_IndexField_DESCENDING {
		t.Fatalf("CreateIndex = %v, %v", idx, err)
	}
	if md, err := iop.Metadata(); err != nil || md.Index != idx.Name || md.State != adminpb.OperationState_SUCCESSFUL {
		t.Errorf("index metadata = %v, %v", md, err)
	}
	it := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db.Name + "/collectionGroups/-"})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("ListIndexes = %d, want 1", n)
	}
	if err := c.DeleteIndex(ctx, &adminpb.DeleteIndexRequest{Name: idx.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIndex(ctx, &adminpb.GetIndexRequest{Name: idx.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted index: %v, want NotFound", err)
	}

	fname := db.Name + "/collectionGroups/cities/fields/expireAt"
	f, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: fname})
	if err != nil || !f.GetIndexConfig().GetUsesAncestorConfig() || len(f.GetIndexConfig().GetIndexes()) != 3 {
		t.Fatalf("GetField = %v, %v", f, err)
	}
	fop, err := c.UpdateField(ctx, &adminpb.UpdateFieldRequest{Field: &adminpb.Field{Name: fname, TtlConfig: &adminpb.Field_TtlConfig{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl_config"}}})
	if err != nil {
		t.Fatal(err)
	}
	if f, err = fop.Wait(ctx); err != nil || f.GetTtlConfig().GetState() != adminpb.Field_TtlConfig_ACTIVE {
		t.Fatalf("UpdateField = %v, %v", f, err)
	}
	fit := c.ListFields(ctx, &adminpb.ListFieldsRequest{Parent: db.Name + "/collectionGroups/-", Filter: "ttlConfig:*"})
	if got, err := fit.Next(); err != nil || got.Name != fname {
		t.Errorf("ListFields = %v, %v", got, err)
	}

	if _, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: db.Name}); status.Code(err) != codes.Unimplemented {
		t.Errorf("ExportDocuments: %v, want Unimplemented", err)
	}

	uop, _ = c.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{
		Database:   &adminpb.Database{Name: db.Name, DeleteProtectionState: adminpb.Database_DELETE_PROTECTION_DISABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"delete_protection_state"}}})
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	dop, err := c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: db.Name})
	if err != nil {
		t.Fatal(err)
	}
	if gone, err := dop.Wait(ctx); err != nil || gone.DeleteTime == nil {
		t.Fatalf("DeleteDatabase = %v, %v", gone, err)
	}
	if _, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted database: %v, want NotFound", err)
	}
	cleared := false
	for _, s := range emu.seen() {
		cleared = cleared || s == "DELETE /emulator/v1/projects/p/databases/named/documents"
	}
	if !cleared {
		t.Errorf("deleting the database did not clear its documents in the emulator: %v", emu.seen())
	}
}

func TestAdminOverRESTAndDocumentsPassThrough(t *testing.T) {
	addr, emu := front(t)
	base := "http://" + addr
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	code, op := do("POST", "/v1/projects/p/databases?databaseId=tfdb", `{"locationId":"nam5","type":"FIRESTORE_NATIVE","concurrencyMode":"OPTIMISTIC"}`)
	if code != 200 || op["done"] != true {
		t.Fatalf("create = %d %v", code, op)
	}
	code, got := do("GET", "/v1/"+op["name"].(string), "")
	if code != 200 || got["response"].(map[string]any)["concurrencyMode"] != "OPTIMISTIC" {
		t.Fatalf("the operation = %d %v", code, got)
	}
	code, got = do("PATCH", "/v1/projects/p/databases/tfdb?updateMask=pointInTimeRecoveryEnablement", `{"pointInTimeRecoveryEnablement":"POINT_IN_TIME_RECOVERY_ENABLED"}`)
	if code != 200 {
		t.Fatalf("patch = %d %v", code, got)
	}
	code, got = do("GET", "/v1/projects/p/databases/tfdb", "")
	if code != 200 || got["versionRetentionPeriod"] != "604800s" {
		t.Errorf("get = %d %v", code, got)
	}
	code, got = do("POST", "/v1/projects/p/databases/tfdb/collectionGroups/c/indexes", `{"queryScope":"COLLECTION","fields":[{"fieldPath":"a","order":"ASCENDING"}]}`)
	if code != 400 {
		t.Errorf("a one-field composite index = %d %v, want 400", code, got)
	}
	code, got = do("POST", "/v1/projects/p/databases/tfdb:exportDocuments", `{}`)
	if code != 501 || got["error"].(map[string]any)["status"] != "UNIMPLEMENTED" {
		t.Errorf("export = %d %v, want 501", code, got)
	}
	code, _ = do("GET", "/v1/projects/p/databases/tfdb/documents/c/d", "")
	if code != 200 {
		t.Errorf("a document read = %d", code)
	}
	if s := emu.seen(); len(s) != 1 || s[0] != "GET /v1/projects/p/databases/tfdb/documents/c/d" {
		t.Errorf("the emulator saw %v, want the document read alone", s)
	}
	code, got = do("DELETE", "/v1/projects/p/databases/tfdb", "")
	if code != 200 || got["done"] != true {
		t.Errorf("delete = %d %v", code, got)
	}
}
