//go:build compat

package compat

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryDMLFromSeveralItems (#1459): DML whose FROM has several
// items, as BigQuery's DML reference documents
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/dml-syntax):
// UPDATE ... FROM a JOIN, a comma and UNNEST ... WITH OFFSET; a DELETE
// whose WHERE has a subquery of a JOIN and a correlated one of a comma;
// a MERGE from a subquery of a JOIN. Measured first through the front:
// the UPDATE was 501 "UPDATE ... FROM with a FROM clause of more than one
// table or subquery", and the DELETE failed 400 "sqlite3: SQL logic error:
// ambiguous column name: k" (both tables have a column k).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.tabledata.list
func TestBigQueryDMLFromSeveralItems(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "_from")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	for _, sql := range []string{
		"CREATE TABLE t (id INT64, v STRING)",
		"CREATE TABLE u (id INT64, k INT64)",
		"CREATE TABLE w (k INT64, v STRING)",
		"INSERT t VALUES (1, 'a'), (2, 'b'), (3, 'c')",
		"INSERT u VALUES (1, 10), (2, 20)",
		"INSERT w VALUES (10, 'X'), (20, 'Y')",
	} {
		if err := runIn(ctx, c, project, ds.DatasetID, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	run := func(sql string, wantType string, want int64) {
		t.Helper()
		q := c.Query(sql)
		q.DefaultProjectID, q.DefaultDatasetID = project, ds.DatasetID
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		st, err := job.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		if qs == nil || qs.StatementType != wantType || qs.NumDMLAffectedRows != want {
			t.Errorf("%s: %+v, want %s of %d rows", sql, qs, wantType, want)
		}
	}
	rows := func(want string) {
		t.Helper()
		got := queryRows(t, ctx, c, project, ds.DatasetID, "SELECT STRING_AGG(CONCAT(CAST(id AS STRING), '=', IFNULL(v, '-')), ',' ORDER BY id) FROM t")
		if !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("t is %v, want %s", got, want)
		}
	}

	run("UPDATE t SET v = w.v FROM u JOIN w USING (k) WHERE t.id = u.id", "UPDATE", 2)
	rows("1=X,2=Y,3=c")
	run("UPDATE t x SET v = CONCAT(x.v, CAST(o AS STRING), CAST(n AS STRING)) FROM u, UNNEST([7]) n WITH OFFSET o WHERE x.id = u.id AND u.k = 20",
		"UPDATE", 1)
	rows("1=X,2=Y07,3=c")
	run("UPDATE t SET v = 'joined' FROM u AS a, w AS b WHERE t.id = a.id AND a.k = b.k AND b.v = 'X'", "UPDATE", 1)
	rows("1=joined,2=Y07,3=c")

	// The subqueries' tables have aliases: a table named without one in a
	// subquery loses its name to the front's qualification (#1509).
	run("DELETE t WHERE id IN (SELECT u.id FROM u AS u JOIN w AS w ON u.k = w.k WHERE w.v = 'X')", "DELETE", 1)
	rows("2=Y07,3=c")
	run("DELETE FROM t AS x WHERE EXISTS (SELECT 1 FROM u AS a, w AS b WHERE a.k = b.k AND a.id = x.id)", "DELETE", 1)
	rows("3=c")

	run("MERGE t AS t USING (SELECT u.id, w.v FROM u AS u JOIN w AS w ON u.k = w.k) s ON t.id = s.id "+
		"WHEN NOT MATCHED THEN INSERT (id, v) VALUES (s.id, s.v)", "MERGE", 2)
	rows("1=X,2=Y,3=c")

	// A FROM the front does not read stays 501, and changes nothing.
	err := runIn(ctx, c, project, ds.DatasetID, "UPDATE t SET v = 'q' FROM (u JOIN w USING (k)) WHERE t.id = u.id", false)
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusNotImplemented {
		t.Errorf("UPDATE ... FROM a parenthesized join: %v, want 501", err)
	}
	rows("1=X,2=Y,3=c")
}

// TestBigQueryCreateSchemaOptionsAndCollation (#1459): CREATE SCHEMA with
// each option of schema_option_list and DEFAULT COLLATE
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_schema_statement)
// makes the dataset with them, and datasets.get returns each; a new table
// in it, by CREATE TABLE or tables.insert, takes its default collation,
// and so do its STRING columns. A value BigQuery refuses is 400; the
// options of cross-region replication stay 501. Measured first through
// the front: 501 for every option but description, friendly_name, labels
// and location, and for DEFAULT COLLATE; a table made by tables.insert in
// a dataset with a defaultCollation had none, nor its columns.
//
// covers: bigquery.jobs.query, bigquery.datasets.get, bigquery.tables.insert, bigquery.tables.get
func TestBigQueryCreateSchemaOptionsAndCollation(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_opts"
	t.Cleanup(func() { _ = c.Dataset(id).DeleteWithContents(h.Context()) })
	key := "projects/" + project + "/locations/us/keyRings/r/cryptoKeys/k"
	sql := "CREATE SCHEMA " + id + " DEFAULT COLLATE 'und:ci' OPTIONS(default_kms_key_name='" + key + "', " +
		"default_partition_expiration_days=0.5, default_rounding_mode='ROUND_HALF_EVEN', default_table_expiration_days=3.75, " +
		"is_case_insensitive=TRUE, max_time_travel_hours=72, storage_billing_model='PHYSICAL', " +
		"tags=[('" + project + "/env', 'prod')], description='opts', location='us')"
	if err := bqRun(ctx, c, sql, false); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	md, err := c.Dataset(id).Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.DefaultCollation != "und:ci" || md.DefaultPartitionExpiration != 12*time.Hour ||
		md.DefaultTableExpiration != 90*time.Hour || !md.IsCaseInsensitive || md.MaxTimeTravel != 72*time.Hour ||
		md.StorageBillingModel != "PHYSICAL" || md.DefaultEncryptionConfig == nil ||
		md.DefaultEncryptionConfig.KMSKeyName != key || md.Description != "opts" {
		t.Errorf("Dataset.Metadata = %+v", md)
	}
	raw, err := svc.Datasets.Get(project, id).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if raw.DefaultRoundingMode != "ROUND_HALF_EVEN" || raw.ResourceTags[project+"/env"] != "prod" {
		t.Errorf("datasets.get: defaultRoundingMode %q, resourceTags %v", raw.DefaultRoundingMode, raw.ResourceTags)
	}

	// New tables take the collation: by CREATE TABLE and by tables.insert.
	if err := runIn(ctx, c, project, id, "CREATE TABLE ddl (s STRING, n INT64, r STRUCT<x STRING>)", false); err != nil {
		t.Fatal(err)
	}
	if err := c.Dataset(id).Table("api").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "s", Type: bigquery.StringFieldType}, {Name: "n", Type: bigquery.IntegerFieldType},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ddl", "api"} {
		tm, err := c.Dataset(id).Table(name).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tm.DefaultCollation != "und:ci" || tm.Schema[0].Collation != "und:ci" || tm.Schema[1].Collation != "" {
			t.Errorf("%s: defaultCollation %q, columns %q %q", name, tm.DefaultCollation, tm.Schema[0].Collation, tm.Schema[1].Collation)
		}
		if name == "ddl" && tm.Schema[2].Schema[0].Collation != "und:ci" {
			t.Errorf("ddl: r.x collation %q", tm.Schema[2].Schema[0].Collation)
		}
	}
	got := queryRows(t, ctx, c, project, id, "SELECT STRING_AGG(CONCAT(table_name, '.', column_name, ':', IFNULL(collation_name, '-')), ',' "+
		"ORDER BY table_name, column_name) FROM `"+id+"`.INFORMATION_SCHEMA.COLUMNS")
	if want := []string{"api.n:-,api.s:und:ci,ddl.n:-,ddl.r:-,ddl.s:und:ci"}; !reflect.DeepEqual(got, want) {
		t.Errorf("COLUMNS collation_name: %v, want %v", got, want)
	}

	var gerr *googleapi.Error
	for _, c2 := range []struct {
		sql  string
		code int
	}{
		{"CREATE SCHEMA " + id + "_bad OPTIONS(max_time_travel_hours=50)", http.StatusBadRequest},
		{"CREATE SCHEMA " + id + "_bad DEFAULT COLLATE 'und:cs'", http.StatusBadRequest},
		{"CREATE SCHEMA " + id + "_bad OPTIONS(primary_replica='us-east4')", http.StatusNotImplemented},
	} {
		err := bqRun(ctx, c, c2.sql, false)
		if !errors.As(err, &gerr) || gerr.Code != c2.code {
			t.Errorf("%s: %v, want %d", c2.sql, err, c2.code)
		}
		if _, err := c.Dataset(id + "_bad").Metadata(ctx); err == nil {
			t.Errorf("%s made the dataset", c2.sql)
		}
	}
}
