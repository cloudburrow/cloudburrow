//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// httpStatus is err's HTTP status, or 0.
func httpStatus(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// TestBigQueryDDLMoreOptionsAndWholeAlter (#1456): the table option
// enable_fine_grained_mutations (BOOL, "to enable fine-grained DML
// optimization on the table"), set by CREATE TABLE and ALTER TABLE SET
// OPTIONS, reads back in INFORMATION_SCHEMA.TABLE_OPTIONS and NULL clears
// it; a column's data_policies, by CREATE TABLE and by ALTER COLUMN SET OPTIONS (ARRAY<STRING> of
// "{'name':'project.region-us.policy'}", with = and +=) read back as the
// field's dataPolicies through tables.get, NULL clearing them, and a name
// not of that form is 400 (data_governance_tags: TestBigQueryDataGovernanceTags). And an ALTER
// TABLE of several actions that go different ways (ADD COLUMN through
// tables.patch, SET DATA TYPE and DROP COLUMN by making the table again)
// whose later action is refused leaves the table as it was, as BigQuery's
// statement is carried out whole or not at all (before, the earlier
// actions were done). Each through jobs.query and jobs.insert. Before,
// enable_fine_grained_mutations and data_policies were 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDDLMoreOptionsAndWholeAlter(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		name := fmt.Sprintf("more%v", insert)
		ref := "`" + ds.DatasetID + "." + name + "`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		option := func() string {
			t.Helper()
			got := rows(t, ctx, c, "SELECT option_value FROM "+ds.DatasetID+".INFORMATION_SCHEMA.TABLE_OPTIONS "+
				"WHERE table_name = '"+name+"' AND option_name = 'enable_fine_grained_mutations'")
			return fmt.Sprint(got)
		}
		run("CREATE TABLE " + ref + " (a STRING OPTIONS (data_policies = [\"{'name':'" + project + ".region-us.p0'}\"]), b INT64, n INT64) " +
			"OPTIONS (enable_fine_grained_mutations = TRUE)")
		if got := option(); got != "[[true]]" {
			t.Errorf("insert %v: after CREATE TABLE: enable_fine_grained_mutations %s, want [[true]]", insert, got)
		}
		run("ALTER TABLE " + ref + " SET OPTIONS (enable_fine_grained_mutations = FALSE)")
		if got := option(); got != "[[false]]" {
			t.Errorf("insert %v: after SET OPTIONS FALSE: %s, want [[false]]", insert, got)
		}
		run("ALTER TABLE " + ref + " SET OPTIONS (enable_fine_grained_mutations = NULL)")
		if got := option(); got != "[]" {
			t.Errorf("insert %v: after SET OPTIONS NULL: %s, want no row", insert, got)
		}

		policies := func() string {
			t.Helper()
			full, err := svc.Tables.Get(project, ds.DatasetID, name).Context(ctx).Do()
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, p := range full.Schema.Fields[0].DataPolicies {
				out = append(out, p.Name)
			}
			return strings.Join(out, ",")
		}
		pol := func(id string) string { return "projects/" + project + "/locations/us/dataPolicies/" + id }
		if got := policies(); got != pol("p0") {
			t.Errorf("insert %v: CREATE TABLE with data_policies = [p0]: %q, want %q", insert, got, pol("p0"))
		}
		run("ALTER TABLE " + ref + " ALTER COLUMN a SET OPTIONS (data_policies = [\"{'name':'" + project + ".region-us.p1'}\"])")
		if got := policies(); got != pol("p1") {
			t.Errorf("insert %v: data_policies = [p1]: %q, want %q", insert, got, pol("p1"))
		}
		run("ALTER TABLE " + ref + " ALTER COLUMN a SET OPTIONS (data_policies += [\"{'name':'" + project + ".region-us.p2'}\"])")
		if got, want := policies(), pol("p1")+","+pol("p2"); got != want {
			t.Errorf("insert %v: data_policies += [p2]: %q, want %q", insert, got, want)
		}
		run("ALTER TABLE " + ref + " ALTER COLUMN a SET OPTIONS (data_policies = NULL)")
		if got := policies(); got != "" {
			t.Errorf("insert %v: data_policies = NULL: %q, want none", insert, got)
		}
		if err := bqRun(ctx, c, "ALTER TABLE "+ref+" ALTER COLUMN a SET OPTIONS (data_policies = ['p1'])", insert); httpStatus(err) != http.StatusBadRequest {
			t.Errorf("insert %v: data_policies of a bare name: %v, want 400", insert, err)
		}

		// Whole or nothing: the refused SET DATA TYPE (STRING is not
		// assignable to INT64) leaves the ADD COLUMN before it undone, and
		// a missing column in a later ALTER COLUMN leaves the DROP COLUMN
		// before it undone.
		run("INSERT INTO " + ref + " (a, b, n) VALUES ('x', 1, 2)")
		for _, sql := range []string{
			"ALTER TABLE " + ref + " ADD COLUMN c INT64, ALTER COLUMN a SET DATA TYPE INT64",
			"ALTER TABLE " + ref + " DROP COLUMN n, ALTER COLUMN missing SET OPTIONS (description = 'd')",
			"ALTER TABLE " + ref + " SET OPTIONS (description = 'd'), ALTER COLUMN b SET DATA TYPE STRING",
		} {
			if err := bqRun(ctx, c, sql, insert); httpStatus(err) != http.StatusBadRequest {
				t.Errorf("insert %v: %s: %v, want 400", insert, sql, err)
			}
		}
		md, err := ds.Table(name).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var cols []string
		for _, f := range md.Schema {
			cols = append(cols, f.Name+" "+string(f.Type))
		}
		if got, want := strings.Join(cols, ", "), "a STRING, b INTEGER, n INTEGER"; got != want || md.Description != "" {
			t.Errorf("insert %v: after the refused statements: %s, description %q; want %s and none", insert, got, md.Description, want)
		}
		if got := fmt.Sprint(rows(t, ctx, c, "SELECT a, b, n FROM "+ref)); got != "[[x 1 2]]" {
			t.Errorf("insert %v: rows %s, want [[x 1 2]]", insert, got)
		}
		// The same actions, none refused, are all carried out.
		run("ALTER TABLE " + ref + " ADD COLUMN c INT64, ALTER COLUMN b SET DATA TYPE NUMERIC, DROP COLUMN n")
		md, err = ds.Table(name).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cols = cols[:0]
		for _, f := range md.Schema {
			cols = append(cols, f.Name+" "+string(f.Type))
		}
		if got, want := strings.Join(cols, ", "), "a STRING, b NUMERIC, c INTEGER"; got != want {
			t.Errorf("insert %v: after the statement: %s, want %s", insert, got, want)
		}
	}
}

// TestBigQueryRangeInARecordReadsBack (#1456): a RANGE<TIMESTAMP> and a
// RANGE<DATETIME> field of a RECORD, and one in a REPEATED RECORD inside
// it, read back in BigQuery's REST form, a TIMESTAMP bound as int64
// microseconds and a DATETIME one with a "T", the forms a top-level RANGE
// has (#1111, #1492), through the generated client's jobs.query and
// tabledata.list. The official Go client does not read a RANGE inside a
// RECORD at all (convertRow: "we don't propagate range element type more
// deeply"), so the cells are read as the REST API gives them. Before, such
// a value was in GoogleSQL's text, a TIMESTAMP bound
// "2020-01-01 00:00:00+00".
//
// covers: bigquery.jobs.query, bigquery.tabledata.list
func TestBigQueryRangeInARecordReadsBack(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	ref := "`" + ds.DatasetID + ".rec`"
	if err := bqRun(ctx, c, "CREATE TABLE "+ref+" (id INT64, st STRUCT<ts RANGE<TIMESTAMP>, dt RANGE<DATETIME>, "+
		"nest ARRAY<STRUCT<r RANGE<TIMESTAMP>>>>)", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ref+" (id, st) VALUES (1, STRUCT(RANGE(TIMESTAMP '2020-01-01 00:00:00+00', NULL), "+
		"RANGE(DATETIME '2020-01-01 10:00:00', DATETIME '2020-01-02 00:00:00'), "+
		"[STRUCT(RANGE(TIMESTAMP '2021-06-01 12:30:00+00', TIMESTAMP '2021-06-02 00:00:00+00'))]))", false); err != nil {
		t.Fatal(err)
	}
	ts := func(y, m, d, hh, mm int) string {
		return fmt.Sprint(time.Date(y, time.Month(m), d, hh, mm, 0, 0, time.UTC).UnixMicro())
	}
	want := []string{"[" + ts(2020, 1, 1, 0, 0) + ", UNBOUNDED)", "[2020-01-01T10:00:00, 2020-01-02T00:00:00)",
		"[" + ts(2021, 6, 1, 12, 30) + ", " + ts(2021, 6, 2, 0, 0) + ")"}
	// cells reads a REST RECORD value, {"f": [{"v": ...}, ...]}.
	cells := func(v any) []any {
		m, _ := v.(map[string]any)
		f, _ := m["f"].([]any)
		out := make([]any, len(f))
		for i, c := range f {
			cm, _ := c.(map[string]any)
			out[i] = cm["v"]
		}
		return out
	}
	check := func(how string, st any) {
		t.Helper()
		f := cells(st)
		if len(f) != 3 {
			t.Fatalf("%s: st %#v", how, st)
		}
		nest, _ := f[2].([]any)
		if len(nest) != 1 {
			t.Fatalf("%s: nest %#v", how, f[2])
		}
		el, _ := nest[0].(map[string]any)
		r := cells(el["v"])
		if len(r) != 1 {
			t.Fatalf("%s: nest[0] %#v", how, nest[0])
		}
		if got := []any{f[0], f[1], r[0]}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", how, got, want)
		}
	}
	res, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: "SELECT id, st FROM " + ref, UseLegacySql: new(bool)}).Context(ctx).Do()
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("jobs.query: %+v (%v)", res, err)
	}
	check("jobs.query", res.Rows[0].F[1].V)
	list, err := svc.Tabledata.List(project, ds.DatasetID, "rec").Context(ctx).Do()
	if err != nil || len(list.Rows) != 1 {
		t.Fatalf("tabledata.list: %+v (%v)", list, err)
	}
	check("tabledata.list", list.Rows[0].F[1].V)
}

// TestBigQueryDefaultKeyOfJobTables (#1456): a project's
// default_kms_key_name is the key of the destination table a query job
// or a copy job makes, given no destinationEncryptionConfiguration, as of
// the tables tables.insert and CREATE TABLE make: "the default Cloud KMS
// key for encrypting table data"
// (https://cloud.google.com/bigquery/docs/default-configuration). A job
// given its own key keeps it, and a destination table that exists is left
// as it is. Before, such a table had no key. The settings are in
// me-central2, which no other test uses.
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDefaultKeyOfJobTables(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	const region = "me-central2"
	const r = "`region-" + region + "."
	run := func(sql string) {
		t.Helper()
		q := c.Query(sql)
		q.Location = region
		if _, err := q.Read(ctx); err != nil {
			t.Fatalf("%s: %v", sql, err)
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
	ds := c.Dataset(strings.ReplaceAll(project, "-", "_") + "_jobkeys")
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
	run("CREATE TABLE " + ds.DatasetID + ".existing (a INT64)")
	run("ALTER PROJECT SET OPTIONS (" + r + "default_kms_key_name` = '" + key + "')")
	wait := func(what string, job *bigquery.Job, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		st, err := job.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
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
	query := func(dest string, ownKey bool) {
		t.Helper()
		q := c.Query("SELECT 1 AS a")
		q.Location, q.Dst = region, ds.Table(dest)
		if ownKey {
			q.DestinationEncryptionConfig = &bigquery.EncryptionConfig{KMSKeyName: own}
		}
		job, err := q.Run(ctx)
		wait("query into "+dest, job, err)
	}
	query("fromquery", false)
	if got := keyOf("fromquery"); got != key {
		t.Errorf("a query job's new destination table: key %q, want %q", got, key)
	}
	query("ownkey", true)
	if got := keyOf("ownkey"); got == key {
		t.Errorf("a query job given its own key: key %q, want not the default", got)
	}
	cp := ds.Table("copied").CopierFrom(ds.Table("fromquery"))
	cp.Location = region
	job, err := cp.Run(ctx)
	wait("copy", job, err)
	if got := keyOf("copied"); got != key {
		t.Errorf("a copy job's new destination table: key %q, want %q", got, key)
	}
	q := c.Query("SELECT 2 AS a")
	q.Location, q.Dst, q.WriteDisposition = region, ds.Table("existing"), bigquery.WriteAppend
	job, err = q.Run(ctx)
	wait("query into existing", job, err)
	if got := keyOf("existing"); got != "" {
		t.Errorf("a destination table that existed: key %q, want none", got)
	}
}

// TestBigQueryDefaultTimeZoneOfCoercedStrings (#1456): a project's
// default_time_zone reads a string literal compared with a TIMESTAMP
// column, which the analyzer coerces to a TIMESTAMP, in that zone, as it
// does a TIMESTAMP literal with no zone ("if no time zone is specified,
// the default time zone is used"); a string with a zone keeps it, and a
// job in another region reads UTC. Before, the coerced string was read
// in UTC. The setting is in africa-south1, which no other test uses.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryDefaultTimeZoneOfCoercedStrings(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	const region = "africa-south1"
	const r = "`region-" + region + "."
	count := func(sql string, insert bool, loc string) int64 {
		t.Helper()
		q := c.Query(sql)
		q.Location = loc
		var it *bigquery.RowIterator
		var err error
		if insert {
			job, e := q.Run(ctx)
			if e != nil {
				t.Fatalf("%s: %v", sql, e)
			}
			it, err = job.Read(ctx)
		} else {
			it, err = q.Read(ctx)
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		n, _ := row[0].(int64)
		return n
	}
	t.Cleanup(func() {
		q := c.Query("ALTER PROJECT SET OPTIONS (" + r + "default_time_zone` = NULL)")
		q.Location = region
		_, _ = q.Read(ctx)
	})
	ds := c.Dataset(strings.ReplaceAll(project, "-", "_") + "_coercedzone")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: region}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	ref := "`" + ds.DatasetID + ".t`"
	for _, sql := range []string{
		"CREATE TABLE " + ref + " (ts TIMESTAMP)",
		"INSERT INTO " + ref + " (ts) VALUES (TIMESTAMP '2020-01-01 03:00:00+00')",
		"ALTER PROJECT SET OPTIONS (" + r + "default_time_zone` = 'America/New_York')",
	} {
		q := c.Query(sql)
		q.Location = region
		if _, err := q.Read(ctx); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// 2020-01-01 03:00:00 UTC is 2019-12-31 22:00:00 in New York.
	for _, insert := range []bool{false, true} {
		for where, want := range map[string]int64{
			"ts = '2019-12-31 22:00:00'":                                 1,
			"ts > '2020-01-01 00:00:00'":                                 0,
			"ts < '2020-01-01'":                                          1,
			"ts = '2020-01-01 03:00:00+00'":                              1,
			"ts = '2020-01-01 03:00:00 UTC'":                             1,
			"'2019-12-31 22:00:00' = ts":                                 1,
			"ts BETWEEN '2019-12-31 21:00:00' AND '2019-12-31 23:00:00'": 1,
		} {
			if got := count("SELECT COUNT(*) FROM "+ref+" WHERE "+where, insert, region); got != want {
				t.Errorf("insert %v: WHERE %s in America/New_York: %d rows, want %d", insert, where, got, want)
			}
		}
	}
	// Another region's job reads UTC: the same rows through a literal
	// table, as the dataset is in africa-south1.
	if got := count("SELECT COUNT(*) FROM UNNEST([TIMESTAMP '2020-01-01 03:00:00+00']) AS ts WHERE ts > '2020-01-01 00:00:00'", false, "US"); got != 1 {
		t.Errorf("a US job: %d rows, want 1 (UTC)", got)
	}
}
