//go:build compat

package compat

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	alloydb "cloud.google.com/go/alloydb/apiv1"
	"cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"cloud.google.com/go/alloydbconn"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// alloyDBCluster creates a cluster with a primary instance (and the
// initial user postgres / burrow-pw) through c, deleted when the test
// ends, and returns the cluster's name and its primary.
func alloyDBCluster(ctx context.Context, t *testing.T, h *Harness, c *alloydb.AlloyDBAdminClient, region, prefix string, primary *alloydbpb.Instance) (string, *alloydbpb.Instance) {
	t.Helper()
	loc := "projects/" + h.Project() + "/locations/" + region
	id := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e6)
	name := loc + "/clusters/" + id
	op, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{
		NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: "projects/" + h.Project() + "/global/networks/default"},
		InitialUser:   &alloydbpb.UserPassword{User: "postgres", Password: "burrow-pw"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { alloyDBDelete(t, c, name) })
	if primary == nil {
		primary = &alloydbpb.Instance{}
	}
	primary.InstanceType = alloydbpb.Instance_PRIMARY
	if primary.MachineConfig == nil {
		primary.MachineConfig = &alloydbpb.Instance_MachineConfig{CpuCount: 2}
	}
	iop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "primary", Instance: primary})
	if err != nil {
		t.Fatal(err)
	}
	in, err := waitLRO(ctx, iop)
	if err != nil {
		t.Fatal(err)
	}
	return name, in
}

// alloyDBDelete deletes a cluster with its instances, its secondaries
// first.
func alloyDBDelete(t *testing.T, c *alloydb.AlloyDBAdminClient, name string) {
	ctx := alloyDBWait(t)
	cl, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	if err != nil {
		return
	}
	for _, s := range cl.GetPrimaryConfig().GetSecondaryClusterNames() {
		alloyDBDelete(t, c, s)
	}
	if op, err := c.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: name, Force: true}); err == nil {
		_ = op.Wait(ctx)
	}
}

func alloyDBExec(ctx context.Context, t *testing.T, conn *pgx.Conn, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// TestAlloyDBExportAndImport (#2191): through the official client, over
// gRPC and REST, ExportCluster writes a SQL dump (pg_dump, with tables,
// schema only, clean) and a query's CSV (with hex delimiter, quote and
// escape characters, gzip for .gz) to the instance's own Cloud Storage,
// and ImportCluster loads a SQL file (into the database named or that a
// \connect names) and a CSV file (into a table's columns, as a user),
// answering the bytes downloaded. What Google refuses is refused.
// covers: google.cloud.alloydb.v1.AlloyDBAdmin/ExportCluster, google.cloud.alloydb.v1.AlloyDBAdmin/ImportCluster
func TestAlloyDBExportAndImport(t *testing.T) {
	h := New(t)
	clients := alloyDBClients(t, h)
	c := clients["grpc"]
	ctx := alloyDBWait(t)
	bh := sqlTransferBucket(t, h, "alloydbxfer")
	bucket := "gs://" + bh.BucketName() + "/"
	name, primary := alloyDBCluster(ctx, t, h, c, "us-central1", "xfer", nil)
	d := alloyDBDialer(t, h)
	pg := alloyDBConn(ctx, t, d, primary.GetName(), "postgres", "burrow-pw")
	alloyDBExec(ctx, t, pg, "CREATE DATABASE shop", "CREATE DATABASE copyshop")
	shop := alloyDBConnDB(ctx, t, d, primary.GetName(), "shop")
	alloyDBExec(ctx, t, shop, "CREATE TABLE items (id int PRIMARY KEY, name text, note text)",
		`INSERT INTO items VALUES (1, 'burrow', 'say "hi"'), (2, 'tunnel', 'a;b'), (3, 'den', NULL)`,
		"CREATE TABLE other (x int)", "INSERT INTO other VALUES (7)")

	read := func(object string) string {
		t.Helper()
		r, err := bh.Object(object).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", object, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}
	export := func(cl *alloydb.AlloyDBAdminClient, req *alloydbpb.ExportClusterRequest) (*alloydbpb.ExportClusterResponse, error) {
		t.Helper()
		req.Name = name
		op, err := cl.ExportCluster(ctx, req)
		if err != nil {
			return nil, err
		}
		return waitLRO(ctx, op)
	}
	gcs := func(object string) *alloydbpb.ExportClusterRequest_GcsDestination {
		return &alloydbpb.ExportClusterRequest_GcsDestination{GcsDestination: &alloydbpb.GcsDestination{Uri: bucket + object}}
	}

	// SQL, one table, over gRPC; the whole database, clean, over REST.
	resp, err := export(c, &alloydbpb.ExportClusterRequest{Database: "shop", Destination: gcs("items.sql"),
		ExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions{Tables: []string{"items"}}}})
	if err != nil || resp.GetGcsDestination().GetUri() != bucket+"items.sql" {
		t.Fatalf("SQL export: %v, %v", resp, err)
	}
	if dump := read("items.sql"); !strings.Contains(dump, "CREATE TABLE public.items") || strings.Contains(dump, "public.other") || !strings.Contains(dump, "tunnel") {
		t.Errorf("the SQL export of items:\n%s", dump)
	}
	clean := true
	if _, err := export(clients["rest"], &alloydbpb.ExportClusterRequest{Database: "shop", Destination: gcs("shop.sql.gz"),
		ExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions{
			CleanTargetObjects: &clean, IfExistTargetObjects: &clean}}}); err != nil {
		t.Fatalf("SQL export over REST: %v", err)
	}
	zr, err := gzip.NewReader(strings.NewReader(read("shop.sql.gz")))
	if err != nil {
		t.Fatalf("shop.sql.gz is not gzip: %v", err)
	}
	full, _ := io.ReadAll(zr)
	if s := string(full); !strings.Contains(s, "DROP TABLE IF EXISTS public.other") || !strings.Contains(s, "CREATE TABLE public.items") {
		t.Errorf("the clean SQL export:\n%s", s)
	}
	schemaOnly := true
	if _, err := export(c, &alloydbpb.ExportClusterRequest{Database: "shop", Destination: gcs("schema.sql"),
		ExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions{SchemaOnly: &schemaOnly}}}); err != nil {
		t.Fatal(err)
	}
	if s := read("schema.sql"); !strings.Contains(s, "CREATE TABLE public.items") || strings.Contains(s, "burrow") {
		t.Errorf("the schema-only export:\n%s", s)
	}

	// CSV: the default characters, then a semicolon delimiter in hex
	// with 0x, and a single quote.
	if _, err := export(c, &alloydbpb.ExportClusterRequest{Database: "shop", Destination: gcs("items.csv"),
		ExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{
			SelectQuery: "SELECT id, name, note FROM items ORDER BY id"}}}); err != nil {
		t.Fatal(err)
	}
	if got, want := read("items.csv"), "1,burrow,\"say \"\"hi\"\"\"\n2,tunnel,a;b\n3,den,\n"; got != want {
		t.Errorf("CSV export = %q, want %q", got, want)
	}
	if _, err := export(c, &alloydbpb.ExportClusterRequest{Database: "shop", Destination: gcs("items-semi.csv.gz"),
		ExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{
			SelectQuery: "SELECT id, name, note FROM items ORDER BY id", FieldDelimiter: "0x3B", QuoteCharacter: "27"}}}); err != nil {
		t.Fatal(err)
	}
	zr, err = gzip.NewReader(strings.NewReader(read("items-semi.csv.gz")))
	if err != nil {
		t.Fatal(err)
	}
	semi, _ := io.ReadAll(zr)
	if got, want := string(semi), "1;burrow;say \"hi\"\n2;tunnel;'a;b'\n3;den;\n"; got != want {
		t.Errorf("CSV export with ; and ' = %q, want %q", got, want)
	}

	// Refusals.
	for what, req := range map[string]*alloydbpb.ExportClusterRequest{
		"no database": {Destination: gcs("x.sql"), ExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions{}}},
		"no options":  {Database: "shop", Destination: gcs("x.sql")},
		"not gs://":   {Database: "shop", Destination: &alloydbpb.ExportClusterRequest_GcsDestination{GcsDestination: &alloydbpb.GcsDestination{Uri: "https://x/y"}}, ExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions{}}},
		"no query":    {Database: "shop", Destination: gcs("x.csv"), ExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{}}},
		"bad hex":     {Database: "shop", Destination: gcs("x.csv"), ExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{SelectQuery: "SELECT 1", FieldDelimiter: ";;"}}},
	} {
		req.Name = name
		if _, err := c.ExportCluster(ctx, req); alloyDBCode(err) != codes.InvalidArgument {
			t.Errorf("an export with %s: %v, want INVALID_ARGUMENT", what, err)
		}
	}

	// Import: the SQL dump of items into another database, as gRPC; a
	// file that \connects to shop; CSV into columns, as a user.
	imp := func(cl *alloydb.AlloyDBAdminClient, req *alloydbpb.ImportClusterRequest) (*alloydbpb.ImportClusterResponse, error) {
		t.Helper()
		req.Name = name
		op, err := cl.ImportCluster(ctx, req)
		if err != nil {
			return nil, err
		}
		return waitLRO(ctx, op)
	}
	sqlImport := &alloydbpb.ImportClusterRequest_SqlImportOptions_{SqlImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions{}}
	ir, err := imp(c, &alloydbpb.ImportClusterRequest{GcsUri: bucket + "items.sql", Database: "copyshop", ImportOptions: sqlImport})
	if err != nil || ir.GetBytesDownloaded() != int64(len(read("items.sql"))) {
		t.Fatalf("SQL import: %v, %v (the file is %d bytes)", ir, err, len(read("items.sql")))
	}
	copyshop := alloyDBConnDB(ctx, t, d, primary.GetName(), "copyshop")
	var n int
	if err := copyshop.QueryRow(ctx, "SELECT count(*) FROM items").Scan(&n); err != nil || n != 3 {
		t.Errorf("copyshop.items after the import: %d, %v", n, err)
	}
	var gzbuf bytes.Buffer
	gw := gzip.NewWriter(&gzbuf)
	_, _ = gw.Write([]byte("\\connect shop\nCREATE TABLE imported (v text);\nINSERT INTO imported VALUES ('via connect');\n"))
	_ = gw.Close()
	w := bh.Object("connect.sql.gz").NewWriter(ctx)
	_, _ = w.Write(gzbuf.Bytes())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := imp(clients["rest"], &alloydbpb.ImportClusterRequest{GcsUri: bucket + "connect.sql.gz", ImportOptions: sqlImport}); err != nil {
		t.Fatalf("a gzip SQL import with \\connect, over REST: %v", err)
	}
	var v string
	if err := shop.QueryRow(ctx, "SELECT v FROM imported").Scan(&v); err != nil || v != "via connect" {
		t.Errorf("shop.imported: %q, %v", v, err)
	}

	alloyDBExec(ctx, t, shop, "CREATE TABLE loaded (id int, name text, note text DEFAULT 'none')")
	if _, err := c.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: name, UserId: "loader", User: &alloydbpb.User{
		UserType: alloydbpb.User_ALLOYDB_BUILT_IN, Password: "loader-pw"}}); err != nil {
		t.Fatal(err)
	}
	alloyDBExec(ctx, t, shop, "GRANT INSERT ON loaded TO loader")
	w = bh.Object("load.csv").NewWriter(ctx)
	_, _ = w.Write([]byte("10;'semi;colon'\n11;plain\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := imp(c, &alloydbpb.ImportClusterRequest{GcsUri: bucket + "load.csv", Database: "shop", User: "loader",
		ImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions_{CsvImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions{
			Table: "loaded", Columns: []string{"id", "name"}, FieldDelimiter: "3B", QuoteCharacter: "27"}}}); err != nil {
		t.Fatalf("CSV import: %v", err)
	}
	var names string
	if err := shop.QueryRow(ctx, "SELECT string_agg(id || '=' || name || '/' || note, ',' ORDER BY id) FROM loaded").Scan(&names); err != nil ||
		names != "10=semi;colon/none,11=plain/none" {
		t.Errorf("loaded after the CSV import: %q, %v", names, err)
	}
	// A user with no grant is refused by the server; a missing table and
	// a missing object fail the operation.
	for what, req := range map[string]*alloydbpb.ImportClusterRequest{
		"a missing table":  {GcsUri: bucket + "load.csv", Database: "shop", ImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions_{CsvImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions{Table: "nope"}}},
		"a missing object": {GcsUri: bucket + "nope.sql", Database: "shop", ImportOptions: sqlImport},
		"a user with no grant on the table": {GcsUri: bucket + "load.csv", Database: "shop", User: "loader",
			ImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions_{CsvImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions{Table: "items", FieldDelimiter: "3B"}}},
	} {
		if _, err := imp(c, req); err == nil {
			t.Errorf("an import of %s succeeded", what)
		}
	}
	if _, err := c.ImportCluster(ctx, &alloydbpb.ImportClusterRequest{Name: name, GcsUri: bucket + "load.csv", Database: "shop",
		ImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions_{CsvImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions{}}}); alloyDBCode(err) != codes.InvalidArgument {
		t.Errorf("a CSV import with no table: %v, want INVALID_ARGUMENT", err)
	}
}

// alloyDBConnDB connects to database as postgres through the connector.
func alloyDBConnDB(ctx context.Context, t *testing.T, d *alloydbconn.Dialer, instance, database string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("user=postgres password=burrow-pw dbname=%s sslmode=disable", database))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx, instance) }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting to %s/%s through the connector: %v", instance, database, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// TestAlloyDBSecondaryClusters (#2191): cross-region replication through
// the official client. A secondary cluster in another region, with its
// secondary instance, replicates the primary's writes and refuses its own;
// the primary lists it; a switchover swaps the roles with nothing lost,
// and a promotion then makes the former primary standalone.
// covers: google.cloud.alloydb.v1.AlloyDBAdmin/CreateSecondaryCluster, google.cloud.alloydb.v1.AlloyDBAdmin/CreateSecondaryInstance, google.cloud.alloydb.v1.AlloyDBAdmin/PromoteCluster, google.cloud.alloydb.v1.AlloyDBAdmin/SwitchoverCluster
func TestAlloyDBSecondaryClusters(t *testing.T) {
	h := New(t)
	clients := alloyDBClients(t, h)
	c := clients["grpc"]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	pname, pinst := alloyDBCluster(ctx, t, h, c, "us-central1", "xrp", nil)
	d := alloyDBDialer(t, h)
	pg := alloyDBConn(ctx, t, d, pinst.GetName(), "postgres", "burrow-pw")
	alloyDBExec(ctx, t, pg, "CREATE TABLE orders (id int PRIMARY KEY)", "INSERT INTO orders VALUES (1), (2)")

	sloc := "projects/" + h.Project() + "/locations/us-east1"
	sid := fmt.Sprintf("xrs-%d", time.Now().UnixNano()%1e6)
	sname := sloc + "/clusters/" + sid
	network := "projects/" + h.Project() + "/global/networks/default"
	// Refusals: no primary named; a primary that does not exist; an
	// initial user.
	for what, cl := range map[string]*alloydbpb.Cluster{
		"no primary cluster": {NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}},
		"an initial user": {NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}, InitialUser: &alloydbpb.UserPassword{User: "x", Password: "y"},
			SecondaryConfig: &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: pname}},
	} {
		if _, err := c.CreateSecondaryCluster(ctx, &alloydbpb.CreateSecondaryClusterRequest{Parent: sloc, ClusterId: sid, Cluster: cl}); alloyDBCode(err) != codes.InvalidArgument {
			t.Errorf("a secondary cluster with %s: %v, want INVALID_ARGUMENT", what, err)
		}
	}
	op, err := clients["rest"].CreateSecondaryCluster(ctx, &alloydbpb.CreateSecondaryClusterRequest{Parent: sloc, ClusterId: sid, Cluster: &alloydbpb.Cluster{
		NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}, SecondaryConfig: &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: pname}}})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	// After the switchover and the promotion it is a cluster of its own,
	// which the primary's cleanup no longer reaches.
	t.Cleanup(func() { alloyDBDelete(t, clients["grpc"], sloc+"/clusters/"+sid) })
	if sc.GetClusterType() != alloydbpb.Cluster_SECONDARY || sc.GetSecondaryConfig().GetPrimaryClusterName() != pname || sc.GetDatabaseVersion() != alloydbpb.DatabaseVersion_POSTGRES_16 {
		t.Errorf("the secondary cluster: %v", sc)
	}
	if p, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: pname}); err != nil || len(p.GetPrimaryConfig().GetSecondaryClusterNames()) != 1 || p.GetPrimaryConfig().GetSecondaryClusterNames()[0] != sname {
		t.Errorf("the primary's secondary_cluster_names: %v, %v", p.GetPrimaryConfig(), err)
	}
	if _, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: sname, InstanceId: "p", Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY}}); alloyDBCode(err) != codes.InvalidArgument {
		t.Errorf("a PRIMARY instance in a secondary cluster: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: pname, Force: true}); alloyDBCode(err) != codes.FailedPrecondition {
		t.Errorf("deleting a primary cluster with a secondary: %v, want FAILED_PRECONDITION", err)
	}
	iop, err := c.CreateSecondaryInstance(ctx, &alloydbpb.CreateSecondaryInstanceRequest{Parent: sname, InstanceId: "secondary", Instance: &alloydbpb.Instance{}})
	if err != nil {
		t.Fatal(err)
	}
	sinst, err := waitLRO(ctx, iop)
	if err != nil {
		t.Fatal(err)
	}
	if sinst.GetInstanceType() != alloydbpb.Instance_SECONDARY || sinst.GetMachineConfig().GetCpuCount() != 2 || sinst.GetState() != alloydbpb.Instance_READY {
		t.Errorf("the secondary instance: %v", sinst)
	}
	// It replicates the primary, reached through the connector by its
	// instance URI, and takes no writes.
	alloyDBExec(ctx, t, pg, "INSERT INTO orders VALUES (3)")
	alloyDBExpectRows(ctx, t, d, sinst.GetName(), 3, true)
	sec := alloyDBConn(ctx, t, d, sinst.GetName(), "postgres", "burrow-pw")
	if _, err := sec.Exec(ctx, "INSERT INTO orders VALUES (99)"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a write to the secondary: %v, want a read-only refusal", err)
	}
	if _, err := c.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: sinst.GetName()}); alloyDBCode(err) != codes.FailedPrecondition {
		t.Errorf("deleting a secondary instance: %v, want FAILED_PRECONDITION", err)
	}

	// Switchover: the secondary becomes the primary, the former primary
	// its secondary, with every row.
	if _, err := c.SwitchoverCluster(ctx, &alloydbpb.SwitchoverClusterRequest{Name: pname}); alloyDBCode(err) != codes.FailedPrecondition {
		t.Errorf("a switchover of a primary cluster: %v, want FAILED_PRECONDITION", err)
	}
	alloyDBExec(ctx, t, pg, "INSERT INTO orders VALUES (4)")
	sop, err := c.SwitchoverCluster(ctx, &alloydbpb.SwitchoverClusterRequest{Name: sname})
	if err != nil {
		t.Fatal(err)
	}
	swapped, err := waitLRO(ctx, sop)
	if err != nil {
		t.Fatalf("switchover: %v", err)
	}
	if swapped.GetClusterType() != alloydbpb.Cluster_PRIMARY || len(swapped.GetPrimaryConfig().GetSecondaryClusterNames()) != 1 || swapped.GetPrimaryConfig().GetSecondaryClusterNames()[0] != pname {
		t.Errorf("the new primary cluster: %v", swapped)
	}
	former, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: pname})
	if err != nil || former.GetClusterType() != alloydbpb.Cluster_SECONDARY || former.GetSecondaryConfig().GetPrimaryClusterName() != sname {
		t.Errorf("the former primary cluster: %v, %v", former, err)
	}
	if in, err := c.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: sinst.GetName()}); err != nil || in.GetInstanceType() != alloydbpb.Instance_PRIMARY {
		t.Errorf("the promoted instance: %v, %v", in, err)
	}
	if in, err := c.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: pinst.GetName()}); err != nil || in.GetInstanceType() != alloydbpb.Instance_SECONDARY {
		t.Errorf("the former primary instance: %v, %v", in, err)
	}
	d2 := alloyDBDialer(t, h)
	newPrimary := alloyDBConn(ctx, t, d2, sinst.GetName(), "postgres", "burrow-pw")
	alloyDBExec(ctx, t, newPrimary, "INSERT INTO orders VALUES (5)")
	alloyDBExpectRows(ctx, t, d2, sinst.GetName(), 5, false)
	alloyDBExpectRows(ctx, t, d2, pinst.GetName(), 5, true)

	// Promote the former primary: standalone, writable, no longer listed.
	pop, err := c.PromoteCluster(ctx, &alloydbpb.PromoteClusterRequest{Name: pname})
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := waitLRO(ctx, pop)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted.GetClusterType() != alloydbpb.Cluster_PRIMARY || promoted.GetSecondaryConfig() != nil {
		t.Errorf("the promoted cluster: %v", promoted)
	}
	if cl, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: sname}); err != nil || len(cl.GetPrimaryConfig().GetSecondaryClusterNames()) != 0 {
		t.Errorf("after the promotion, %s's secondaries: %v, %v", sname, cl.GetPrimaryConfig(), err)
	}
	d3 := alloyDBDialer(t, h)
	alone := alloyDBConn(ctx, t, d3, pinst.GetName(), "postgres", "burrow-pw")
	alloyDBExec(ctx, t, alone, "INSERT INTO orders VALUES (6)")
	alloyDBExpectRows(ctx, t, d3, pinst.GetName(), 6, false)
	alloyDBExpectRows(ctx, t, d3, sinst.GetName(), 5, false)
	if _, err := c.PromoteCluster(ctx, &alloydbpb.PromoteClusterRequest{Name: pname}); alloyDBCode(err) != codes.FailedPrecondition {
		t.Errorf("promoting a primary cluster: %v, want FAILED_PRECONDITION", err)
	}
}

// TestAlloyDBConnectionPoolingAndAddresses (#2191): managed connection
// pooling through UpdateInstance (flags checked, pooler_count set); the
// pooler answers the connectors and, at the instance's address, port 6432
// with the user's password checked; each instance has an address of its
// own where the connector reaches it by instance URI and PostgreSQL
// answers on 5432 over SSL; plaintext and require_connectors are refused.
// covers: google.cloud.alloydb.v1.AlloyDBAdmin/UpdateInstance, google.cloud.alloydb.v1.AlloyDBAdmin/GetConnectionInfo
func TestAlloyDBConnectionPoolingAndAddresses(t *testing.T) {
	h := New(t)
	c := alloyDBClients(t, h)["grpc"]
	ctx := alloyDBWait(t)
	name, primary := alloyDBCluster(ctx, t, h, c, "us-central1", "pool", nil)
	d := alloyDBDialer(t, h)
	pg := alloyDBConn(ctx, t, d, primary.GetName(), "postgres", "burrow-pw")
	alloyDBExec(ctx, t, pg, "CREATE TABLE orders (id int)", "INSERT INTO orders VALUES (1), (2)")
	rop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "pool", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_READ_POOL, ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 2},
		MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := waitLRO(ctx, rop)
	if err != nil {
		t.Fatal(err)
	}
	if pool.GetIpAddress() == "" || pool.GetIpAddress() == primary.GetIpAddress() && alloyDBOwnAddress(pool) {
		t.Errorf("the read pool's address %q, the primary's %q", pool.GetIpAddress(), primary.GetIpAddress())
	}
	// The connector reaches the read pool by its instance URI, at its
	// own address (the default, private IP). Where neither instance has
	// one (macOS with no loopback aliases), both share the host's, and
	// the connectors, which send no server name for an address, reach the
	// primary there (docs/alloydb.md).
	if alloyDBOwnAddress(pool) || alloyDBOwnAddress(primary) {
		alloyDBExpectRows(ctx, t, d, pool.GetName(), 2, true)
	} else {
		t.Logf("the read pool and the primary share %s (no spare loopback address here): the read pool is not told apart by the connector", pool.GetIpAddress())
		alloyDBExpectRows(ctx, t, d, pool.GetName(), 2, false)
	}
	for _, in := range []*alloydbpb.Instance{primary, pool} {
		if !alloyDBOwnAddress(in) {
			t.Logf("%s shares the host's address %s (no spare loopback address here): not reached by IP", in.GetName(), in.GetIpAddress())
			continue
		}
		conn := alloyDBDirect(ctx, t, in.GetIpAddress(), 5432, "postgres", "burrow-pw", "require")
		var recovery bool
		if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&recovery); err != nil || recovery != (in.GetInstanceType() == alloydbpb.Instance_READ_POOL) {
			t.Errorf("%s by IP: in recovery %v, %v", in.GetName(), recovery, err)
		}
		if _, err := alloyDBDirectErr(ctx, in.GetIpAddress(), 5432, "postgres", "burrow-pw", "disable"); err == nil || !strings.Contains(err.Error(), "SSL") {
			t.Errorf("%s by IP without SSL: %v, want the ENCRYPTED_ONLY refusal", in.GetName(), err)
		}
	}

	// Pooling: bad flags refused; then on, with flags.
	update := func(in *alloydbpb.Instance, paths ...string) (*alloydbpb.Instance, error) {
		op, err := c.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return nil, err
		}
		return waitLRO(ctx, op)
	}
	for what, flags := range map[string]map[string]string{
		"an unknown flag":       {"no_such_flag": "1"},
		"a bad pool_mode":       {"pool_mode": "statement"},
		"max_pool_size 0":       {"max_pool_size": "0"},
		"min above max":         {"min_pool_size": "10", "max_pool_size": "5"},
		"max_client_conn above": {"max_client_connections": "262043"},
	} {
		if _, err := c.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: &alloydbpb.Instance{Name: primary.GetName(),
			ConnectionPoolConfig: &alloydbpb.Instance_ConnectionPoolConfig{Enabled: true, Flags: flags}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"connection_pool_config"}}}); alloyDBCode(err) != codes.InvalidArgument {
			t.Errorf("pooling with %s: %v, want INVALID_ARGUMENT", what, err)
		}
	}
	pooled, err := update(&alloydbpb.Instance{Name: primary.GetName(), ConnectionPoolConfig: &alloydbpb.Instance_ConnectionPoolConfig{
		Enabled: true, Flags: map[string]string{"pool_mode": "transaction", "max_pool_size": "1", "max_client_connections": "100"}}}, "connection_pool_config")
	if err != nil {
		t.Fatalf("enable pooling: %v", err)
	}
	if pc := pooled.GetConnectionPoolConfig(); !pc.GetEnabled() || pc.GetPoolerCount() != 1 || pc.GetFlags()["max_pool_size"] != "1" {
		t.Errorf("connection_pool_config: %v", pc)
	}
	// Through the connector, connections are pooled: with max_pool_size
	// 1, in transaction mode, two clients' statements run on one server
	// backend.
	d2 := alloyDBDialer(t, h)
	a := alloyDBConn(ctx, t, d2, primary.GetName(), "postgres", "burrow-pw")
	b := alloyDBConn(ctx, t, d2, primary.GetName(), "postgres", "burrow-pw")
	if pa, pb := alloyDBBackend(ctx, t, a), alloyDBBackend(ctx, t, b); pa != pb {
		t.Errorf("two clients through the connector to a pooling instance ran on backends %d and %d; want one, pooled", pa, pb)
	}
	var n int
	if err := a.QueryRow(ctx, "SELECT count(*) FROM orders", pgx.QueryExecModeSimpleProtocol).Scan(&n); err != nil || n != 2 {
		t.Errorf("through the pooler: %d rows, %v", n, err)
	}
	if alloyDBOwnAddress(primary) {
		x := alloyDBDirect(ctx, t, primary.GetIpAddress(), 6432, "postgres", "burrow-pw", "require")
		y := alloyDBDirect(ctx, t, primary.GetIpAddress(), 6432, "postgres", "burrow-pw", "require")
		if px, py := alloyDBBackend(ctx, t, x), alloyDBBackend(ctx, t, y); px != py {
			t.Errorf("two clients on port 6432 ran on backends %d and %d; want one, pooled", px, py)
		}
		if _, err := alloyDBDirectErr(ctx, primary.GetIpAddress(), 6432, "postgres", "wrong-pw", "require"); err == nil {
			t.Error("the pooler took a wrong password")
		}
		if _, err := alloyDBDirectErr(ctx, pool.GetIpAddress(), 6432, "postgres", "burrow-pw", "require"); err == nil && alloyDBOwnAddress(pool) {
			t.Error("port 6432 of a read pool that does not pool answered")
		}
	}
	// require_connectors refuses direct connections.
	if alloyDBOwnAddress(pool) {
		if _, err := update(&alloydbpb.Instance{Name: pool.GetName(), ClientConnectionConfig: &alloydbpb.Instance_ClientConnectionConfig{RequireConnectors: true,
			SslConfig: &alloydbpb.SslConfig{SslMode: alloydbpb.SslConfig_ENCRYPTED_ONLY}}}, "client_connection_config"); err != nil {
			t.Fatal(err)
		}
		if _, err := alloyDBDirectErr(ctx, pool.GetIpAddress(), 5432, "postgres", "burrow-pw", "require"); err == nil || !strings.Contains(err.Error(), "connectors") {
			t.Errorf("by IP with require_connectors: %v, want a refusal", err)
		}
		alloyDBExpectRows(ctx, t, d2, pool.GetName(), 2, true)
	}
	// Off again: each client has a backend of its own.
	if off, err := update(&alloydbpb.Instance{Name: primary.GetName(), ConnectionPoolConfig: &alloydbpb.Instance_ConnectionPoolConfig{}}, "connection_pool_config"); err != nil || off.GetConnectionPoolConfig().GetEnabled() {
		t.Errorf("disable pooling: %v, %v", off.GetConnectionPoolConfig(), err)
	}
	d3 := alloyDBDialer(t, h)
	a = alloyDBConn(ctx, t, d3, primary.GetName(), "postgres", "burrow-pw")
	b = alloyDBConn(ctx, t, d3, primary.GetName(), "postgres", "burrow-pw")
	if pa, pb := alloyDBBackend(ctx, t, a), alloyDBBackend(ctx, t, b); pa == pb {
		t.Errorf("with pooling off, two clients ran on one backend, %d", pa)
	}
}

// alloyDBBackend is the server backend's process ID conn's statement runs
// on (the simple protocol, which transaction pooling serves).
func alloyDBBackend(ctx context.Context, t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()", pgx.QueryExecModeSimpleProtocol).Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

// alloyDBOwnAddress is whether in has an address of its own rather than
// the host's shared one (127.0.0.1): every instance on Linux.
func alloyDBOwnAddress(in *alloydbpb.Instance) bool {
	return in.GetIpAddress() != "" && in.GetIpAddress() != "127.0.0.1"
}

func alloyDBDirectErr(ctx context.Context, ip string, port int, user, password, sslmode string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=postgres sslmode=%s connect_timeout=10", ip, port, user, password, sslmode))
	if err != nil {
		return nil, err
	}
	if cfg.TLSConfig != nil {
		cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}
	cfg.Fallbacks = nil
	return pgx.ConnectConfig(ctx, cfg)
}

// alloyDBDirect connects to ip:port, not through a connector.
func alloyDBDirect(ctx context.Context, t *testing.T, ip string, port int, user, password, sslmode string) *pgx.Conn {
	t.Helper()
	conn, err := alloyDBDirectErr(ctx, ip, port, user, password, sslmode)
	if err != nil {
		t.Fatalf("connecting to %s port %d: %v", ip, port, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}
