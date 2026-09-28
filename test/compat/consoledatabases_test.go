//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleError is the error the console returned for a refused request.
func consoleError(t *testing.T, body string) string {
	t.Helper()
	var out struct{ Error string }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the console's error is not JSON: %v: %s", err, body)
	}
	return out.Error
}

// TestConsoleBigtableCreateAndDeleteTable.
//
// The Bigtable screen's create and delete (#699). A table the console created
// with two column families is listed by the official bigtable admin client,
// in the console's one fixed instance, with exactly those families; a console
// delete removes it, so the admin client no longer lists it and TableInfo
// answers NOT_FOUND. The create and delete go through the console, so only
// the admin client's reads are claimed: Tables is ListTables and TableInfo
// is GetTable.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/ListTables, google.bigtable.admin.v2.BigtableTableAdmin/GetTable
func TestConsoleBigtableCreateAndDeleteTable(t *testing.T) {
	h := New(t)
	btAddr := h.Endpoint(EnvBigtable)
	addr := consoleAddr(t, h)
	t.Setenv("BIGTABLE_EMULATOR_HOST", btAddr)
	ctx := h.Context()
	project := h.Project()

	// The console's Bigtable screen serves one fixed instance, "cloudburrow".
	admin, err := bigtable.NewAdminClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	defer admin.Close()

	const table = "console-widgets"
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), table) })

	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/bigtable?project="+project,
		fmt.Sprintf(`{"table":%q,"families":"info, stats"}`, table))
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}

	tables, err := admin.Tables(ctx)
	if err != nil {
		t.Fatalf("admin Tables: %v", err)
	}
	if !slices.Contains(tables, table) {
		t.Fatalf("the admin client does not list the console-created table %s: %v", table, tables)
	}
	info, err := admin.TableInfo(ctx, table)
	if err != nil {
		t.Fatalf("admin TableInfo: %v", err)
	}
	var families []string
	for _, f := range info.FamilyInfos {
		families = append(families, f.Name)
	}
	slices.Sort(families)
	if !slices.Equal(families, []string{"info", "stats"}) {
		t.Errorf("the console-created table's families = %v, want [info stats]", families)
	}

	code, out = consoleDo(t, addr, http.MethodDelete,
		"/api/resources/bigtable?project="+project+"&name="+url.QueryEscape(table), "")
	if code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, out)
	}
	tables, err = admin.Tables(ctx)
	if err != nil {
		t.Fatalf("admin Tables after the delete: %v", err)
	}
	if slices.Contains(tables, table) {
		t.Errorf("the admin client still lists %s after a console delete: %v", table, tables)
	}
	if _, err := admin.TableInfo(ctx, table); status.Code(err) != codes.NotFound {
		t.Errorf("admin TableInfo after the console delete = %v, want NOT_FOUND", err)
	}
}

// TestConsoleSpannerCreateAndDropDatabase.
//
// The Spanner screen's create and drops (#699). The console's create makes the
// instance it names when absent, and the database with its first table: the
// instance admin client returns the instance, and the database admin client
// returns the database and its DDL. "Drop database", on the database's page,
// leaves GetDatabase NOT_FOUND; "Delete instance", on the instance's page,
// leaves GetInstance NOT_FOUND. The creates and drops go through the
// console, so only the admin clients' reads are claimed; the cleanup's
// DeleteInstance ignores its result.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/GetInstance, google.spanner.admin.database.v1.DatabaseAdmin/GetDatabase, google.spanner.admin.database.v1.DatabaseAdmin/GetDatabaseDdl
func TestConsoleSpannerCreateAndDropDatabase(t *testing.T) {
	h := New(t)
	spAddr := h.Endpoint(EnvSpanner)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	opts := spannerOpts(spAddr)

	instAdmin, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	defer instAdmin.Close()
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()

	const instanceID, dbID = "console-inst", "console-db"
	instName := fmt.Sprintf("projects/%s/instances/%s", project, instanceID)
	dbName := instName + "/databases/" + dbID
	// Deleting the instance takes its databases with it.
	t.Cleanup(func() {
		_ = instAdmin.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: instName})
	})

	body, _ := json.Marshal(map[string]string{
		"instance": instanceID, "database": dbID,
		"ddl": "CREATE TABLE Widgets (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)",
	})
	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/spanner?project="+project, string(body))
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}

	if _, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instName}); err != nil {
		t.Fatalf("the instance admin client cannot read the console-created instance: %v", err)
	}
	db, err := dbAdmin.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: dbName})
	if err != nil {
		t.Fatalf("the database admin client cannot read the console-created database: %v", err)
	}
	if db.GetName() != dbName {
		t.Errorf("GetDatabase name = %q, want %q", db.GetName(), dbName)
	}
	ddl, err := dbAdmin.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: dbName})
	if err != nil {
		t.Fatalf("GetDatabaseDdl: %v", err)
	}
	if !strings.Contains(strings.Join(ddl.GetStatements(), "\n"), "Widgets") {
		t.Errorf("the console-created database's DDL has no Widgets table: %v", ddl.GetStatements())
	}

	act := func(path []string, action string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": action})
		if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/spanner?project="+project, string(body)); code != http.StatusOK {
			t.Fatalf("console %s on %v = %d: %s", action, path, code, out)
		}
	}

	act([]string{instanceID, dbID}, "dropdatabase")
	if _, err := dbAdmin.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: dbName}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetDatabase after the console's Drop database = %v, want NOT_FOUND", err)
	}

	act([]string{instanceID}, "dropinstance")
	if _, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after the console's Delete instance = %v, want NOT_FOUND", err)
	}
}

// TestConsoleSpannerDML.
//
// The Spanner Studio's read-write mode (#798). An INSERT, an UPDATE and a
// DELETE sent from the console's editor with Mode "read-write" each report
// the rows they changed, and the official client's ReadRow sees each one
// committed. The same DELETE in the default read-only mode is refused before
// it is sent, with the reason, and the row is still there; a duplicate key
// comes back with Spanner's ALREADY_EXISTS; DDL and two statements at once are
// refused with the console's message and change nothing, which the database
// admin client's DDL and ReadRow confirm. A SELECT still runs read-only. The
// writes go through the console, so only the official clients' reads are
// claimed. No browser step: this shard has none, and the mode switch and
// confirmation are the console's own client code over this route.
//
// covers: google.spanner.v1.Spanner/StreamingRead, google.spanner.admin.database.v1.DatabaseAdmin/GetDatabaseDdl
func TestConsoleSpannerDML(t *testing.T) {
	h := New(t)
	spAddr := h.Endpoint(EnvSpanner)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	opts := spannerOpts(spAddr)

	const instanceID, dbID = "console-dml", "dml"
	instName := fmt.Sprintf("projects/%s/instances/%s", project, instanceID)
	dbName := spannerDatabase(t, ctx, spAddr, project, instanceID, dbID,
		"CREATE TABLE Widgets (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)")
	t.Cleanup(func() {
		instAdmin, err := instance.NewInstanceAdminClient(context.Background(), opts...)
		if err != nil {
			return
		}
		defer instAdmin.Close()
		_ = instAdmin.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: instName})
	})

	client, err := spanner.NewClient(ctx, dbName, opts...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	defer client.Close()
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()

	path := []string{instanceID, dbID}
	run := func(mode, statement string) (int, string) {
		t.Helper()
		req := map[string]any{"Path": path, "Statement": statement}
		if mode != "" {
			req["Mode"] = mode
		}
		body, _ := json.Marshal(req)
		return consoleDo(t, addr, http.MethodPost, "/api/query/spanner?project="+project, string(body))
	}
	write := func(statement string, want int64) {
		t.Helper()
		code, out := run("read-write", statement)
		if code != http.StatusOK {
			t.Fatalf("console read-write %q = %d: %s", statement, code, out)
		}
		var got struct{ RowCount int64 }
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("the console's answer is not JSON: %v: %s", err, out)
		}
		if got.RowCount != want {
			t.Errorf("console read-write %q reported %d rows, want %d", statement, got.RowCount, want)
		}
	}
	refused := func(mode, statement, want string) {
		t.Helper()
		code, out := run(mode, statement)
		if code != http.StatusBadRequest {
			t.Fatalf("console %s %q = %d, want 400: %s", mode, statement, code, out)
		}
		if msg := consoleError(t, out); !strings.Contains(msg, want) {
			t.Errorf("console %s %q refused with %q, want it to say %q", mode, statement, msg, want)
		}
	}
	name := func(id int64) (string, bool) {
		t.Helper()
		row, err := client.Single().ReadRow(ctx, "Widgets", spanner.Key{id}, []string{"Name"})
		if spanner.ErrCode(err) == codes.NotFound {
			return "", false
		}
		if err != nil {
			t.Fatalf("ReadRow %d: %v", id, err)
		}
		var n spanner.NullString
		if err := row.Columns(&n); err != nil {
			t.Fatalf("ReadRow %d columns: %v", id, err)
		}
		return n.StringVal, true
	}

	write("INSERT INTO Widgets (Id, Name) VALUES (1, 'one'), (2, 'two')", 2)
	for id, want := range map[int64]string{1: "one", 2: "two"} {
		if got, ok := name(id); !ok || got != want {
			t.Errorf("after the console's INSERT, ReadRow %d = %q (found %v), want %q", id, got, ok, want)
		}
	}
	write("UPDATE Widgets SET Name = 'uno' WHERE Id = 1", 1)
	if got, _ := name(1); got != "uno" {
		t.Errorf("after the console's UPDATE, ReadRow 1 = %q, want uno", got)
	}
	write("DELETE FROM Widgets WHERE Id = 2;", 1)
	if _, ok := name(2); ok {
		t.Error("after the console's DELETE, ReadRow 2 still finds the row")
	}

	// Read-only is the default, and a DML statement there never reaches Spanner.
	refused("", "DELETE FROM Widgets WHERE Id = 1", "the editor is read-only")
	if _, ok := name(1); !ok {
		t.Error("a DELETE refused in read-only mode removed the row")
	}
	// A constraint violation is Spanner's own answer.
	refused("read-write", "INSERT INTO Widgets (Id, Name) VALUES (1, 'again')", "AlreadyExists")
	if got, _ := name(1); got != "uno" {
		t.Errorf("a refused INSERT changed row 1 to %q", got)
	}
	// DDL is not run here, in either mode, and two statements are not one.
	refused("read-write", "CREATE TABLE Gadgets (Id INT64 NOT NULL) PRIMARY KEY (Id)", "is DDL")
	refused("", "DROP TABLE Widgets", "is DDL")
	refused("read-write", "INSERT INTO Widgets (Id) VALUES (10); INSERT INTO Widgets (Id) VALUES (11)",
		"one DML statement at a time")
	for _, id := range []int64{10, 11} {
		if _, ok := name(id); ok {
			t.Errorf("a refused multi-statement write inserted row %d", id)
		}
	}
	ddl, err := dbAdmin.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: dbName})
	if err != nil {
		t.Fatalf("GetDatabaseDdl: %v", err)
	}
	if got := strings.Join(ddl.GetStatements(), "\n"); strings.Contains(got, "Gadgets") || !strings.Contains(got, "Widgets") {
		t.Errorf("the schema after the refused DDL = %v, want Widgets alone", ddl.GetStatements())
	}
	// A SELECT is refused in read-write and still runs read-only.
	refused("read-write", "SELECT Id FROM Widgets", "switch the editor to Read-only")
	code, out := run("", "SELECT Id, Name FROM Widgets ORDER BY Id")
	if code != http.StatusOK || !strings.Contains(out, "uno") {
		t.Errorf("console read-only SELECT = %d: %s, want row 1 named uno", code, out)
	}
}

// TestConsoleCloudSQLCreateAndDropDatabase.
//
// The Cloud SQL screen's create and drop (#699). A database the console
// created is in pg_database and accepts a pgx connection; a console drop
// removes it. Dropping the database the server was initialised with is
// refused with the provider's message, and that database is still there.
func TestConsoleCloudSQLCreateAndDropDatabase(t *testing.T) {
	h := New(t)
	pgAddr := h.Endpoint(EnvCloudSQL)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()

	// A PostgreSQL database is not scoped by project, so the name is unique
	// to this run: cb-test-<n> becomes console_<n>.
	name := "console_" + strings.NewReplacer("cb-test-", "", "-", "_").Replace(project)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(),
			fmt.Sprintf("postgres://cloudburrow@%s/cloudburrow?sslmode=disable", pgAddr))
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
	})

	admin := pgConnect(t, h, "cloudburrow")
	exists := func(db string) bool {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", db).Scan(&n); err != nil {
			t.Fatalf("reading pg_database: %v", err)
		}
		return n == 1
	}

	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/cloudsql?project="+project,
		fmt.Sprintf(`{"database":%q}`, name))
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	if !exists(name) {
		t.Fatalf("pg_database has no %s after the console created it", name)
	}
	// A real database: pgx connects to it and it answers as itself. Closed
	// before the drop, which PostgreSQL refuses while a session is open.
	c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://cloudburrow@%s/%s?sslmode=disable", pgAddr, name))
	if err != nil {
		t.Fatalf("pgx cannot connect to the console-created database: %v", err)
	}
	var current string
	err = c.QueryRow(ctx, "SELECT current_database()").Scan(&current)
	_ = c.Close(ctx)
	if err != nil || current != name {
		t.Fatalf("current_database() = %q, %v; want %q", current, err, name)
	}

	code, out = consoleDo(t, addr, http.MethodDelete,
		"/api/resources/cloudsql?project="+project+"&name="+url.QueryEscape(name), "")
	if code != http.StatusOK {
		t.Fatalf("console drop = %d: %s", code, out)
	}
	if exists(name) {
		t.Errorf("pg_database still has %s after a console drop", name)
	}

	// The initial database is refused, with the provider's reason.
	code, out = consoleDo(t, addr, http.MethodDelete,
		"/api/resources/cloudsql?project="+project+"&name=cloudburrow", "")
	if code != http.StatusBadRequest {
		t.Fatalf("console drop of the initial database = %d, want 400: %s", code, out)
	}
	const want = `"cloudburrow" is the database the server was initialised with and cannot be dropped here`
	if msg := consoleError(t, out); msg != want {
		t.Errorf("refusal = %q, want %q", msg, want)
	}
	if !exists("cloudburrow") {
		t.Error("the initial database is gone after a refused drop")
	}
}
