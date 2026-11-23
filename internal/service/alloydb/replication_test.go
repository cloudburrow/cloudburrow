package alloydb

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	connectorspb "cloud.google.com/go/alloydb/connectors/apiv1alpha/connectorspb"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// fakeTransfers records each export and import; an export writes a line
// naming what it dumped.
type fakeTransfers struct {
	mu    sync.Mutex
	calls []string
	read  string
}

func (f *fakeTransfers) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeTransfers) ExportSQL(_ context.Context, server string, x SQLExport, w io.Writer) error {
	f.record(fmt.Sprintf("sql-export %s %s %v %v %v %v", server, x.Database, x.Tables, x.SchemaOnly, x.Clean, x.IfExists))
	_, err := fmt.Fprintf(w, "-- dump of %s\n", x.Database)
	return err
}

func (f *fakeTransfers) ExportCSV(_ context.Context, server, database, query string, c CSVFormat, w io.Writer) error {
	f.record(fmt.Sprintf("csv-export %s %s %q %q%q%q", server, database, query, c.Delimiter, c.Quote, c.Escape))
	_, err := io.WriteString(w, "1"+c.Delimiter+"a\n")
	return err
}

func (f *fakeTransfers) ImportSQL(_ context.Context, server, database, user string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	f.read = string(b)
	f.mu.Unlock()
	f.record(fmt.Sprintf("sql-import %s %s %s", server, database, user))
	return nil
}

func (f *fakeTransfers) ImportCSV(_ context.Context, server, database, table string, columns []string, c CSVFormat, user string, r io.Reader) error {
	if table == "missing" {
		return fmt.Errorf(`relation "missing" does not exist`)
	}
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	f.read = string(b)
	f.mu.Unlock()
	f.record(fmt.Sprintf("csv-import %s %s %s %v %q %s", server, database, table, columns, c.Delimiter, user))
	return nil
}

func (f *fakeTransfers) saw(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type fakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (o *fakeObjects) Read(_ context.Context, bucket, object string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.objects[bucket+"/"+object]
	if !ok {
		return nil, apierror.NotFound("gs://%s/%s does not exist", bucket, object)
	}
	return b, nil
}

func (o *fakeObjects) Write(_ context.Context, bucket, object string, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objects[bucket+"/"+object] = append([]byte(nil), data...)
	return nil
}

// opFailed waits for op and returns its error's code.
func opFailed(t *testing.T, s *Server, name string) codes.Code {
	t.Helper()
	s.Wait()
	got, err := s.Operation(name)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetDone() {
		t.Fatalf("operation %s is not done", name)
	}
	return codes.Code(got.GetError().GetCode())
}

func TestExportAndImport(t *testing.T) {
	ctx := context.Background()
	// Without Cloud Storage, both are refused.
	bare := newFixture(t, Options{})
	bare.cluster(t, false)
	bare.primary(t)
	if _, err := bare.s.ExportCluster(ctx, &alloydbpb.ExportClusterRequest{Name: cluster, Database: "postgres",
		Destination:   &alloydbpb.ExportClusterRequest_GcsDestination{GcsDestination: &alloydbpb.GcsDestination{Uri: "gs://b/o.sql"}},
		ExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: &alloydbpb.ExportClusterRequest_SqlExportOptions{}}}); code(err) != codes.FailedPrecondition {
		t.Errorf("an export with no Cloud Storage: %v, want FAILED_PRECONDITION", err)
	}

	tr := &fakeTransfers{}
	objs := &fakeObjects{objects: map[string][]byte{}}
	f := newFixture(t, Options{Transfers: tr, Objects: objs})
	f.cluster(t, false)
	gcs := func(uri string) *alloydbpb.ExportClusterRequest_GcsDestination {
		return &alloydbpb.ExportClusterRequest_GcsDestination{GcsDestination: &alloydbpb.GcsDestination{Uri: uri}}
	}
	sqlOpts := func(o *alloydbpb.ExportClusterRequest_SqlExportOptions) *alloydbpb.ExportClusterRequest_SqlExportOptions_ {
		return &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: o}
	}
	// A cluster with no primary has nothing to export.
	if _, err := f.s.ExportCluster(ctx, &alloydbpb.ExportClusterRequest{Name: cluster, Database: "postgres", Destination: gcs("gs://b/o.sql"),
		ExportOptions: sqlOpts(&alloydbpb.ExportClusterRequest_SqlExportOptions{})}); err == nil {
		t.Error("an export of a cluster with no primary was accepted")
	}
	f.primary(t)

	for name, req := range map[string]*alloydbpb.ExportClusterRequest{
		"no database":       {Name: cluster, Destination: gcs("gs://b/o.sql"), ExportOptions: sqlOpts(&alloydbpb.ExportClusterRequest_SqlExportOptions{})},
		"no destination":    {Name: cluster, Database: "postgres", ExportOptions: sqlOpts(&alloydbpb.ExportClusterRequest_SqlExportOptions{})},
		"a bucket, no file": {Name: cluster, Database: "postgres", Destination: gcs("gs://b/"), ExportOptions: sqlOpts(&alloydbpb.ExportClusterRequest_SqlExportOptions{})},
		"not gs://":         {Name: cluster, Database: "postgres", Destination: gcs("s3://b/o"), ExportOptions: sqlOpts(&alloydbpb.ExportClusterRequest_SqlExportOptions{})},
		"no format":         {Name: cluster, Database: "postgres", Destination: gcs("gs://b/o.sql")},
		"if_exist alone": {Name: cluster, Database: "postgres", Destination: gcs("gs://b/o.sql"),
			ExportOptions: sqlOpts(&alloydbpb.ExportClusterRequest_SqlExportOptions{IfExistTargetObjects: boolp(true)})},
		"CSV with no query": {Name: cluster, Database: "postgres", Destination: gcs("gs://b/o.csv"),
			ExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{}}},
		"a two-character delimiter": {Name: cluster, Database: "postgres", Destination: gcs("gs://b/o.csv"),
			ExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{
				SelectQuery: "SELECT 1", FieldDelimiter: "2C2C"}}},
	} {
		if _, err := f.s.ExportCluster(ctx, req); code(err) != codes.InvalidArgument {
			t.Errorf("an export with %s: %v, want INVALID_ARGUMENT", name, err)
		}
	}

	// SQL, with its options, and compressed when the name ends in .gz.
	op, err := f.s.ExportCluster(ctx, &alloydbpb.ExportClusterRequest{Name: cluster, Database: "shop", Destination: gcs("gs://b/dump.sql.gz"),
		ExportOptions: sqlOpts(&alloydbpb.ExportClusterRequest_SqlExportOptions{Tables: []string{"orders"}, SchemaOnly: boolp(true),
			CleanTargetObjects: boolp(true), IfExistTargetObjects: boolp(true)})})
	got := wait(t, f.s, op, err)
	var resp alloydbpb.ExportClusterResponse
	if err := got.GetResponse().UnmarshalTo(&resp); err != nil || resp.GetGcsDestination().GetUri() != "gs://b/dump.sql.gz" {
		t.Errorf("export response = %v, %v", &resp, err)
	}
	if !tr.saw("sql-export " + ServerOf(cluster+"/instances/primary") + " shop [orders] true true true") {
		t.Errorf("the SQL export ran as %v", tr.calls)
	}
	zr, err := gzip.NewReader(bytes.NewReader(objs.objects["b/dump.sql.gz"]))
	if err != nil {
		t.Fatalf("the .gz export is not gzip: %v", err)
	}
	if b, _ := io.ReadAll(zr); string(b) != "-- dump of shop\n" {
		t.Errorf("the .gz export holds %q", b)
	}

	// CSV, with hex characters, with and without 0x.
	op, err = f.s.ExportCluster(ctx, &alloydbpb.ExportClusterRequest{Name: cluster, Database: "shop", Destination: gcs("gs://b/rows.csv"),
		ExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{
			SelectQuery: "SELECT * FROM orders", FieldDelimiter: "0x7C", QuoteCharacter: "27"}}})
	wait(t, f.s, op, err)
	if !tr.saw(`csv-export ` + ServerOf(cluster+"/instances/primary") + ` shop "SELECT * FROM orders" "|""'""'"`) {
		t.Errorf("the CSV export ran as %v", tr.calls)
	}
	if string(objs.objects["b/rows.csv"]) != "1|a\n" {
		t.Errorf("rows.csv = %q", objs.objects["b/rows.csv"])
	}

	// Imports: SQL defaults to postgres; CSV needs a database and a table.
	csvImport := func(table, db string) *alloydbpb.ImportClusterRequest {
		return &alloydbpb.ImportClusterRequest{Name: cluster, GcsUri: "gs://b/rows.csv", Database: db,
			ImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions_{CsvImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions{
				Table: table, Columns: []string{"id", "name"}, FieldDelimiter: "7C"}}}
	}
	for name, req := range map[string]*alloydbpb.ImportClusterRequest{
		"no URI":         {Name: cluster, ImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions_{SqlImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions{}}},
		"no format":      {Name: cluster, GcsUri: "gs://b/dump.sql"},
		"CSV, no table":  csvImport("", "shop"),
		"CSV, no db":     csvImport("orders", ""),
		"a bad cluster":  {Name: "clusters/c1", GcsUri: "gs://b/dump.sql"},
		"a folder's URI": {Name: cluster, GcsUri: "gs://b/dir/", ImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions_{}},
	} {
		if _, err := f.s.ImportCluster(ctx, req); code(err) != codes.InvalidArgument {
			t.Errorf("an import with %s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	op, err = f.s.ImportCluster(ctx, &alloydbpb.ImportClusterRequest{Name: cluster, GcsUri: "gs://b/dump.sql.gz", User: "alice",
		ImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions_{SqlImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions{}}})
	got = wait(t, f.s, op, err)
	var iresp alloydbpb.ImportClusterResponse
	if err := got.GetResponse().UnmarshalTo(&iresp); err != nil || iresp.GetBytesDownloaded() != int64(len(objs.objects["b/dump.sql.gz"])) {
		t.Errorf("import response = %v, %v", &iresp, err)
	}
	if !tr.saw("sql-import "+ServerOf(cluster+"/instances/primary")+" postgres alice") || tr.read != "-- dump of shop\n" {
		t.Errorf("the SQL import ran as %v, read %q", tr.calls, tr.read)
	}
	op, err = f.s.ImportCluster(ctx, csvImport("orders", "shop"))
	wait(t, f.s, op, err)
	if !tr.saw(`csv-import ` + ServerOf(cluster+"/instances/primary") + ` shop orders [id name] "|"`) {
		t.Errorf("the CSV import ran as %v", tr.calls)
	}

	// A missing object and a missing table fail the operation.
	op, err = f.s.ImportCluster(ctx, &alloydbpb.ImportClusterRequest{Name: cluster, GcsUri: "gs://b/none.sql",
		ImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions_{SqlImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions{}}})
	if err != nil {
		t.Fatal(err)
	}
	if c := opFailed(t, f.s, op.GetName()); c != codes.NotFound {
		t.Errorf("an import of a missing object: %v, want NOT_FOUND", c)
	}
	op, err = f.s.ImportCluster(ctx, csvImport("missing", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	if c := opFailed(t, f.s, op.GetName()); c != codes.InvalidArgument {
		t.Errorf("an import into a missing table: %v, want INVALID_ARGUMENT", c)
	}
}

func boolp(b bool) *bool { return &b }

const secondary = "projects/p1/locations/us-east1/clusters/s1"

func (f *fixture) secondary(t *testing.T) {
	t.Helper()
	op, err := f.s.CreateSecondaryCluster(context.Background(), &alloydbpb.CreateSecondaryClusterRequest{Parent: "projects/p1/locations/us-east1",
		ClusterId: "s1", Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
			SecondaryConfig: &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: cluster}}})
	wait(t, f.s, op, err)
	op, err = f.s.CreateSecondaryInstance(context.Background(), &alloydbpb.CreateSecondaryInstanceRequest{Parent: secondary, InstanceId: "replica",
		Instance: &alloydbpb.Instance{}})
	wait(t, f.s, op, err)
}

func TestSecondaryClusters(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.cluster(t, false)

	newSecondary := func(id string, c *alloydbpb.Cluster) error {
		if c.NetworkConfig == nil {
			c.NetworkConfig = &alloydbpb.Cluster_NetworkConfig{Network: network}
		}
		_, err := f.s.CreateSecondaryCluster(ctx, &alloydbpb.CreateSecondaryClusterRequest{Parent: "projects/p1/locations/us-east1", ClusterId: id, Cluster: c})
		f.s.Wait()
		return err
	}
	f.primary(t)
	for name, c := range map[string]*alloydbpb.Cluster{
		"no secondary_config": {},
		"a missing primary":   {SecondaryConfig: &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: loc + "/clusters/none"}},
		"an initial user": {SecondaryConfig: &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: cluster},
			InitialUser: &alloydbpb.UserPassword{User: "u", Password: "p"}},
	} {
		if err := newSecondary("sx", c); err == nil {
			t.Errorf("a secondary cluster with %s was accepted", name)
		}
	}

	f.secondary(t)
	s, err := f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: secondary})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetClusterType() != alloydbpb.Cluster_SECONDARY || s.GetSecondaryConfig().GetPrimaryClusterName() != cluster {
		t.Errorf("secondary = %v %v", s.GetClusterType(), s.GetSecondaryConfig())
	}
	p, _ := f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: cluster})
	if names := p.GetPrimaryConfig().GetSecondaryClusterNames(); len(names) != 1 || names[0] != secondary {
		t.Errorf("the primary's secondaries = %v", names)
	}
	replica := ServerOf(secondary + "/instances/replica")
	f.eng.mu.Lock()
	spec := f.eng.running[replica]
	f.eng.mu.Unlock()
	if spec.ReplicaOf != ServerOf(cluster+"/instances/primary") {
		t.Errorf("the secondary instance replicates %q, want the primary's server", spec.ReplicaOf)
	}

	// A secondary cluster has one secondary instance, takes no primary,
	// and its instance is not deleted alone; the primary is not deleted
	// while it has secondaries.
	if _, err := f.s.CreateSecondaryInstance(ctx, &alloydbpb.CreateSecondaryInstanceRequest{Parent: secondary, InstanceId: "two", Instance: &alloydbpb.Instance{}}); err == nil {
		t.Error("a second secondary instance was accepted")
	}
	if _, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: secondary, InstanceId: "p",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}}); code(err) != codes.InvalidArgument {
		t.Errorf("a primary instance in a secondary cluster: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := f.s.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: secondary + "/instances/replica"}); code(err) != codes.FailedPrecondition {
		t.Errorf("deleting the secondary instance: %v, want FAILED_PRECONDITION", err)
	}
	if _, err := f.s.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: cluster, Force: true}); code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a primary with secondaries: %v, want FAILED_PRECONDITION", err)
	}
	if _, err := f.s.PromoteCluster(ctx, &alloydbpb.PromoteClusterRequest{Name: cluster}); err == nil {
		t.Error("promoting a primary cluster was accepted")
	}
	if _, err := f.s.SwitchoverCluster(ctx, &alloydbpb.SwitchoverClusterRequest{Name: cluster}); err == nil {
		t.Error("switching over a primary cluster was accepted")
	}

	// Switchover: the secondary becomes the primary, and the old primary
	// its secondary, replicating it.
	op, err := f.s.SwitchoverCluster(ctx, &alloydbpb.SwitchoverClusterRequest{Name: secondary})
	wait(t, f.s, op, err)
	if !f.eng.called("promote " + replica) {
		t.Errorf("the switchover did not promote %s: %v", replica, f.eng.calls)
	}
	s, _ = f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: secondary})
	p, _ = f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: cluster})
	if s.GetClusterType() != alloydbpb.Cluster_PRIMARY || p.GetClusterType() != alloydbpb.Cluster_SECONDARY ||
		p.GetSecondaryConfig().GetPrimaryClusterName() != secondary {
		t.Errorf("after the switchover: %s is %v, %s is %v of %q", secondary, s.GetClusterType(), cluster, p.GetClusterType(),
			p.GetSecondaryConfig().GetPrimaryClusterName())
	}
	old, _ := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/primary"})
	if old.GetInstanceType() != alloydbpb.Instance_SECONDARY {
		t.Errorf("the old primary instance is %v, want SECONDARY", old.GetInstanceType())
	}
	f.eng.mu.Lock()
	spec = f.eng.running[ServerOf(cluster+"/instances/primary")]
	f.eng.mu.Unlock()
	if spec.ReplicaOf != replica || spec.Epoch == 0 {
		t.Errorf("the old primary replicates %q (epoch %d), want %s, rebuilt", spec.ReplicaOf, spec.Epoch, replica)
	}

	// Promote: the old primary, now a secondary, becomes standalone.
	op, err = f.s.PromoteCluster(ctx, &alloydbpb.PromoteClusterRequest{Name: cluster})
	wait(t, f.s, op, err)
	p, _ = f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: cluster})
	s, _ = f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: secondary})
	if p.GetClusterType() != alloydbpb.Cluster_PRIMARY || len(s.GetPrimaryConfig().GetSecondaryClusterNames()) != 0 {
		t.Errorf("after the promotion: %s is %v; %s's secondaries are %v", cluster, p.GetClusterType(), secondary,
			s.GetPrimaryConfig().GetSecondaryClusterNames())
	}
	if in, _ := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/primary"}); in.GetInstanceType() != alloydbpb.Instance_PRIMARY {
		t.Errorf("the promoted instance is %v, want PRIMARY", in.GetInstanceType())
	}
	f.eng.mu.Lock()
	spec = f.eng.running[ServerOf(cluster+"/instances/primary")]
	f.eng.mu.Unlock()
	if spec.ReplicaOf != "" {
		t.Errorf("the promoted server still replicates %q", spec.ReplicaOf)
	}
}

func TestConnectionPooling(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.cluster(t, false)
	flags, err := f.s.ListSupportedDatabaseFlags(ctx, &alloydbpb.ListSupportedDatabaseFlagsRequest{Parent: loc,
		Scope: alloydbpb.SupportedDatabaseFlag_CONNECTION_POOL})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*alloydbpb.SupportedDatabaseFlag{}
	for _, fl := range flags.GetSupportedDatabaseFlags() {
		byName[fl.GetFlagName()] = fl
	}
	if len(byName) != len(poolFlags) || byName["max_pool_size"].GetIntegerRestrictions().GetMaxValue().GetValue() != 262042 ||
		byName["pool_mode"].GetScope() != alloydbpb.SupportedDatabaseFlag_CONNECTION_POOL {
		t.Errorf("the CONNECTION_POOL flags = %v", flags.GetSupportedDatabaseFlags())
	}
	for name, pc := range map[string]*alloydbpb.Instance_ConnectionPoolConfig{
		"an unknown flag":      {Enabled: true, Flags: map[string]string{"auth_type": "trust"}},
		"a bad pool_mode":      {Enabled: true, Flags: map[string]string{"pool_mode": "statement"}},
		"a size of 0":          {Enabled: true, Flags: map[string]string{"max_pool_size": "0"}},
		"too many clients":     {Enabled: true, Flags: map[string]string{"max_client_connections": "262043"}},
		"min above max":        {Enabled: true, Flags: map[string]string{"min_pool_size": "10", "max_pool_size": "5"}},
		"a bad stats user":     {Enabled: true, Flags: map[string]string{"stats_users": "a;b"}},
		"a non-numeric number": {Enabled: true, Flags: map[string]string{"query_wait_timeout": "soon"}},
	} {
		_, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "primary",
			Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 8},
				ConnectionPoolConfig: pc}})
		if code(err) != codes.InvalidArgument {
			t.Errorf("pooling with %s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	op, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "primary",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 8},
			ConnectionPoolConfig: &alloydbpb.Instance_ConnectionPoolConfig{Enabled: true,
				Flags: map[string]string{"pool_mode": "session", "max_pool_size": "7", "stats_users": "alice,bob"}}}})
	wait(t, f.s, op, err)
	in, _ := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/primary"})
	if in.GetConnectionPoolConfig().GetPoolerCount() != 2 {
		t.Errorf("pooler_count = %d, want 2 for 8 vCPUs", in.GetConnectionPoolConfig().GetPoolerCount())
	}
	server := ServerOf(cluster + "/instances/primary")
	f.eng.mu.Lock()
	pooler := f.eng.running[server].Pooler
	f.eng.mu.Unlock()
	for k, v := range map[string]string{"pool_mode": "session", "default_pool_size": "7", "stats_users": "alice,bob",
		"max_client_conn": "5000", "server_lifetime": "3600"} {
		if pooler[k] != v {
			t.Errorf("the pooler's %s = %q, want %q (all: %v)", k, pooler[k], v, pooler)
		}
	}

	// Turning it off restarts the server without a pooler.
	op, err = f.s.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: &alloydbpb.Instance{Name: cluster + "/instances/primary",
		ConnectionPoolConfig: &alloydbpb.Instance_ConnectionPoolConfig{Enabled: false}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"connection_pool_config"}}})
	wait(t, f.s, op, err)
	f.eng.mu.Lock()
	pooler = f.eng.running[server].Pooler
	f.eng.mu.Unlock()
	if pooler != nil {
		t.Errorf("pooling off, the server still has a pooler: %v", pooler)
	}
	in, _ = f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/primary"})
	if in.GetConnectionPoolConfig().GetPoolerCount() != 0 {
		t.Errorf("pooling off, pooler_count = %d", in.GetConnectionPoolConfig().GetPoolerCount())
	}
}

// freePort is a port free on ip.
func freePort(t *testing.T, ip string) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("%s does not bind here: %v", ip, err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// direct connects with pgx to ip:port, as a client of an instance's
// address, and runs SELECT 1.
func direct(ip string, port int, sslmode string) error {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=%s", net.JoinHostPort(ip, strconv.Itoa(port)), sslmode))
	if err != nil {
		return err
	}
	if cfg.TLSConfig != nil {
		cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	var one int
	return c.QueryRow(ctx, "SELECT 1").Scan(&one)
}

func TestInstanceAddresses(t *testing.T) {
	ctx := context.Background()
	ip := "::1"
	ports := &ListenPorts{Connector: freePort(t, ip), Direct: freePort(t, ip), Pooler: freePort(t, ip)}
	f := newFixture(t, Options{InstanceAddresses: []string{ip}, Ports: ports})
	conn, err := f.s.ServeConnector("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	f.cluster(t, false)
	op, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "primary",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2},
			ConnectionPoolConfig: &alloydbpb.Instance_ConnectionPoolConfig{Enabled: true}}})
	wait(t, f.s, op, err)
	// The only candidate is taken: the read pool shares the connectors'
	// address.
	op, err = f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "pool",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_READ_POOL, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2},
			ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 1}}})
	wait(t, f.s, op, err)
	in, _ := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/primary"})
	pool, _ := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/pool"})
	if in.GetIpAddress() != ip || pool.GetIpAddress() != "127.0.0.1" {
		t.Fatalf("addresses: primary %q, read pool %q; want %s and 127.0.0.1", in.GetIpAddress(), pool.GetIpAddress(), ip)
	}

	// The connectors reach each at its own address, the read pool by its
	// address too (the shared one names one instance per client cluster
	// only when it is the only one there).
	cert, caPool := clientCert(t, f.s, true)
	mdx := &connectorspb.MetadataExchangeRequest{AuthType: connectorspb.MetadataExchangeRequest_DB_NATIVE}
	for _, addr := range []string{net.JoinHostPort(ip, strconv.Itoa(ports.Connector)), conn.Addr()} {
		c, resp := dialConnector(t, addr, "", cert, caPool, mdx)
		if resp.GetResponseCode() != connectorspb.MetadataExchangeResponse_OK {
			t.Fatalf("the connector at %s: %v", addr, resp)
		}
		query(t, c)
	}

	// Port 5432 needs TLS by default (ENCRYPTED_ONLY); port 6432 is the
	// pooler, which the fake engine serves on its own fake server.
	if err := direct(ip, ports.Direct, "disable"); err == nil || !strings.Contains(err.Error(), "requires SSL") {
		t.Errorf("a plaintext connection to 5432: %v, want refused", err)
	}
	if err := direct(ip, ports.Direct, "require"); err != nil {
		t.Errorf("a TLS connection to 5432: %v", err)
	}
	before := len(f.eng.pool.queries)
	if err := direct(ip, ports.Pooler, "require"); err != nil {
		t.Errorf("a TLS connection to the pooler: %v", err)
	}
	if len(f.eng.pool.queries) == before {
		t.Error("the connection to port 6432 did not reach the pooler")
	}

	// require_connectors refuses direct connections.
	op, err = f.s.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: &alloydbpb.Instance{Name: cluster + "/instances/primary",
		ClientConnectionConfig: &alloydbpb.Instance_ClientConnectionConfig{RequireConnectors: true}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"client_connection_config.require_connectors"}}})
	wait(t, f.s, op, err)
	if err := direct(ip, ports.Direct, "require"); err == nil || !strings.Contains(err.Error(), "require_connectors") {
		t.Errorf("a direct connection with require_connectors: %v, want refused", err)
	}

	// Deleting the instance frees its address for the next one.
	op, err = f.s.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: cluster + "/instances/pool"})
	wait(t, f.s, op, err)
	op, err = f.s.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: cluster + "/instances/primary"})
	wait(t, f.s, op, err)
	if l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(ports.Connector))); err != nil {
		t.Errorf("the deleted instance's address still listens: %v", err)
	} else {
		l.Close()
	}
	op, err = f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "again",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	wait(t, f.s, op, err)
	if again, _ := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/again"}); again.GetIpAddress() != ip {
		t.Errorf("the next instance's address = %q, want the freed %s", again.GetIpAddress(), ip)
	}
}
