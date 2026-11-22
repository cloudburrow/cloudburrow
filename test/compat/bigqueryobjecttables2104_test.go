//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
	"google.golang.org/api/iterator"
)

// TestBigQueryObjectTablesDefaultConnectionOptionsAndRowPolicies (#2104),
// through the official cloud.google.com/go/bigquery, connection/apiv1,
// storage and resourcemanager clients, to Google's object table,
// default connection and DDL pages (read 2026-11-21):
//
//   - CREATE EXTERNAL TABLE ... WITH CONNECTION DEFAULT of an object table
//     makes __default_cloudresource_connection__ and grants its service
//     account roles/storage.legacyBucketReader and
//     roles/storage.legacyObjectReader on the bucket ("Cloud Storage
//     BigLake tables", "Object tables"), so the table reads its objects
//     with no other grant; a caller without bigquery.connections.create is
//     refused 403, nothing made;
//   - the external table options of no format: expiration_timestamp (the
//     table's expirationTime, read back), tags (resourceTags),
//     file_set_spec_type NEW_LINE_DELIMITED_MANIFEST ("the provided URIs
//     are newline-delimited manifest files, with one URI per line"), and
//     max_staleness as an expression; a format's option (CSV's
//     field_delimiter, format) is 400, nothing made, as is WITH PARTITION
//     COLUMNS (hive partitioning has a format);
//   - a row access policy on an object table restricts "access to the
//     object metadata in selected rows": a SELECT, and
//     EXTERNAL_OBJECT_TRANSFORM's SIGNED_URL rows, return only the
//     objects whose rows the policy grants.
//
// covers: bigquery.jobs.query, bigquery.tables.get, bigquery.tables.insert, bigqueryconnection.projects.locations.connections.get
func TestBigQueryObjectTablesDefaultConnectionOptionsAndRowPolicies(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	made := "projects/" + project + "/locations/us/connections/__default_cloudresource_connection__"
	if _, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: made}); err == nil {
		t.Skip("the project already has a default connection in the US; this test makes it")
	}
	t.Cleanup(func() {
		_ = cc.DeleteConnection(context.Background(), &connectionpb.DeleteConnectionRequest{Name: made})
	})

	ds := "compat_obj2104_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(context.Background()) })
	bh := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-obj2104")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	put := func(name, ct, data string) {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType = ct
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	put("docs/a.txt", "text/plain", "alpha")
	put("docs/b.png", "image/png", "\x89PNG....")
	put("docs/c.txt", "text/plain", "gamma")
	uri := "gs://" + bh.BucketName() + "/docs/*"
	uris := func(sql string) []string {
		t.Helper()
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var out []string
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			out = append(out, fmt.Sprint(row[0]))
		}
	}

	// WITH CONNECTION DEFAULT: refused without the permission to make it.
	expires := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	create := fmt.Sprintf("CREATE EXTERNAL TABLE `%s.objs` WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'SIMPLE', uris = ['%s'], "+
		"expiration_timestamp = TIMESTAMP '%s', tags = [('%s/env', 'dev')], max_staleness = INTERVAL 30 * 2 MINUTE, metadata_cache_mode = 'MANUAL')",
		ds, uri, expires.Format("2006-01-02 15:04:05 UTC"), project)
	if err := mlRun(h, c, create); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "bigquery.connections.create") {
		t.Fatalf("no permission to make the default connection: %v", err)
	}
	if tableExists(t, ctx, c.Dataset(ds).Table("objs")) {
		t.Fatal("the refused CREATE made the table")
	}
	grantProject(t, h, pc, project, "roles/bigquery.connectionAdmin", callerMember(project))
	grantProject(t, h, pc, project, "roles/storage.admin", callerMember(project))
	if err := mlRun(h, c, create); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE WITH CONNECTION DEFAULT: %v", err)
	}
	cn, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: made})
	if err != nil {
		t.Fatalf("the default connection was not made: %v", err)
	}
	sa := "serviceAccount:" + cn.GetCloudResource().GetServiceAccountId()
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("the bucket's policy: %v", err)
	}
	for _, role := range []string{"roles/storage.legacyBucketReader", "roles/storage.legacyObjectReader"} {
		if !pol.HasRole(sa, iam.RoleName(role)) {
			t.Errorf("the default connection's service account was not granted %s: %v", role, pol.Roles())
		}
	}
	md, err := c.Dataset(ds).Table("objs").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !md.ExpirationTime.Equal(expires) || md.MaxStaleness == nil || md.MaxStaleness.Hours != 1 && md.MaxStaleness.Minutes != 60 ||
		md.ExternalDataConfig == nil || md.ExternalDataConfig.ConnectionID != project+".us.__default_cloudresource_connection__" {
		t.Errorf("tables.get: expiration %v (want %v), max_staleness %v, external %+v", md.ExpirationTime, expires, md.MaxStaleness, md.ExternalDataConfig)
	}
	if md.ResourceTags[project+"/env"] != "dev" {
		t.Errorf("tags: %v", md.ResourceTags)
	}
	all := uris(fmt.Sprintf("SELECT uri FROM `%s.objs` ORDER BY uri", ds))
	if len(all) != 3 {
		t.Fatalf("rows read through the default connection: %v", all)
	}

	// A row access policy: the caller sees text objects only.
	if err := mlRun(h, c, fmt.Sprintf("CREATE ROW ACCESS POLICY text_only ON `%s.objs` GRANT TO ('%s') FILTER USING (content_type = 'text/plain')",
		ds, callerMember(project))); err != nil {
		t.Fatalf("CREATE ROW ACCESS POLICY on an object table: %v", err)
	}
	if got := uris(fmt.Sprintf("SELECT uri FROM `%s.objs` ORDER BY uri", ds)); strings.Join(got, ",") != all[0]+","+all[2] {
		t.Errorf("SELECT under the policy: %v, want the two text objects of %v", got, all)
	}
	if got := uris(fmt.Sprintf("SELECT uri FROM EXTERNAL_OBJECT_TRANSFORM(TABLE `%s.objs`, ['SIGNED_URL']) ORDER BY uri", ds)); len(got) != 2 {
		t.Errorf("SIGNED_URL rows under the policy: %v, want the two text objects", got)
	}
	if err := mlRun(h, c, fmt.Sprintf("DROP ALL ROW ACCESS POLICIES ON `%s.objs`", ds)); err != nil {
		t.Fatalf("DROP ALL ROW ACCESS POLICIES: %v", err)
	}
	if got := uris(fmt.Sprintf("SELECT uri FROM `%s.objs`", ds)); len(got) != 3 {
		t.Errorf("rows after the policies were dropped: %v", got)
	}

	// A manifest of two of the objects.
	put("lists/manifest.txt", "text/plain", "gs://"+bh.BucketName()+"/docs/a.txt\n\ngs://"+bh.BucketName()+"/docs/b.png\n")
	if err := mlRun(h, c, fmt.Sprintf("CREATE EXTERNAL TABLE `%s.listed` WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'SIMPLE', "+
		"uris = ['gs://%s/lists/manifest.txt'], file_set_spec_type = 'NEW_LINE_DELIMITED_MANIFEST')", ds, bh.BucketName())); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE of a manifest: %v", err)
	}
	if got := uris(fmt.Sprintf("SELECT uri FROM `%s.listed` ORDER BY uri", ds)); strings.Join(got, ",") != all[0]+","+all[1] {
		t.Errorf("the manifest's objects: %v", got)
	}
	if md, err := c.Dataset(ds).Table("listed").Metadata(ctx); err != nil || md.ExternalDataConfig == nil ||
		md.ExternalDataConfig.FileSetSpecType != bigquery.FileSetSpecTypeNewLineDelimitedManifest {
		t.Errorf("tables.get of the manifest table: %+v, %v", md, err)
	}

	// Options of a format, and hive partitioning: 400, nothing made.
	for what, opt := range map[string]string{"field_delimiter": "field_delimiter = ','", "format": "format = 'CSV'"} {
		err := mlRun(h, c, fmt.Sprintf("CREATE EXTERNAL TABLE `%s.bad` WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'SIMPLE', uris = ['%s'], %s)", ds, uri, opt))
		if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), what) {
			t.Errorf("%s: %v, want 400 naming it", what, err)
		}
	}
	err = mlRun(h, c, fmt.Sprintf("CREATE EXTERNAL TABLE `%s.bad` WITH CONNECTION DEFAULT WITH PARTITION COLUMNS (d DATE) "+
		"OPTIONS(object_metadata = 'SIMPLE', uris = ['%s'])", ds, uri))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("WITH PARTITION COLUMNS: %v, want 400", err)
	}
	if tableExists(t, ctx, c.Dataset(ds).Table("bad")) {
		t.Error("a refused CREATE made the table")
	}
}
