//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryDefaultKeyOfScriptAndUploadTables (#1456): a project's
// default_kms_key_name is "the default Cloud KMS key for encrypting table
// data" (https://cloud.google.com/bigquery/docs/default-configuration),
// whatever makes the table, so a table a script makes (in its turn, inside
// an IF, beside a TEMP table) and the destination table of a load of
// uploaded data (jobs.insert's media upload) take it too, as a lone CREATE
// TABLE and a query, copy or JSON load job's new table did. Measured
// first: each of these tables had no key. A table the script names with
// its own kms_key_name keeps that key, and a dataset with its own
// defaultEncryptionConfiguration gives its own. The settings are in
// asia-southeast2, which no other test uses.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDefaultKeyOfScriptAndUploadTables(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	const region = "asia-southeast2"
	const r = "`region-" + region + "."
	run := func(sql string, insert bool) {
		t.Helper()
		q := c.Query(sql)
		q.Location = region
		var err error
		if insert {
			var job *bigquery.Job
			if job, err = q.Run(ctx); err == nil {
				var st *bigquery.JobStatus
				if st, err = job.Wait(ctx); err == nil {
					err = st.Err()
				}
			}
		} else {
			_, err = q.Read(ctx)
		}
		if err != nil {
			t.Fatalf("insert=%v: %s: %v", insert, sql, err)
		}
	}
	t.Cleanup(func() {
		// Not ctx: h.Context's 60 seconds may be spent by the time the
		// cleanups run, and a key left set reaches every later test's
		// tables.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		q := c.Query("ALTER PROJECT SET OPTIONS (" + r + "default_kms_key_name` = NULL)")
		q.Location = region
		if _, err := q.Read(cctx); err != nil {
			t.Errorf("clearing the project's default_kms_key_name: %v", err)
		}
	})
	ds := c.Dataset(strings.ReplaceAll(project, "-", "_") + "_scriptkeys")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: region}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ds.DeleteWithContents(cctx)
	})
	key := "projects/" + project + "/locations/" + region + "/keyRings/r/cryptoKeys/k"
	own := "projects/" + project + "/locations/" + region + "/keyRings/r/cryptoKeys/own"
	run("ALTER PROJECT SET OPTIONS ("+r+"default_kms_key_name` = '"+key+"')", false)
	keyOf := func(table string) string {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if md.EncryptionConfig == nil {
			return ""
		}
		return md.EncryptionConfig.KMSKeyName
	}
	d := ds.DatasetID
	for _, insert := range []bool{false, true} {
		sfx := "_q"
		if insert {
			sfx = "_j"
		}
		// A script carried out a statement at a time, with control flow.
		run("DECLARE x INT64 DEFAULT 1;\nCREATE TABLE "+d+".turn"+sfx+" (a INT64);\n"+
			"IF x = 1 THEN\n  CREATE TABLE "+d+".inif"+sfx+" AS SELECT 1 AS a;\nEND IF;\n"+
			"CREATE TABLE "+d+".ownkey"+sfx+" (a INT64) OPTIONS (kms_key_name = '"+own+"');", insert)
		// A script with a TEMP table, sent whole.
		run("CREATE TEMP TABLE tmp AS SELECT 1 AS a;\nCREATE TABLE "+d+".fromtemp"+sfx+" AS SELECT * FROM tmp;\n"+
			"CREATE OR REPLACE TABLE "+d+".replaced"+sfx+" (a INT64);", insert)
		// EXECUTE IMMEDIATE, a procedure's body, LIKE and COPY.
		run("EXECUTE IMMEDIATE 'CREATE TABLE "+d+".immediate"+sfx+" (a INT64)';\n"+
			"CREATE PROCEDURE "+d+".mk"+sfx+"() BEGIN CREATE TABLE "+d+".inproc"+sfx+" (a INT64); END;\n"+
			"CALL "+d+".mk"+sfx+"();\n"+
			"CREATE TABLE "+d+".like"+sfx+" LIKE "+d+".turn"+sfx+";\n"+
			"CREATE TABLE "+d+".copy"+sfx+" COPY "+d+".turn"+sfx+";", insert)
		for _, table := range []string{"turn", "inif", "fromtemp", "replaced", "immediate", "inproc", "like", "copy"} {
			if got := keyOf(table + sfx); got != key {
				t.Errorf("insert=%v: %s, made by a script: key %q, want %q", insert, table+sfx, got, key)
			}
		}
		if got := keyOf("ownkey" + sfx); got != own {
			t.Errorf("insert=%v: a script's table with its own kms_key_name: key %q, want %q", insert, got, own)
		}
	}

	// A load of uploaded data.
	src := bigquery.NewReaderSource(strings.NewReader("1\n2\n"))
	src.Schema = bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}
	ld := ds.Table("uploaded").LoaderFrom(src)
	ld.Location = region
	job, err := ld.Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err != nil {
		t.Fatalf("upload load: %v", err)
	}
	if got := keyOf("uploaded"); got != key {
		t.Errorf("an upload load's new destination table: key %q, want %q", got, key)
	}
	// A load into a table that exists leaves it as it is.
	run("ALTER PROJECT SET OPTIONS ("+r+"default_kms_key_name` = NULL)", false)
	run("CREATE TABLE "+d+".plain (a INT64)", false)
	run("ALTER PROJECT SET OPTIONS ("+r+"default_kms_key_name` = '"+key+"')", false)
	src = bigquery.NewReaderSource(strings.NewReader("3\n"))
	ld = ds.Table("plain").LoaderFrom(src)
	ld.Location = region
	if job, err = ld.Run(ctx); err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err != nil {
		t.Fatalf("upload load into plain: %v", err)
	}
	if got := keyOf("plain"); got != "" {
		t.Errorf("an upload load into a table that existed: key %q, want none", got)
	}
}
