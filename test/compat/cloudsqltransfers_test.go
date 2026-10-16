//go:build compat

package compat

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// sqlTransferEngine is one server an import/export test reads and writes.
type sqlTransferEngine struct {
	instance string
	exec     func(t *testing.T, stmts ...string)
	rows     func(t *testing.T) string // "id=name;..." of xfer_rows
}

func sqlTransferEngines(t *testing.T, h *Harness) []sqlTransferEngine {
	var out []sqlTransferEngine
	if os.Getenv(EnvCloudSQL) != "" {
		pg := func(t *testing.T) func(string) {
			c := pgConnect(t, h, "cloudburrow")
			return func(s string) {
				t.Helper()
				if _, err := c.Exec(h.Context(), s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
		}
		out = append(out, sqlTransferEngine{instance: "postgres",
			exec: func(t *testing.T, stmts ...string) {
				run := pg(t)
				for _, s := range stmts {
					run(s)
				}
			},
			rows: func(t *testing.T) string {
				c := pgConnect(t, h, "cloudburrow")
				var s string
				if err := c.QueryRow(h.Context(), "SELECT coalesce(string_agg(id::text || '=' || name, ';' ORDER BY id), '') FROM xfer_rows").Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}})
	}
	if os.Getenv(EnvMySQL) != "" {
		db := func(t *testing.T) *sql.DB { return mysqlDB(t, h) }
		out = append(out, sqlTransferEngine{instance: "mysql",
			exec: func(t *testing.T, stmts ...string) {
				d := db(t)
				for _, s := range stmts {
					if _, err := d.ExecContext(h.Context(), s); err != nil {
						t.Fatalf("%s: %v", s, err)
					}
				}
			},
			rows: func(t *testing.T) string {
				var s sql.NullString
				if err := db(t).QueryRowContext(h.Context(),
					"SELECT GROUP_CONCAT(CONCAT(id, '=', name) ORDER BY id SEPARATOR ';') FROM xfer_rows").Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s.String
			}})
	}
	if len(out) == 0 {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	return out
}

func sqlReadObject(t *testing.T, h *Harness, bh *storage.BucketHandle, name string) string {
	t.Helper()
	r, err := bh.Object(name).NewReader(h.Context())
	if err != nil {
		t.Fatalf("read gs://%s/%s: %v", bh.BucketName(), name, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func sqlWriteObject(t *testing.T, h *Harness, bh *storage.BucketHandle, name, data string) {
	t.Helper()
	w := bh.Object(name).NewWriter(h.Context())
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// sqlTransferBucket is a fresh bucket in the instance's Cloud Storage.
func sqlTransferBucket(t *testing.T, h *Harness, prefix string) *storage.BucketHandle {
	t.Helper()
	sc := instanceStorage(t, h)
	bh := sc.Bucket(fmt.Sprintf("%s-%s-%d", h.Project(), prefix, time.Now().UnixNano()%1e9))
	if err := bh.Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	return bh
}

// waitSQLOpDone waits for op and returns it finished, failed or not.
func waitSQLOpDone(t *testing.T, svc *sqladmin.Service, project string, op *sqladmin.Operation) *sqladmin.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for op.Status != "DONE" {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s (%s) did not finish", op.Name, op.OperationType)
		}
		time.Sleep(500 * time.Millisecond)
		var err error
		if op, err = svc.Operations.Get(project, op.Name).Do(); err != nil {
			t.Fatal(err)
		}
	}
	return op
}

// TestCloudSQLImportAndExport (#1380): through the official Go client, an
// export writes a SQL dump (pg_dump / mysqldump) or a query's CSV, with the
// csvExportOptions characters, to the instance's own Cloud Storage as an
// EXPORT operation; an import loads either back as an IMPORT operation; an
// export to a file that exists fails its operation, a .gz file is gzip, BAK
// is 501, and a done operation, or one that is not an import or export,
// cannot be cancelled.
// covers: sql.instances.export, sql.instances.import, sql.operations.cancel
func TestCloudSQLImportAndExport(t *testing.T) {
	h := New(t)
	engines := sqlTransferEngines(t, h)
	svc := sqlAdminService(t, h)
	p := h.Project()
	bh := sqlTransferBucket(t, h, "sqlx")
	uri := func(name string) string { return "gs://" + bh.BucketName() + "/" + name }
	for _, e := range engines {
		t.Run(e.instance, func(t *testing.T) {
			e.exec(t, "DROP TABLE IF EXISTS xfer_rows", "CREATE TABLE xfer_rows (id INT PRIMARY KEY, name VARCHAR(40))",
				`INSERT INTO xfer_rows VALUES (1, 'plain'), (2, 'a,comma'), (3, 'say "hi"')`)
			defer e.exec(t, "DROP TABLE IF EXISTS xfer_rows")
			want := `1=plain;2=a,comma;3=say "hi"`
			export := func(x *sqladmin.ExportContext) *sqladmin.Operation {
				t.Helper()
				op, err := svc.Instances.Export(p, e.instance, &sqladmin.InstancesExportRequest{ExportContext: x}).Do()
				if err != nil {
					t.Fatal(err)
				}
				if op.OperationType != "EXPORT" || op.ExportContext == nil || op.ExportContext.Uri != x.Uri {
					t.Errorf("export operation: %+v", op)
				}
				return waitSQLOpDone(t, svc, p, op)
			}
			imp := func(x *sqladmin.ImportContext) {
				t.Helper()
				op, err := svc.Instances.Import(p, e.instance, &sqladmin.InstancesImportRequest{ImportContext: x}).Do()
				if err != nil {
					t.Fatal(err)
				}
				if op.OperationType != "IMPORT" {
					t.Errorf("import operation: %+v", op)
				}
				if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
					t.Fatalf("import %s: %s", x.Uri, op.Error.Errors[0].Message)
				}
			}
			dump := e.instance + ".sql"
			if op := export(&sqladmin.ExportContext{Uri: uri(dump), FileType: "SQL", Databases: []string{"cloudburrow"}}); op.Error != nil {
				t.Fatalf("export: %s", op.Error.Errors[0].Message)
			}
			if got := sqlReadObject(t, h, bh, dump); !strings.Contains(got, "xfer_rows") || !strings.Contains(got, "CREATE TABLE") {
				t.Fatalf("the dump holds no xfer_rows table:\n%.400s", got)
			}
			// The file exists: the request succeeds and its operation fails.
			if op := export(&sqladmin.ExportContext{Uri: uri(dump), FileType: "SQL", Databases: []string{"cloudburrow"}}); op.Error == nil {
				t.Error("a second export to the same file succeeded")
			}
			gz := e.instance + ".sql.gz"
			if op := export(&sqladmin.ExportContext{Uri: uri(gz), Databases: []string{"cloudburrow"},
				SqlExportOptions: &sqladmin.ExportContextSqlExportOptions{Tables: []string{"xfer_rows"}}}); op.Error != nil {
				t.Fatalf("gzip export: %s", op.Error.Errors[0].Message)
			}
			if got := sqlReadObject(t, h, bh, gz); !strings.HasPrefix(got, "\x1f\x8b") {
				t.Errorf("the .gz export is not gzip: %q", got[:min(len(got), 8)])
			}
			e.exec(t, "DROP TABLE xfer_rows")
			imp(&sqladmin.ImportContext{Uri: uri(gz), FileType: "SQL", Database: "cloudburrow"})
			if got := e.rows(t); got != want {
				t.Errorf("rows after the SQL import: %q, want %q", got, want)
			}
			csvName := e.instance + ".csv"
			if op := export(&sqladmin.ExportContext{Uri: uri(csvName), FileType: "CSV", Databases: []string{"cloudburrow"},
				CsvExportOptions: &sqladmin.ExportContextCsvExportOptions{SelectQuery: "SELECT id, name FROM xfer_rows ORDER BY id"}}); op.Error != nil {
				t.Fatalf("CSV export: %s", op.Error.Errors[0].Message)
			}
			got := sqlReadObject(t, h, bh, csvName)
			if !strings.Contains(got, `"a,comma"`) || !strings.Contains(got, `"say ""hi"""`) {
				t.Errorf("CSV export, Cloud SQL's default quoting expected:\n%s", got)
			}
			semi := e.instance + "-semi.csv"
			if op := export(&sqladmin.ExportContext{Uri: uri(semi), FileType: "CSV", Databases: []string{"cloudburrow"},
				CsvExportOptions: &sqladmin.ExportContextCsvExportOptions{SelectQuery: "SELECT id, name FROM xfer_rows WHERE id = 1",
					FieldsTerminatedBy: "3B"}}); op.Error != nil {
				t.Fatalf("CSV export with fieldsTerminatedBy: %s", op.Error.Errors[0].Message)
			}
			if got := strings.TrimSpace(sqlReadObject(t, h, bh, semi)); got != `1;"plain"` && got != "1;plain" {
				t.Errorf("fieldsTerminatedBy 3B wrote %q", got)
			}
			e.exec(t, "DELETE FROM xfer_rows")
			imp(&sqladmin.ImportContext{Uri: uri(csvName), FileType: "CSV", Database: "cloudburrow",
				CsvImportOptions: &sqladmin.ImportContextCsvImportOptions{Table: "xfer_rows", Columns: []string{"id", "name"}}})
			if got := e.rows(t); got != want {
				t.Errorf("rows after the CSV import: %q, want %q", got, want)
			}
			sqlWriteObject(t, h, bh, e.instance+"-in.csv", "7;'x;y'\n")
			imp(&sqladmin.ImportContext{Uri: uri(e.instance + "-in.csv"), FileType: "CSV", Database: "cloudburrow",
				CsvImportOptions: &sqladmin.ImportContextCsvImportOptions{Table: "xfer_rows", FieldsTerminatedBy: "3B", QuoteCharacter: "27"}})
			if got := e.rows(t); !strings.HasSuffix(got, ";7=x;y") {
				t.Errorf("rows after a CSV import with fieldsTerminatedBy 3B, quoteCharacter 27: %q", got)
			}
			op := export(&sqladmin.ExportContext{Uri: uri(e.instance + "-again.sql"), Databases: []string{"cloudburrow"}})
			_, err := svc.Operations.Cancel(p, op.Name).Do()
			wantHTTP(t, "operations.cancel of a done export", err, http.StatusBadRequest)
			_, err = svc.Instances.Export(p, e.instance, &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
				Uri: uri("x.bak"), FileType: "BAK", Databases: []string{"cloudburrow"}}}).Do()
			wantHTTP(t, "a BAK export", err, http.StatusNotImplemented)
			_, err = svc.Instances.Import(p, e.instance, &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
				Uri: uri("nope.csv"), FileType: "CSV", Database: "cloudburrow"}}).Do()
			wantHTTP(t, "a CSV import without a table", err, http.StatusBadRequest)
		})
	}
	dbop, err := svc.Databases.Insert(p, engines[0].instance, &sqladmin.Database{Name: "xfer_cancel"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = svc.Databases.Delete(p, engines[0].instance, "xfer_cancel").Do() }()
	_, err = svc.Operations.Cancel(p, dbop.Name).Do()
	wantHTTP(t, "operations.cancel of a CREATE_DATABASE operation", err, http.StatusBadRequest)
}

// TestSQLAdminSSLCerts (#1380): sslCerts.insert returns a client
// certificate and its private key signed by the instance's server CA (the
// one connectSettings returns); list, get and delete see it, a second of the
// same name is refused, createEphemeral signs a public key, and
// resetSslConfig deletes them all.
// covers: sql.sslCerts.insert, sql.sslCerts.list, sql.sslCerts.get, sql.sslCerts.delete, sql.sslCerts.createEphemeral, sql.instances.resetSslConfig
func TestSQLAdminSSLCerts(t *testing.T) {
	h := New(t)
	engines := sqlTransferEngines(t, h)
	svc := sqlAdminService(t, h)
	p := h.Project()
	inst := engines[0].instance
	cs, err := svc.Connect.Get(p, inst).Do()
	if err != nil {
		t.Fatal(err)
	}
	parse := func(s string) *x509.Certificate {
		b, _ := pem.Decode([]byte(s))
		if b == nil {
			t.Fatalf("not PEM: %q", s)
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca := parse(cs.ServerCaCert.Cert)
	res, err := svc.SslCerts.Insert(p, inst, &sqladmin.SslCertsInsertRequest{CommonName: "xfer-app"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	cert := parse(res.ClientCert.CertInfo.Cert)
	if err := cert.CheckSignatureFrom(ca); err != nil || cert.Subject.CommonName != "xfer-app" {
		t.Errorf("the client certificate is not the server CA's: %v %s", err, cert.Subject)
	}
	if res.ServerCaCert == nil || res.ServerCaCert.Cert != cs.ServerCaCert.Cert || !strings.Contains(res.ClientCert.CertPrivateKey, "PRIVATE KEY") {
		t.Errorf("insert response: %+v", res)
	}
	_, err = svc.SslCerts.Insert(p, inst, &sqladmin.SslCertsInsertRequest{CommonName: "xfer-app"}).Do()
	wantHTTP(t, "a second sslCerts.insert of the same name", err, http.StatusConflict)
	fp := res.ClientCert.CertInfo.Sha1Fingerprint
	if got, err := svc.SslCerts.Get(p, inst, fp).Do(); err != nil || got.CommonName != "xfer-app" || got.Sha1Fingerprint != fp {
		t.Errorf("sslCerts.get: %v %+v", err, got)
	}
	list, err := svc.SslCerts.List(p, inst).Do()
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("sslCerts.list: %v %+v", err, list)
	}
	if _, err := svc.SslCerts.Delete(p, inst, fp).Do(); err != nil {
		t.Fatal(err)
	}
	_, err = svc.SslCerts.Get(p, inst, fp).Do()
	wantHTTP(t, "sslCerts.get after delete", err, http.StatusNotFound)
	if _, err := svc.SslCerts.Insert(p, inst, &sqladmin.SslCertsInsertRequest{CommonName: "xfer-reset"}).Do(); err != nil {
		t.Fatal(err)
	}
	if op, err := svc.Instances.ResetSslConfig(p, inst).Do(); err != nil {
		t.Fatal(err)
	} else {
		waitSQLOp(t, svc, p, op)
	}
	if list, err := svc.SslCerts.List(p, inst).Do(); err != nil || len(list.Items) != 0 {
		t.Errorf("sslCerts.list after resetSslConfig: %v %+v", err, list)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	eph, err := svc.SslCerts.CreateEphemeral(p, inst, &sqladmin.SslCertsCreateEphemeralRequest{PublicKey: pub}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if c := parse(eph.Cert); c.CheckSignatureFrom(ca) != nil {
		t.Error("createEphemeral's certificate is not the server CA's")
	}
}

// TestSQLAdminTiersAndFlags (#1380): tiers.list and flags.list answer
// Google's documented lists, flags filtered by databaseVersion.
// covers: sql.tiers.list, sql.flags.list
func TestSQLAdminTiersAndFlags(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	tiers, err := svc.Tiers.List(h.Project()).Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tr := range tiers.Items {
		if tr.Tier == "db-custom-1-3840" {
			found = tr.RAM == 3840<<20 && tr.DiskQuota > 0
		}
	}
	if !found {
		t.Errorf("tiers.list has no db-custom-1-3840 of 3840 MiB: %d tiers", len(tiers.Items))
	}
	for _, c := range []struct{ version, flag, typ string }{
		{"POSTGRES_17", "max_connections", "INTEGER"},
		{"MYSQL_8_4", "max_connections", "INTEGER"},
		{"POSTGRES_17", "log_min_duration_statement", "INTEGER"},
		{"MYSQL_8_4", "slow_query_log", "BOOLEAN"},
	} {
		list, err := svc.Flags.List().DatabaseVersion(c.version).Do()
		if err != nil {
			t.Fatal(err)
		}
		var hit *sqladmin.Flag
		for _, f := range list.Items {
			if len(f.AppliesTo) != 1 || f.AppliesTo[0] != c.version {
				t.Fatalf("flags.list for %s returned %s applying to %v", c.version, f.Name, f.AppliesTo)
			}
			if f.Name == c.flag {
				hit = f
			}
		}
		if hit == nil || hit.Type != c.typ {
			t.Errorf("flags.list %s: %s is %+v, want type %s", c.version, c.flag, hit, c.typ)
		}
	}
}

// TestGcloudSQLImportAndExport (#1380): gcloud sql export sql/csv and
// gcloud sql import sql/csv against the instance's Cloud Storage.
func TestGcloudSQLImportAndExport(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	e := sqlTransferEngines(t, h)[0]
	bh := sqlTransferBucket(t, h, "gsqlx")
	e.exec(t, "DROP TABLE IF EXISTS xfer_rows", "CREATE TABLE xfer_rows (id INT PRIMARY KEY, name VARCHAR(40))",
		"INSERT INTO xfer_rows VALUES (1, 'one'), (2, 'two')")
	defer e.exec(t, "DROP TABLE IF EXISTS xfer_rows")
	g := newGcloudSession(t, h)
	p := "--project=" + h.Project()
	gs := "gs://" + bh.BucketName() + "/"
	g.must("sql", "export", "sql", "postgres", gs+"g.sql", "--database=cloudburrow", p)
	g.must("sql", "export", "csv", "postgres", gs+"g.csv", "--database=cloudburrow", "--query=SELECT id, name FROM xfer_rows ORDER BY id", p)
	if got := sqlReadObject(t, h, bh, "g.csv"); !strings.Contains(got, "2,two") && !strings.Contains(got, `2,"two"`) {
		t.Errorf("gcloud sql export csv wrote %q", got)
	}
	e.exec(t, "DROP TABLE xfer_rows")
	g.must("sql", "import", "sql", "postgres", gs+"g.sql", "--database=cloudburrow", p, "--quiet")
	if got := e.rows(t); got != "1=one;2=two" {
		t.Errorf("rows after gcloud sql import sql: %q", got)
	}
	e.exec(t, "DELETE FROM xfer_rows")
	g.must("sql", "import", "csv", "postgres", gs+"g.csv", "--database=cloudburrow", "--table=xfer_rows", p, "--quiet")
	if got := e.rows(t); got != "1=one;2=two" {
		t.Errorf("rows after gcloud sql import csv: %q", got)
	}
	if out := g.must("sql", "tiers", "list", p, "--format=value(tier)"); !strings.Contains(out, "db-custom-1-3840") {
		t.Errorf("gcloud sql tiers list: %s", out)
	}
	if out := g.must("sql", "flags", "list", "--database-version=POSTGRES_17", "--format=value(name)"); !strings.Contains(out, "max_connections") {
		t.Errorf("gcloud sql flags list: %s", out)
	}
}
