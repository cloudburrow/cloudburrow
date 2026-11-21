//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
)

// TestBigQueryIcebergDefaultConnection (#1456): WITH CONNECTION DEFAULT of
// a BigQuery table for Apache Iceberg takes the project's default
// connection, `region-REGION.default_cloud_resource_connection_id` set by
// ALTER PROJECT SET OPTIONS ("Only specify the connection's ID"), and with
// none the one BigQuery makes, "Name: __default_cloudresource_connection__",
// in the dataset's region (Default connection overview,
// https://docs.cloud.google.com/bigquery/docs/default-connections, read
// 2026-10-17), read back as biglakeConfiguration.connectionId; the
// table's files are written under its storage_uri. Before, the
// connectionId read "DEFAULT". storage_uri with no WITH CONNECTION at all
// is still kept only (TestBigQueryStorageURIMakesIcebergTable): the DDL
// reference's managed table takes WITH CONNECTION {CONNECTION_NAME |
// DEFAULT}, and Google documents no connection for one without it.
//
// Since #2104 the default connection is made, as BigQuery makes it: a
// caller without bigquery.connections.create is refused 403, nothing
// made; one with BigQuery Connection Admin and Storage Admin has the
// connection made (CLOUD_RESOURCE, read back through the official
// Connection API client) and its service account granted
// roles/storage.legacyBucketWriter and roles/storage.legacyObjectOwner on
// the storage_uri's bucket ("Apache Iceberg managed tables", the default
// connections page, read 2026-11-21), read back as the bucket's policy.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigqueryconnection.projects.locations.connections.get
func TestBigQueryIcebergDefaultConnection(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-icedefconn")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := bucket.Objects(context.Background(), nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = bucket.Object(o.Name).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	cc, _ := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	made := "projects/" + project + "/locations/us/connections/__default_cloudresource_connection__"
	lake := "projects/" + project + "/locations/us/connections/lake"
	for _, n := range []string{made, lake} {
		if _, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: n}); err == nil {
			t.Skipf("the project already has the connection %s; this test makes it", n)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{made, lake} {
			_ = cc.DeleteConnection(context.Background(), &connectionpb.DeleteConnectionRequest{Name: n})
		}
	})
	uri := func(p string) string { return "gs://" + bucket.BucketName() + "/" + p }
	name := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("insert=%v: %s: %v", insert, sql, err)
		}
	}
	conn := func(table string) string {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil || md.BigLakeConfiguration == nil {
			t.Fatalf("tables.get of %s: %+v, %v", table, md, err)
		}
		return md.BigLakeConfiguration.ConnectionID
	}

	createMade := "CREATE TABLE " + name("made") + " (i INT64, s STRING) WITH CONNECTION DEFAULT OPTIONS (storage_uri = '" + uri("made") + "')"
	if err := bqRun(ctx, c, createMade, false); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "bigquery.connections.create") {
		t.Fatalf("no permission to make the default connection: %v", err)
	}
	if tableExists(t, ctx, ds.Table("made")) {
		t.Fatalf("the refused CREATE made the table")
	}
	grantProject(t, h, pc, project, "roles/bigquery.connectionAdmin", callerMember(project))
	grantProject(t, h, pc, project, "roles/storage.admin", callerMember(project))
	run(createMade, false)
	cn, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: made})
	if err != nil || cn.GetCloudResource().GetServiceAccountId() == "" {
		t.Fatalf("the default connection was not made: %v, %v", cn, err)
	}
	pol, err := bucket.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("the bucket's policy: %v", err)
	}
	sa := "serviceAccount:" + cn.GetCloudResource().GetServiceAccountId()
	for _, role := range []string{"roles/storage.legacyBucketWriter", "roles/storage.legacyObjectOwner"} {
		if !pol.HasRole(sa, iam.RoleName(role)) {
			t.Errorf("the default connection's service account was not granted %s on the bucket: %v", role, pol.Roles())
		}
	}
	if got, want := conn("made"), project+".us.__default_cloudresource_connection__"; got != want {
		t.Errorf("WITH CONNECTION DEFAULT, no default set: connection %q, want %q", got, want)
	}
	alter := "ALTER PROJECT `" + project + "` SET OPTIONS (`region-us.default_cloud_resource_connection_id` = "
	run(alter+"'lake')", false)
	t.Cleanup(func() { _ = bqRun(context.Background(), c, alter+"NULL)", false) })
	run("CREATE TABLE "+name("set")+" WITH CONNECTION DEFAULT OPTIONS (storage_uri = '"+uri("set")+"') AS SELECT 1 AS i, 'a' AS s", true)
	if got, want := conn("set"), project+".us.lake"; got != want {
		t.Errorf("WITH CONNECTION DEFAULT, default lake: connection %q, want %q", got, want)
	}
	if _, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: lake}); err != nil {
		t.Errorf("the project's default connection lake was not made: %v", err)
	}
	run("INSERT "+name("made")+" (i, s) VALUES (1, 'x')", true)
	for _, tbl := range []string{"made", "set"} {
		run("EXPORT TABLE METADATA FROM "+name(tbl), true)
	}
	root := t.TempDir()
	download(t, ctx, bucket, root)
	for tbl, want := range map[string]string{"made": "1|x", "set": "1|a"} {
		if got, _ := readIceberg(t, root, bucket.BucketName(), tbl); got != want {
			t.Errorf("%s's Iceberg rows:\n%s\nwant\n%s", tbl, got, want)
		}
	}
}
