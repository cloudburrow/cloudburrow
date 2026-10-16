//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/api/iterator"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// sqlObjectNames are the names of bh's objects under prefix.
func sqlObjectNames(t *testing.T, h *Harness, bh *storage.BucketHandle, prefix string) []string {
	t.Helper()
	var out []string
	it := bh.Objects(h.Context(), &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a.Name)
	}
}

// TestCloudSQLParallelExportAndImport (#1421): through the official Go
// client, a parallel export (sqlExportOptions.parallel, threads) writes
// the directory Cloud SQL documents under the uri, a folder: pg_dump's
// directory format (toc.dat and a file per table) for PostgreSQL, MySQL
// Shell's dump (@.json, @.done.json, per-table chunks) for MySQL; a
// parallel import (sqlImportOptions.parallel) restores it, read back by the
// database's own client; a second parallel export to the same folder fails
// its operation, threads without parallel is 400, and a PostgreSQL parallel
// export of several databases is 400.
// covers: sql.instances.export, sql.instances.import
func TestCloudSQLParallelExportAndImport(t *testing.T) {
	h := New(t)
	engines := sqlTransferEngines(t, h)
	svc := sqlAdminService(t, h)
	p := h.Project()
	bh := sqlTransferBucket(t, h, "sqlpar")
	for _, e := range engines {
		t.Run(e.instance, func(t *testing.T) {
			e.exec(t, "DROP TABLE IF EXISTS xfer_rows", "CREATE TABLE xfer_rows (id INT PRIMARY KEY, name VARCHAR(40))",
				`INSERT INTO xfer_rows VALUES (1, 'one'), (2, 'two'), (3, 'three')`)
			defer e.exec(t, "DROP TABLE IF EXISTS xfer_rows")
			want := "1=one;2=two;3=three"
			dir := e.instance + "-par"
			uri := "gs://" + bh.BucketName() + "/" + dir
			x := &sqladmin.ExportContext{Uri: uri, FileType: "SQL", Databases: []string{"cloudburrow"},
				SqlExportOptions: &sqladmin.ExportContextSqlExportOptions{Parallel: true, Threads: 2}}
			if e.instance == "mysql" {
				x.SqlExportOptions.Tables = []string{"xfer_rows"}
			}
			op, err := svc.Instances.Export(p, e.instance, &sqladmin.InstancesExportRequest{ExportContext: x}).Do()
			if err != nil {
				t.Fatal(err)
			}
			if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
				t.Fatalf("parallel export: %s", op.Error.Errors[0].Message)
			}
			names := strings.Join(sqlObjectNames(t, h, bh, dir+"/"), " ")
			marker := dir + "/toc.dat"
			if e.instance == "mysql" {
				marker = dir + "/@.done.json"
			}
			if !strings.Contains(names, marker) {
				t.Fatalf("the parallel export wrote %s, without %s", names, marker)
			}
			// The folder is not empty now.
			op, err = svc.Instances.Export(p, e.instance, &sqladmin.InstancesExportRequest{ExportContext: x}).Do()
			if err != nil {
				t.Fatal(err)
			}
			if op = waitSQLOpDone(t, svc, p, op); op.Error == nil {
				t.Error("a second parallel export to the same folder succeeded")
			}
			_, err = svc.Instances.Export(p, e.instance, &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
				Uri: uri + "-t", Databases: []string{"cloudburrow"}, SqlExportOptions: &sqladmin.ExportContextSqlExportOptions{Threads: 2}}}).Do()
			wantHTTP(t, "threads without parallel", err, 400)
			if e.instance == "postgres" {
				_, err = svc.Instances.Export(p, e.instance, &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
					Uri: uri + "-2", Databases: []string{"cloudburrow", "postgres"}, SqlExportOptions: &sqladmin.ExportContextSqlExportOptions{Parallel: true}}}).Do()
				wantHTTP(t, "a parallel export of two PostgreSQL databases", err, 400)
			}

			e.exec(t, "DROP TABLE xfer_rows")
			imp := &sqladmin.ImportContext{Uri: uri, FileType: "SQL", Database: "cloudburrow",
				SqlImportOptions: &sqladmin.ImportContextSqlImportOptions{Parallel: true, Threads: 2}}
			op, err = svc.Instances.Import(p, e.instance, &sqladmin.InstancesImportRequest{ImportContext: imp}).Do()
			if err != nil {
				t.Fatal(err)
			}
			if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
				t.Fatalf("parallel import: %s", op.Error.Errors[0].Message)
			}
			if got := e.rows(t); got != want {
				t.Errorf("rows after the parallel import: %q, want %q", got, want)
			}
			if e.instance == "postgres" {
				// pg_restore --clean --if-exists over the rows already there.
				e.exec(t, "DELETE FROM xfer_rows WHERE id = 3")
				imp.SqlImportOptions.PostgresImportOptions = &sqladmin.ImportContextSqlImportOptionsPostgresImportOptions{Clean: true, IfExists: true}
				op, err = svc.Instances.Import(p, e.instance, &sqladmin.InstancesImportRequest{ImportContext: imp}).Do()
				if err != nil {
					t.Fatal(err)
				}
				if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
					t.Fatalf("parallel import with clean: %s", op.Error.Errors[0].Message)
				}
				if got := e.rows(t); got != want {
					t.Errorf("rows after the clean parallel import: %q, want %q", got, want)
				}
			}
		})
	}
}

// TestCloudSQLExportMasterData (#1421): a MySQL export with
// mysqlExportOptions.masterData 1 holds the binary log coordinates as a
// CHANGE REPLICATION SOURCE TO statement (mysqldump --source-data), and
// with 2 as a comment; any other value, or on PostgreSQL, is 400.
// covers: sql.instances.export
func TestCloudSQLExportMasterData(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	p := h.Project()
	bh := sqlTransferBucket(t, h, "sqlmd")
	md := func(n int64) *sqladmin.ExportContextSqlExportOptions {
		return &sqladmin.ExportContextSqlExportOptions{MysqlExportOptions: &sqladmin.ExportContextSqlExportOptionsMysqlExportOptions{MasterData: n}}
	}
	if os.Getenv(EnvCloudSQL) != "" {
		_, err := svc.Instances.Export(p, "postgres", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
			Uri: "gs://" + bh.BucketName() + "/pg.sql", Databases: []string{"cloudburrow"}, SqlExportOptions: md(1)}}).Do()
		wantHTTP(t, "masterData on PostgreSQL", err, 400)
	}
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	_, err := svc.Instances.Export(p, "mysql", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://" + bh.BucketName() + "/x.sql", Databases: []string{"cloudburrow"}, SqlExportOptions: md(3)}}).Do()
	wantHTTP(t, "masterData 3", err, 400)
	for _, c := range []struct {
		n      int64
		prefix string
	}{{1, "CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE="}, {2, "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE="}} {
		name := fmt.Sprintf("md%d.sql", c.n)
		op, err := svc.Instances.Export(p, "mysql", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
			Uri: "gs://" + bh.BucketName() + "/" + name, Databases: []string{"cloudburrow"}, SqlExportOptions: md(c.n)}}).Do()
		if err != nil {
			t.Fatal(err)
		}
		if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
			t.Fatalf("masterData %d: %s", c.n, op.Error.Errors[0].Message)
		}
		found := false
		for _, line := range strings.Split(sqlReadObject(t, h, bh, name), "\n") {
			found = found || strings.HasPrefix(line, c.prefix)
		}
		if !found {
			t.Errorf("the masterData %d dump has no line starting %q", c.n, c.prefix)
		}
	}
}

// TestCloudSQLCloneToBinLogCoordinates (#1421): a clone of a MySQL instance
// with binary logging on, to binLogCoordinates read from the source with
// SHOW BINARY LOG STATUS, holds what was committed before them and not
// what came after, read through the official connector; binLogCoordinates
// on PostgreSQL, with a pointInTime, or on an instance without binary
// logging are 400, and databaseNames, SQL Server's, is 400.
// covers: sql.instances.clone
func TestCloudSQLCloneToBinLogCoordinates(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	clone := func(src string, c *sqladmin.CloneContext) (*sqladmin.Operation, error) {
		return svc.Instances.Clone(project, src, &sqladmin.InstancesCloneRequest{CloneContext: c}).Do()
	}
	coords := &sqladmin.BinLogCoordinates{BinLogFileName: "binlog.000001", BinLogPosition: 4}
	if os.Getenv(EnvCloudSQL) != "" {
		_, err := clone("postgres", &sqladmin.CloneContext{DestinationInstanceName: "bl-pg-" + suffix, BinLogCoordinates: coords})
		wantHTTP(t, "binLogCoordinates on PostgreSQL", err, 400)
		_, err = clone("postgres", &sqladmin.CloneContext{DestinationInstanceName: "bl-pg-" + suffix, DatabaseNames: []string{"cloudburrow"}})
		wantHTTP(t, "databaseNames on PostgreSQL", err, 400)
	}
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	_, err := clone("mysql", &sqladmin.CloneContext{DestinationInstanceName: "bl-up-" + suffix, BinLogCoordinates: coords})
	wantHTTP(t, "binLogCoordinates on an instance without binaryLogEnabled", err, 400)
	src := "bl-src-" + suffix
	in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: src, DatabaseVersion: "MYSQL_8_4", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, BinaryLogEnabled: true}}})
	waitRecoverable(t, svc, project, src)
	e := sqlEngine{mysql: true}
	db := e.root(t, h, in.ConnectionName)
	for _, q := range []string{"CREATE DATABASE app", "CREATE TABLE app.t (id INT PRIMARY KEY)", "INSERT INTO app.t VALUES (1)"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	var file, doDB, ignoreDB, gtids string
	var pos int64
	if err := db.QueryRow("SHOW BINARY LOG STATUS").Scan(&file, &pos, &doDB, &ignoreDB, &gtids); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO app.t VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	_, err = clone(src, &sqladmin.CloneContext{DestinationInstanceName: src + "-x", DatabaseNames: []string{"app"}})
	wantHTTP(t, "databaseNames on MySQL", err, 400)
	_, err = clone(src, &sqladmin.CloneContext{DestinationInstanceName: src + "-x", PointInTime: time.Now().UTC().Format(time.RFC3339Nano),
		BinLogCoordinates: &sqladmin.BinLogCoordinates{BinLogFileName: file, BinLogPosition: pos}})
	wantHTTP(t, "binLogCoordinates with pointInTime", err, 400)
	dest := src + "-bl"
	op, err := clone(src, &sqladmin.CloneContext{DestinationInstanceName: dest,
		BinLogCoordinates: &sqladmin.BinLogCoordinates{BinLogFileName: file, BinLogPosition: pos}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, dest).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	if op.OperationType != "CLONE" {
		t.Errorf("operation type %s", op.OperationType)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("clone to %s:%d: %s", file, pos, op.Error.Errors[0].Message)
	}
	got, err := svc.Instances.Get(project, dest).Do()
	if err != nil {
		t.Fatal(err)
	}
	cdb := e.root(t, h, got.ConnectionName)
	var ids string
	eventually(t, "read the clone", func() error {
		return cdb.QueryRow("SELECT GROUP_CONCAT(id ORDER BY id) FROM app.t").Scan(&ids)
	})
	if ids != "1" {
		t.Errorf("rows in the clone to %s:%d: %q, want 1", file, pos, ids)
	}
}

// TestCloudSQLUpgradeWithReplicas (#1421): a major version upgrade of a
// primary with a read replica, as Cloud SQL documents it: without
// includeReplicasForMajorVersionUpgrade it is 400, since the replicas
// would have to be upgraded first; with it the primary is upgraded and then
// its replica, which reports the new version, is RUNNABLE, and receives the
// rows written on the primary afterwards, read through the official
// connector.
// covers: sql.instances.patch
func TestCloudSQLUpgradeWithReplicas(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	type path struct{ from, to string }
	var paths []path
	if os.Getenv(EnvCloudSQL) != "" {
		paths = append(paths, path{"POSTGRES_16", "POSTGRES_17"})
	}
	if os.Getenv(EnvMySQL) != "" {
		paths = append(paths, path{"MYSQL_8_0", "MYSQL_8_4"})
	}
	for _, p := range paths {
		t.Run(p.from, func(t *testing.T) {
			t.Parallel()
			e := sqlEngine{mysql: strings.HasPrefix(p.from, "MYSQL_")}
			name := "ur-" + strings.ToLower(strings.ReplaceAll(p.from, "_", "-")) + "-" + suffix
			in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: p.from,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
			db := e.root(t, h, in.ConnectionName)
			table := "t"
			stmts := []string{"CREATE TABLE t (v varchar(20))", "INSERT INTO t VALUES ('before')"}
			if e.mysql {
				table = "app.t"
				stmts = []string{"CREATE DATABASE app", "CREATE TABLE app.t (v varchar(20))", "INSERT INTO app.t VALUES ('before')"}
			}
			for _, q := range stmts {
				q := q
				eventually(t, q, func() error { _, err := db.Exec(q); return err })
			}
			rep := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name + "-r", MasterInstanceName: name,
				Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
			_, err := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: p.to}).Do()
			wantHTTP(t, "upgrade with a replica not upgraded", err, 400)
			op, err := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: p.to,
				IncludeReplicasForMajorVersionUpgrade: true}).Do()
			if err != nil {
				t.Fatal(err)
			}
			if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
				t.Fatalf("upgrade with replicas: %+v", op.Error.Errors[0])
			}
			for _, n := range []string{name, rep.Name} {
				got, err := svc.Instances.Get(project, n).Do()
				if err != nil || got.DatabaseVersion != p.to || got.State != "RUNNABLE" {
					t.Fatalf("%s after the upgrade: %+v %v", n, got, err)
				}
			}
			db = e.root(t, h, in.ConnectionName)
			eventually(t, "write after the upgrade", func() error { _, err := db.Exec("INSERT INTO " + table + " VALUES ('after')"); return err })
			rdb := e.root(t, h, rep.ConnectionName)
			eventually(t, "the replica's rows", func() error {
				var n int
				if err := rdb.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
					return err
				}
				if n != 2 {
					return fmt.Errorf("%d rows, want 2", n)
				}
				return nil
			})
		})
	}
}

// TestGcloudSQLParallelExportAndBinlogClone (#1421): `gcloud sql export sql
// --parallel --threads` writes the parallel export's folder, `gcloud sql
// import sql --parallel` restores it, and `gcloud sql instances clone
// --bin-log-file-name --bin-log-position` clones a MySQL instance to
// binary log coordinates.
func TestGcloudSQLParallelExportAndBinlogClone(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	g := newGcloudSession(t, h)
	bh := sqlTransferBucket(t, h, "gsqlpar")
	var pg sqlTransferEngine
	for _, e := range sqlTransferEngines(t, h) {
		if e.instance == "postgres" {
			pg = e
		}
	}
	pg.exec(t, "DROP TABLE IF EXISTS xfer_rows", "CREATE TABLE xfer_rows (id INT PRIMARY KEY, name VARCHAR(40))", "INSERT INTO xfer_rows VALUES (1, 'g')")
	defer pg.exec(t, "DROP TABLE IF EXISTS xfer_rows")
	uri := "gs://" + bh.BucketName() + "/gpar"
	g.must("sql", "export", "sql", "postgres", uri, "--project", g.project, "--database=cloudburrow", "--parallel", "--threads=2", "--quiet")
	if names := strings.Join(sqlObjectNames(t, h, bh, "gpar/"), " "); !strings.Contains(names, "gpar/toc.dat") {
		t.Fatalf("gcloud's parallel export wrote %s", names)
	}
	pg.exec(t, "DROP TABLE xfer_rows")
	g.must("sql", "import", "sql", "postgres", uri, "--project", g.project, "--database=cloudburrow", "--parallel", "--threads=2", "--quiet")
	if got := pg.rows(t); got != "1=g" {
		t.Errorf("rows after gcloud's parallel import: %q", got)
	}
	if os.Getenv(EnvMySQL) == "" {
		return
	}
	svc := sqlAdminService(t, h)
	n := time.Now().UnixNano() % 1e6
	src, clone := fmt.Sprintf("gcloud-bl-%d", n), fmt.Sprintf("gcloud-bl-%d-cl", n)
	g.must("sql", "instances", "create", src, "--project", g.project, "--database-version=MYSQL_8_4",
		"--tier=db-custom-1-3840", "--region=us-central1", "--enable-bin-log", "--backup", "--root-password=Root-pass1", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", src, "--project", g.project, "--quiet")
	waitRecoverable(t, svc, g.project, src)
	e := sqlEngine{mysql: true}
	db := e.root(t, h, g.project+":us-central1:"+src)
	for _, q := range []string{"CREATE DATABASE app", "CREATE TABLE app.t (id INT PRIMARY KEY)", "INSERT INTO app.t VALUES (1)"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	var file, doDB, ignoreDB, gtids string
	var pos int64
	if err := db.QueryRow("SHOW BINARY LOG STATUS").Scan(&file, &pos, &doDB, &ignoreDB, &gtids); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO app.t VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	g.must("sql", "instances", "clone", src, clone, "--project", g.project, "--bin-log-file-name", file,
		"--bin-log-position", fmt.Sprint(pos), "--quiet")
	defer g.run(nil, "sql", "instances", "delete", clone, "--project", g.project, "--quiet")
	cdb := e.root(t, h, g.project+":us-central1:"+clone)
	var ids string
	eventually(t, "read the gcloud clone", func() error { return cdb.QueryRow("SELECT GROUP_CONCAT(id ORDER BY id) FROM app.t").Scan(&ids) })
	if ids != "1" {
		t.Errorf("rows in the gcloud clone to %s:%d: %q, want 1", file, pos, ids)
	}
}

// TestConsoleCloudSQLParallelExportAndImport (#1421): the instance page's
// Export dialog offers Parallel, Threads and the MySQL binary log
// coordinates (masterData), and Import offers Parallel, Threads, Clean and
// If exists; a parallel export through it writes pg_dump's directory under
// the folder, and a parallel import through it restores the table, read
// back with pgx.
func TestConsoleCloudSQLParallelExportAndImport(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	project := h.Project()
	var pg sqlTransferEngine
	for _, e := range sqlTransferEngines(t, h) {
		if e.instance == "postgres" {
			pg = e
		}
	}
	bh := sqlTransferBucket(t, h, "csqlpar")
	pg.exec(t, "DROP TABLE IF EXISTS xfer_rows", "CREATE TABLE xfer_rows (id INT PRIMARY KEY, name VARCHAR(40))",
		"INSERT INTO xfer_rows VALUES (1, 'console')")
	defer pg.exec(t, "DROP TABLE IF EXISTS xfer_rows")
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/cloudsql-instances?project="+project+"&name=postgres", "")
	for _, f := range []string{`"name":"parallel"`, `"name":"threads"`, `"name":"masterData"`, `"name":"clean"`, `"name":"ifExists"`} {
		if code != http.StatusOK || !strings.Contains(out, f) {
			t.Fatalf("the instance's page has no %s field = %d: %.300s", f, code, out)
		}
	}
	act := func(action string, values map[string]string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{"postgres"}, "Action": action, "Values": values})
		if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project, string(body)); code != http.StatusOK {
			t.Fatalf("console %s = %d: %s", action, code, out)
		}
	}
	uri := "gs://" + bh.BucketName() + "/console-par"
	act("export", map[string]string{"uri": uri, "fileType": "SQL", "database": "cloudburrow", "parallel": "true", "threads": "2"})
	found := false
	for i := 0; i < 240 && !found; i++ {
		found = strings.Contains(strings.Join(sqlObjectNames(t, h, bh, "console-par/"), " "), "console-par/toc.dat")
		if !found {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("the console's parallel export wrote no toc.dat")
	}
	time.Sleep(time.Second) // the operation finishes after its last file
	pg.exec(t, "DROP TABLE xfer_rows")
	act("import", map[string]string{"uri": uri, "fileType": "SQL", "database": "cloudburrow", "parallel": "true", "threads": "2"})
	c, err := pgx.Connect(h.Context(), fmt.Sprintf("postgres://cloudburrow@%s/cloudburrow?sslmode=disable", h.Endpoint(EnvCloudSQL)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var got string
	for i := 0; i < 240 && got != "1=console"; i++ {
		time.Sleep(500 * time.Millisecond)
		var s string
		if err := c.QueryRow(h.Context(), "SELECT coalesce(string_agg(id::text || '=' || name, ';'), '') FROM xfer_rows").Scan(&s); err == nil {
			got = s
		}
	}
	if got != "1=console" {
		t.Errorf("rows after the console's parallel import: %q", got)
	}
}
