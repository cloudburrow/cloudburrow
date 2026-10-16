package sqladmin

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

type fakeTransferer struct {
	mu    sync.Mutex
	got   []string
	block chan struct{} // ExportSQL waits on it, or on ctx, when set
}

func (f *fakeTransferer) note(s string) { f.mu.Lock(); f.got = append(f.got, s); f.mu.Unlock() }

func (f *fakeTransferer) ExportSQL(ctx context.Context, t DumpTarget, x SQLExport, w io.Writer) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.note("exportSQL " + strings.Join(x.Databases, ","))
	_, err := io.WriteString(w, "CREATE TABLE t (id int);\n")
	return err
}

func (f *fakeTransferer) ImportSQL(_ context.Context, t DumpTarget, db, user string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.note("importSQL " + db + " " + string(b))
	return nil
}

func (f *fakeTransferer) ExportCSV(_ context.Context, t DumpTarget, db, q string, o CSVOptions, w io.Writer) error {
	f.note("exportCSV " + db + " " + q + " [" + o.Fields + o.Quote + o.Escape + "]")
	_, err := io.WriteString(w, "1;a\n")
	return err
}

func (f *fakeTransferer) ImportCSV(_ context.Context, t DumpTarget, db, table string, cols []string, o CSVOptions, user string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.note("importCSV " + db + " " + table + " " + strings.Join(cols, ",") + " " + string(b))
	return nil
}

type fakeObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (f *fakeObjects) Read(_ context.Context, b, o string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.objs[b+"/"+o]
	if !ok {
		return nil, Errorf(http.StatusNotFound, "no gs://%s/%s", b, o)
	}
	return d, nil
}

func (f *fakeObjects) Create(_ context.Context, b, o string, d []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objs[b+"/"+o]; ok {
		return Errorf(http.StatusConflict, "exists")
	}
	f.objs[b+"/"+o] = d
	return nil
}

func transferServer(t *testing.T) (*sqladmin.Service, *fakeTransferer, *fakeObjects) {
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	tr, obj := &fakeTransferer{}, &fakeObjects{objs: map[string][]byte{}}
	srv.EnableTransfers(tr, obj)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc, tr, obj
}

func waitOp(t *testing.T, svc *sqladmin.Service, op *sqladmin.Operation) *sqladmin.Operation {
	t.Helper()
	for i := 0; i < 200; i++ {
		got, err := svc.Operations.Get("proj", op.Name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "DONE" {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s never finished", op.Name)
	return nil
}

func TestExportAndImportThroughTheGoClient(t *testing.T) {
	svc, tr, obj := transferServer(t)
	op, err := svc.Instances.Export("proj", "pg", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://b/d.sql", FileType: "SQL", Databases: []string{"cloudburrow"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationType != "EXPORT" || op.ExportContext == nil || op.ExportContext.Uri != "gs://b/d.sql" {
		t.Errorf("export operation: %+v", op)
	}
	if done := waitOp(t, svc, op); done.Error != nil {
		t.Fatalf("export failed: %+v", done.Error.Errors[0])
	}
	if string(obj.objs["b/d.sql"]) != "CREATE TABLE t (id int);\n" {
		t.Errorf("exported object: %q", obj.objs["b/d.sql"])
	}
	// The file exists now: the request succeeds, the operation fails.
	op, err = svc.Instances.Export("proj", "pg", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://b/d.sql", Databases: []string{"cloudburrow"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if done := waitOp(t, svc, op); done.Error == nil {
		t.Error("a second export to the same file succeeded")
	}
	op, err = svc.Instances.Import("proj", "pg", &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
		Uri: "gs://b/d.sql", FileType: "SQL", Database: "cloudburrow"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if done := waitOp(t, svc, op); done.Error != nil || done.OperationType != "IMPORT" {
		t.Fatalf("import: %+v", done)
	}
	op, err = svc.Instances.Export("proj", "pg", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://b/q.csv", FileType: "CSV", Databases: []string{"cloudburrow"},
		CsvExportOptions: &sqladmin.ExportContextCsvExportOptions{SelectQuery: "SELECT 1", FieldsTerminatedBy: "3B",
			QuoteCharacter: "27", EscapeCharacter: "5C"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, svc, op)
	op, err = svc.Instances.Import("proj", "pg", &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
		Uri: "gs://b/q.csv", FileType: "CSV", Database: "cloudburrow",
		CsvImportOptions: &sqladmin.ImportContextCsvImportOptions{Table: "t", Columns: []string{"id", "v"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, svc, op)
	want := []string{"exportSQL cloudburrow", "exportSQL cloudburrow", "importSQL cloudburrow CREATE TABLE t (id int);\n",
		"exportCSV cloudburrow SELECT 1 [;'\\]", "importCSV cloudburrow t id,v 1;a\n"}
	if strings.Join(tr.got, "|") != strings.Join(want, "|") {
		t.Errorf("ran %q, want %q", tr.got, want)
	}
	for _, bad := range []*sqladmin.ExportContext{
		{Uri: "b/d.sql", Databases: []string{"x"}},
		{Uri: "gs://b/x.sql"}, // PostgreSQL names one database
		{Uri: "gs://b/x.csv", FileType: "CSV", Databases: []string{"x"}},
	} {
		if _, err := svc.Instances.Export("proj", "pg", &sqladmin.InstancesExportRequest{ExportContext: bad}).Do(); err == nil ||
			!strings.Contains(err.Error(), "400") {
			t.Errorf("export %+v: %v, want 400", bad, err)
		}
	}
	if _, err := svc.Instances.Export("proj", "pg", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://b/x.bak", FileType: "BAK", Databases: []string{"x"}}}).Do(); err == nil || !strings.Contains(err.Error(), "501") {
		t.Errorf("BAK export: %v, want 501", err)
	}
}

func TestCancelStopsARunningExport(t *testing.T) {
	svc, tr, obj := transferServer(t)
	tr.block = make(chan struct{})
	op, err := svc.Instances.Export("proj", "pg", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://b/slow.sql", Databases: []string{"cloudburrow"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Operations.Cancel("proj", op.Name).Do(); err != nil {
		t.Fatal(err)
	}
	done := waitOp(t, svc, op)
	if done.Error == nil || done.Error.Errors[0].Code != "CANCEL_SUCCESSFUL" {
		t.Errorf("cancelled export: %+v", done.Error)
	}
	if _, ok := obj.objs["b/slow.sql"]; ok {
		t.Error("a cancelled export wrote its file")
	}
	if _, err := svc.Operations.Cancel("proj", op.Name).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("cancel of a done operation: %v, want 400", err)
	}
	dbop, _ := svc.Databases.Insert("proj", "pg", &sqladmin.Database{Name: "x"}).Do()
	if _, err := svc.Operations.Cancel("proj", dbop.Name).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("cancel of a database insert: %v, want 400", err)
	}
}

func TestSSLCertsAreSignedByTheServerCA(t *testing.T) {
	svc, _, _ := transferServer(t)
	res, err := svc.SslCerts.Insert("proj", "pg", &sqladmin.SslCertsInsertRequest{CommonName: "app"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	ca := parseCertPEM(res.ServerCaCert.Cert)
	cert := parseCertPEM(res.ClientCert.CertInfo.Cert)
	if ca == nil || cert == nil || cert.CheckSignatureFrom(ca) != nil || cert.Subject.CommonName != "app" {
		t.Fatalf("client certificate not signed by the server CA: %+v", res.ClientCert.CertInfo)
	}
	if b, _ := pem.Decode([]byte(res.ClientCert.CertPrivateKey)); b == nil {
		t.Error("no private key")
	} else if _, err := x509.ParsePKCS1PrivateKey(b.Bytes); err != nil {
		t.Error(err)
	}
	if _, err := svc.SslCerts.Insert("proj", "pg", &sqladmin.SslCertsInsertRequest{CommonName: "app"}).Do(); err == nil {
		t.Error("a second certificate with the same name was made")
	}
	fp := res.ClientCert.CertInfo.Sha1Fingerprint
	got, err := svc.SslCerts.Get("proj", "pg", fp).Do()
	if err != nil || got.CommonName != "app" {
		t.Fatalf("get: %v %+v", err, got)
	}
	list, err := svc.SslCerts.List("proj", "pg").Do()
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	if _, err := svc.SslCerts.Delete("proj", "pg", fp).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SslCerts.Get("proj", "pg", fp).Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("get after delete: %v", err)
	}
	if _, err := svc.SslCerts.Insert("proj", "pg", &sqladmin.SslCertsInsertRequest{CommonName: "b"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Instances.ResetSslConfig("proj", "pg").Do(); err != nil {
		t.Fatal(err)
	}
	if list, err := svc.SslCerts.List("proj", "pg").Do(); err != nil || len(list.Items) != 0 {
		t.Errorf("list after resetSslConfig: %v %+v", err, list)
	}
}

func TestTiersAndFlags(t *testing.T) {
	svc, _, _ := transferServer(t)
	tiers, err := svc.Tiers.List("proj").Do()
	if err != nil || len(tiers.Items) != len(documentedTiers) {
		t.Fatalf("tiers: %v", err)
	}
	all, err := svc.Flags.List().Do()
	if err != nil || len(all.Items) != len(documentedFlags) {
		t.Fatalf("flags: %v", err)
	}
	pg, err := svc.Flags.List().DatabaseVersion("POSTGRES_17").Do()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range pg.Items {
		if len(f.AppliesTo) != 1 || f.AppliesTo[0] != "POSTGRES_17" {
			t.Errorf("flag %s applies to %v", f.Name, f.AppliesTo)
		}
	}
}
