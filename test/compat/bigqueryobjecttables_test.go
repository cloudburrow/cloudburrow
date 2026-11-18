//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/api/iterator"
)

// TestBigQueryObjectTables (#1783): BigQuery object tables over the
// instance's Cloud Storage, through the official cloud.google.com/go
// bigquery, storage and bigquery/connection/apiv1 clients:
// CREATE EXTERNAL TABLE … WITH CONNECTION … OPTIONS(object_metadata =
// 'SIMPLE', uris = [...]) and tables.insert of an objectMetadata
// externalDataConfiguration with maxStaleness and metadataCacheMode
// MANUAL; the fixed schema (uri, generation, content_type, size,
// md5_hash, updated, metadata, ref as an ObjectRef) read back by a query;
// the connection's service account refused (403 naming
// storage.objects.list) until it holds Storage Object Viewer on the
// bucket; a MANUAL cache read until CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE,
// a table without a cache read live; tables.get as type EXTERNAL; and the
// documented refusals (object_metadata other than SIMPLE, a staleness
// without a cache mode or outside 30 minutes to 7 days, DML into the
// read-only table, the data pseudocolumn in a query, a refresh of a
// table without a MANUAL cache).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.insert, bigquery.tables.get, bigquery.tables.list
func TestBigQueryObjectTables(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_obj"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	sa := cn.GetCloudResource().GetServiceAccountId()
	conn := project + ".us." + id

	ds := "compat_obj_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })

	bh := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-objects")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	put := func(name, ct, data string, md map[string]string) {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType, w.Metadata = ct, md
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	put("img/a.txt", "text/plain", "hello", map[string]string{"k": "v"})
	put("img/b.png", "image/png", "\x89PNG....", nil)
	put("other/c.txt", "text/plain", "skip", nil)
	uri := "gs://" + bh.BucketName() + "/img/*"

	count := func(table string) (int64, error) {
		it, err := c.Query(fmt.Sprintf("SELECT COUNT(*) FROM `%s.%s`", ds, table)).Read(ctx)
		if err != nil {
			return 0, err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return 0, err
		}
		return row[0].(int64), nil
	}

	// Without bigquery.connections.use the CREATE is refused.
	create := fmt.Sprintf("CREATE EXTERNAL TABLE `%s.live` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['%s'])", ds, conn, uri)
	if err := mlRun(h, c, create); err == nil || !strings.Contains(err.Error(), "bigquery.connections.use") {
		t.Fatalf("CREATE without connections.use: %v", err)
	}
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: []string{callerMember(project)}}}}}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
	if err := mlRun(h, c, create); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE: %v", err)
	}
	// The service account cannot list the bucket yet.
	if _, err := count("live"); err == nil || !strings.Contains(err.Error(), "storage.objects.list") || !strings.Contains(err.Error(), sa) {
		t.Fatalf("query before the bucket grant: %v", err)
	}
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("bucket IAM Policy: %v", err)
	}
	pol.Add("serviceAccount:"+sa, iam.RoleName("roles/storage.objectViewer"))
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("bucket IAM SetPolicy: %v", err)
	}
	if n, err := count("live"); err != nil || n != 2 {
		t.Fatalf("rows after the grant: %d, %v", n, err)
	}

	attrs, err := bh.Object("img/a.txt").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	it, err := c.Query(fmt.Sprintf("SELECT uri, generation, content_type, size, md5_hash, updated IS NOT NULL, metadata[SAFE_OFFSET(0)].name, "+
		"metadata[SAFE_OFFSET(0)].value, ref.uri, ref.version, ref.authorizer, TO_JSON_STRING(ref.details) "+
		"FROM `%s.live` WHERE uri LIKE '%%a.txt'", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("query the schema: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatalf("row: %v", err)
	}
	want := []bigquery.Value{"gs://" + bh.BucketName() + "/img/a.txt", attrs.Generation, "text/plain", int64(5),
		attrs.MD5 != nil, true, "k", "v", "gs://" + bh.BucketName() + "/img/a.txt", fmt.Sprint(attrs.Generation), conn}
	row[4] = row[4] != nil && row[4].(string) != ""
	// ref.details holds gcs_metadata; the engine keeps a JSON field of a
	// STRUCT as JSON text (docs/compatibility.md), so it is read as text.
	if d := fmt.Sprint(row[11]); !strings.Contains(d, "gcs_metadata") || !strings.Contains(d, "text/plain") {
		t.Errorf("ref.details %s", d)
	}
	for i := range want {
		if fmt.Sprint(row[i]) != fmt.Sprint(want[i]) {
			t.Errorf("column %d: %v, want %v (row %v)", i, row[i], want[i], row)
		}
	}
	if _, err := c.Query(fmt.Sprintf("SELECT data FROM `%s.live`", ds)).Read(ctx); err == nil {
		t.Errorf("a query of the data pseudocolumn ran")
	}

	md, err := c.Dataset(ds).Table("live").Metadata(ctx)
	if err != nil {
		t.Fatalf("tables.get: %v", err)
	}
	if md.Type != bigquery.ExternalTable || md.ExternalDataConfig == nil || md.ExternalDataConfig.ObjectMetadata != bigquery.ObjectMetadataSimple ||
		len(md.ExternalDataConfig.SourceURIs) != 1 || md.ExternalDataConfig.SourceURIs[0] != uri || md.ExternalDataConfig.ConnectionID != conn {
		t.Errorf("tables.get: type %v, external %+v", md.Type, md.ExternalDataConfig)
	}
	var names []string
	for _, f := range md.Schema {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "uri,generation,content_type,size,md5_hash,updated,metadata,ref" {
		t.Errorf("schema %v", names)
	}

	// tables.insert: a MANUAL cache of 4 hours.
	if err := c.Dataset(ds).Table("cached").Create(ctx, &bigquery.TableMetadata{
		ExternalDataConfig: &bigquery.ExternalDataConfig{SourceURIs: []string{uri}, ObjectMetadata: bigquery.ObjectMetadataSimple,
			ConnectionID: conn, MetadataCacheMode: bigquery.Manual},
		MaxStaleness: &bigquery.IntervalValue{Hours: 4}}); err != nil {
		t.Fatalf("tables.insert: %v", err)
	}
	cmd, err := c.Dataset(ds).Table("cached").Metadata(ctx)
	if err != nil || cmd.ExternalDataConfig == nil || cmd.ExternalDataConfig.MetadataCacheMode != bigquery.Manual || cmd.MaxStaleness == nil ||
		cmd.MaxStaleness.Hours != 4 {
		t.Errorf("tables.get of the cached table: %+v, %v", cmd, err)
	}
	if n, err := count("cached"); err != nil || n != 2 {
		t.Fatalf("cached rows: %d, %v", n, err)
	}
	put("img/d.txt", "text/plain", "new", nil)
	if n, err := count("cached"); err != nil || n != 2 {
		t.Errorf("cached rows after a new object, before the refresh: %d, %v", n, err)
	}
	if n, err := count("live"); err != nil || n != 3 {
		t.Errorf("live rows after a new object: %d, %v", n, err)
	}
	if err := mlRun(h, c, fmt.Sprintf("CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE('%s.cached')", ds)); err != nil {
		t.Fatalf("BQ.REFRESH_EXTERNAL_METADATA_CACHE: %v", err)
	}
	if n, err := count("cached"); err != nil || n != 3 {
		t.Errorf("cached rows after the refresh: %d, %v", n, err)
	}
	if err := mlRun(h, c, fmt.Sprintf("CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE('%s.live')", ds)); err == nil {
		t.Errorf("a refresh of a table without a MANUAL cache ran")
	}

	lit := c.Dataset(ds).Tables(ctx)
	for {
		tb, err := lit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("tables.list: %v", err)
		}
		if tb.TableID == "live" {
			if lmd, err := tb.Metadata(ctx); err != nil || lmd.Type != bigquery.ExternalTable {
				t.Errorf("listed table: %v, %v", lmd, err)
			}
		}
	}

	for name, sql := range map[string]string{
		"DIRECTORY": fmt.Sprintf("CREATE EXTERNAL TABLE `%s.bad1` WITH CONNECTION `%s` OPTIONS(object_metadata = 'DIRECTORY', uris = ['%s'])", ds, conn, uri),
		"no mode": fmt.Sprintf("CREATE EXTERNAL TABLE `%s.bad2` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['%s'], "+
			"max_staleness = INTERVAL 1 HOUR)", ds, conn, uri),
		"10 minutes": fmt.Sprintf("CREATE EXTERNAL TABLE `%s.bad3` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['%s'], "+
			"max_staleness = INTERVAL 10 MINUTE, metadata_cache_mode = 'AUTOMATIC')", ds, conn, uri),
		"no connection": fmt.Sprintf("CREATE EXTERNAL TABLE `%s.bad4` OPTIONS(object_metadata = 'SIMPLE', uris = ['%s'])", ds, uri),
		"two wildcards": fmt.Sprintf("CREATE EXTERNAL TABLE `%s.bad5` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/*/*'])",
			ds, conn, bh.BucketName()),
		"DML": fmt.Sprintf("INSERT INTO `%s.live` (uri) VALUES ('gs://x/y')", ds),
	} {
		if err := mlRun(h, c, sql); err == nil || !strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := mlRun(h, c, create); err == nil || !strings.Contains(err.Error(), "Already Exists") {
		t.Errorf("a second CREATE: %v", err)
	}
	if err := mlRun(h, c, strings.Replace(create, "CREATE EXTERNAL", "CREATE OR REPLACE EXTERNAL", 1)); err != nil {
		t.Errorf("CREATE OR REPLACE: %v", err)
	}
}
